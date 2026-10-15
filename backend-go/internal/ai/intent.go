package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
)

// Intent is the validated, side-effect-free interpretation of one user turn.
// A clarification may retain partial operations for the next turn, but none of
// those operations may execute until NeedsClarification is false.
type Intent struct {
	Operations         []IntentOperation `json:"operations"`
	NeedsClarification bool              `json:"needs_clarification"`
	Question           string            `json:"question"`
}

type IntentOperation struct {
	SkillID string         `json:"skill_id"`
	Slots   map[string]any `json:"slots"`
}

// IntentError exposes a safe diagnostic without returning provider output.
type IntentError struct {
	Code    string
	Message string
	Stage   string
}

func (e *IntentError) Error() string { return e.Message }

func invalidIntent() error {
	return &IntentError{Code: "AI_INTENT_INVALID", Message: "返回内容未通过格式或资源校验。请明确要执行的操作、区域和时相；已有任务可在工作区查看。"}
}

func intentOperationSummary(operation IntentOperation, workspace map[string]any) string {
	labels := map[string]string{"detect": "提交变化检测", "query": "查询影像目录", "locate": "定位区域", "models": "查询模型", "images": "查询已准备影像", "phase": "查看时相", "query_tasks": "查询检测任务", "status": "查看任务状态", "summary": "查看检测结果", "filter": "筛选变化图斑", "export": "导出检测结果"}
	label := labels[stringValue(operation.Slots["operation"])]
	if label == "" {
		label = "处理请求"
	}
	parts := []string{label}
	if path := dataStrings(operation.Slots, "admin_path"); len(path) > 0 {
		parts = append(parts, strings.Join(path, "/"))
	} else if operation.Slots["use_workspace_context"] == true {
		if name := stringValue(workspace["region_name"]); name != "" {
			parts = append(parts, name)
		}
		if operation.SkillID == "change.detect.v1" && workspace["aoi"] != nil {
			parts = append(parts, "沿用当前框选")
		}
	}
	if dates := dataStrings(operation.Slots, "phase_dates"); len(dates) > 0 {
		parts = append(parts, strings.Join(dates, " 与 "))
	} else if dates := mapValue(operation.Slots["date_range"]); len(dates) > 0 {
		parts = append(parts, stringValue(dates["start"])+" 至 "+stringValue(dates["end"]))
	}
	return strings.Join(parts, "；")
}

func (g *Gateway) parseIntent(ctx context.Context, session *Session, request MessageRequest) (Intent, error) {
	if err := ctx.Err(); err != nil {
		return Intent{}, err
	}
	// Explicit skill IDs are for controlled clients and existing integrations.
	// Natural-language requests always use the dedicated semantic parsing call.
	if request.SkillID != "" {
		if _, exists := g.skills[request.SkillID]; !exists {
			return Intent{}, invalidIntent()
		}
		slots := map[string]any{}
		if request.SkillID == "change.detect.v1" {
			slots["operation"] = "detect"
			slots["use_workspace_context"] = true
		}
		return Intent{Operations: []IntentOperation{{SkillID: request.SkillID, Slots: slots}}}, nil
	}
	if session == nil || strings.TrimSpace(request.Text) == "" {
		return Intent{}, invalidIntent()
	}
	selected, err := g.selectSkills(ctx, session, request)
	if err != nil {
		return Intent{}, err
	}
	schema := g.intentSchemaFor(selected)
	if err := validateProviderParameters(schema); err != nil {
		return Intent{}, fmt.Errorf("build intent schema: %w", err)
	}
	history := intentHistory(session, request)
	workspace := session.Workspace
	if request.Workspace != nil {
		workspace = request.Workspace
	}
	contextValue := map[string]any{
		"input": request.Text, "history": history,
		"workspace": scrubExternal(normalizeJSONValue(workspace)), "pending_intent": session.PendingIntent,
	}
	encoded, err := json.Marshal(contextValue)
	if err != nil {
		return Intent{}, invalidIntent()
	}
	g.requestMu.Lock()
	provider := g.provider
	g.requestMu.Unlock()
	var intent Intent
	err = completeStructuredIntent(ctx, provider, CompletionRequest{
		Stage: "intent",
		Messages: []ProviderMessage{
			{Role: "system", Content: g.intentPromptFor(selected)},
			{Role: "user", Content: string(encoded)},
		},
		Tools: []ProviderTool{{Type: "function", Function: ProviderFunction{
			Name: "route_intent", Description: "解析当前用户意图，返回有序操作和已知槽位；不执行任何业务或界面操作。", Parameters: schema,
		}}},
	}, func(raw string) error {
		var decodeErr error
		intent, decodeErr = g.decodeIntent(raw, schema)
		return decodeErr
	})
	if err != nil {
		return Intent{}, err
	}
	if err := ctx.Err(); err != nil {
		return Intent{}, err
	}
	// Opaque IDs must come from this session's user/final context or workspace.
	// They remain untrusted until execution revalidates the underlying resources.
	groundedText := request.Text
	for _, message := range history {
		groundedText += "\n" + message.Content
	}
	groundedResources := normalizeJSONValue(map[string]any{"workspace": workspace, "pending_intent": session.PendingIntent})
	for _, operation := range intent.Operations {
		for key, value := range operation.Slots {
			if key == "aoi" && !intentAOIIsGrounded(value, workspace, session.PendingIntent, request.Text, history) {
				return Intent{}, invalidIntent()
			}
			if strings.HasSuffix(key, "_id") {
				if id, ok := value.(string); ok && !strings.Contains(groundedText, id) && !intentContainsResource(groundedResources, id) {
					return Intent{}, invalidIntent()
				}
			}
			if strings.HasSuffix(key, "_ids") {
				for _, id := range dataStrings(operation.Slots, key) {
					if !strings.Contains(groundedText, id) && !intentContainsResource(groundedResources, id) {
						return Intent{}, invalidIntent()
					}
				}
			}
		}
	}
	return clarifyMissingDetectionContext(intent, workspace), nil
}

func intentAOIIsGrounded(aoi any, workspace map[string]any, pending *Intent, input string, history []ProviderMessage) bool {
	if aoi == nil {
		return false
	}
	if sameJSON(aoi, normalizeJSONValue(workspace["aoi"])) {
		return true
	}
	if pending != nil {
		for _, operation := range pending.Operations {
			if sameJSON(aoi, normalizeJSONValue(operation.Slots["aoi"])) {
				return true
			}
		}
	}
	texts := []string{input}
	for _, message := range history {
		if message.Role == "user" {
			texts = append(texts, message.Content)
		}
	}
	for _, text := range texts {
		var explicit any
		if json.Unmarshal([]byte(text), &explicit) == nil {
			if sameJSON(aoi, explicit) || sameJSON(aoi, mapValue(explicit)["aoi"]) {
				return true
			}
		}
		encoded, err := json.Marshal(aoi)
		if err == nil && strings.Contains(text, string(encoded)) {
			return true
		}
	}
	return false
}

func intentContainsResource(value any, id string) bool {
	switch value := value.(type) {
	case string:
		return value == id
	case map[string]any:
		for _, item := range value {
			if intentContainsResource(item, id) {
				return true
			}
		}
	case []any:
		for _, item := range value {
			if intentContainsResource(item, id) {
				return true
			}
		}
	}
	return false
}

func (g *Gateway) intentSchema() map[string]any {
	ids := make([]string, 0, len(g.skills))
	for id := range g.skills {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return g.intentSchemaFor(ids)
}

func (g *Gateway) intentSchemaFor(ids []string) map[string]any {
	branches := make([]any, 0, len(ids))
	for _, id := range ids {
		branches = append(branches, map[string]any{
			"type": "object", "additionalProperties": false,
			"required": []any{"skill_id", "slots"},
			"properties": map[string]any{
				"skill_id": map[string]any{"type": "string", "enum": []any{id}},
				"slots":    normalizeJSONMap(g.skills[id].InputSchema),
			},
		})
	}
	return map[string]any{
		"$schema": "https://json-schema.org/draft/2020-12/schema",
		"type":    "object", "additionalProperties": false,
		"required": []any{"operations", "needs_clarification", "question"},
		"properties": map[string]any{
			"operations":          map[string]any{"type": "array", "maxItems": 4, "items": map[string]any{"anyOf": branches}},
			"needs_clarification": map[string]any{"type": "boolean"},
			"question":            map[string]any{"type": "string", "maxLength": 1000},
		},
	}
}

func (g *Gateway) decodeIntent(raw string, schema map[string]any) (Intent, error) {
	if len(raw) == 0 || len(raw) > 64<<10 {
		return Intent{}, invalidIntent()
	}
	decoder := json.NewDecoder(bytes.NewBufferString(raw))
	value, err := decodeIntentJSON(decoder, 0)
	if err != nil {
		return Intent{}, invalidIntent()
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return Intent{}, invalidIntent()
	}
	if err := validateJSONSchema(schema, value); err != nil {
		return Intent{}, invalidIntent()
	}
	// Decode only after the closed schema has checked every field, including slots.
	encoded, _ := json.Marshal(value)
	var intent Intent
	if err := json.Unmarshal(encoded, &intent); err != nil {
		return Intent{}, invalidIntent()
	}
	intent.Question = strings.TrimSpace(intent.Question)
	if len([]rune(intent.Question)) > 1000 || (intent.NeedsClarification && intent.Question == "") ||
		(!intent.NeedsClarification && (len(intent.Operations) == 0 || intent.Question != "")) {
		return Intent{}, invalidIntent()
	}
	for index, operation := range intent.Operations {
		skill, ok := g.skills[operation.SkillID]
		if !ok || operation.Slots == nil || validateIntentSlots(normalizeJSONMap(skill.InputSchema), operation.Slots) != nil {
			return Intent{}, invalidIntent()
		}
		if inherited, _ := operation.Slots["use_previous_result"].(bool); index == 0 && inherited {
			return Intent{}, invalidIntent()
		}
	}
	return intent, nil
}

// A map decoded by encoding/json normally accepts duplicate object fields.
// Reject them here rather than letting the last field silently alter a route.
func decodeIntentJSON(decoder *json.Decoder, depth int) (any, error) {
	if depth > 64 {
		return nil, invalidIntent()
	}
	token, err := decoder.Token()
	if err != nil {
		return nil, err
	}
	switch token {
	case json.Delim('{'):
		object := make(map[string]any)
		for decoder.More() {
			rawKey, err := decoder.Token()
			if err != nil {
				return nil, err
			}
			key, ok := rawKey.(string)
			if _, duplicate := object[key]; !ok || duplicate {
				return nil, invalidIntent()
			}
			value, err := decodeIntentJSON(decoder, depth+1)
			if err != nil {
				return nil, err
			}
			object[key] = value
		}
		closing, err := decoder.Token()
		if err != nil || closing != json.Delim('}') {
			return nil, invalidIntent()
		}
		return object, nil
	case json.Delim('['):
		array := make([]any, 0)
		for decoder.More() {
			value, err := decodeIntentJSON(decoder, depth+1)
			if err != nil {
				return nil, err
			}
			array = append(array, value)
		}
		closing, err := decoder.Token()
		if err != nil || closing != json.Delim(']') {
			return nil, invalidIntent()
		}
		return array, nil
	default:
		if _, delimiter := token.(json.Delim); delimiter {
			return nil, invalidIntent()
		}
		return token, nil
	}
}

func validateIntentSlots(schema map[string]any, value any) error {
	if err := validateJSONSchema(schema, value); err != nil {
		return err
	}
	switch value := value.(type) {
	case string:
		if max, ok := number(schema["maxLength"]); ok && float64(len([]rune(value))) > max {
			return invalidIntent()
		}
	case map[string]any:
		properties, _ := schema["properties"].(map[string]any)
		for key, item := range value {
			if child, ok := properties[key].(map[string]any); ok {
				if err := validateIntentSlots(child, item); err != nil {
					return err
				}
			}
		}
	case []any:
		if child, ok := schema["items"].(map[string]any); ok {
			for _, item := range value {
				if err := validateIntentSlots(child, item); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func intentHistory(session *Session, request MessageRequest) []ProviderMessage {
	history := make([]ProviderMessage, 0, 12)
	for index, message := range session.Messages {
		if message.Role != "user" && message.Role != "assistant" {
			continue
		}
		if message.Role == "assistant" {
			if message.Phase == "intermediate" || (message.Provider != nil && len(message.Provider.ToolCalls) > 0) || stringValue(message.Data["kind"]) == "process" {
				continue
			}
		}
		if message.Role == "user" && index == len(session.Messages)-1 && message.Content == request.Text {
			continue
		}
		if strings.TrimSpace(message.Content) == "" {
			continue
		}
		text := []rune(message.Content)
		if len(text) > 1800 {
			text = text[:1800]
		}
		history = append(history, ProviderMessage{Role: message.Role, Content: string(text)})
	}
	if len(history) > 12 {
		history = history[len(history)-12:]
	}
	return history
}

func clarifyMissingDetectionContext(intent Intent, workspace map[string]any) Intent {
	if intent.NeedsClarification {
		return intent
	}
	for _, operation := range intent.Operations {
		if operation.SkillID != "change.detect.v1" {
			continue
		}
		slots := operation.Slots
		if inherited, _ := slots["use_previous_result"].(bool); inherited {
			// The previous operation will provide verified resources at execution.
			continue
		}
		useWorkspace, _ := slots["use_workspace_context"].(bool)
		hasRegion := stringValue(slots["region_id"]) != "" || len(dataStrings(slots, "admin_path")) > 0
		if useWorkspace && (stringValue(workspace["region_id"]) != "" || workspace["aoi"] != nil) {
			hasRegion = true
		}
		if !hasRegion {
			intent.NeedsClarification = true
			intent.Question = "请指定要检测的行政区，或在工作区选择区域并绘制检测范围。"
			return intent
		}
		hasDates := len(dataStrings(slots, "phase_dates")) >= 2 || slots["date_range"] != nil || len(dataStrings(slots, "phase_ids")) >= 2 || len(dataStrings(slots, "scene_ids")) >= 2
		if useWorkspace && (len(dataStrings(workspace, "phase_ids")) >= 2 || len(dataRows(workspace, "phases")) >= 2) {
			hasDates = true
		}
		if !hasDates {
			intent.NeedsClarification = true
			intent.Question = "请指定要比较的两个时相或日期范围，也可以在工作区加载两个时相。"
			return intent
		}
	}
	return intent
}

func (g *Gateway) intentPromptFor(ids []string) string {
	selected := make([]map[string]any, 0, len(ids))
	for _, id := range ids {
		skill := g.skills[id]
		selected = append(selected, map[string]any{"skill_id": id, "instructions": skill.Body, "input_schema": skill.InputSchema})
	}
	encoded, _ := json.Marshal(selected)
	return promptText("intent") + string(encoded)
}

func (g *Gateway) selectSkills(ctx context.Context, session *Session, request MessageRequest) ([]string, error) {
	ids := []any{}
	for _, skill := range g.skillCatalog() {
		ids = append(ids, skill["skill_id"])
	}
	schema := map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{"skill_ids": map[string]any{"type": "array", "minItems": 1, "maxItems": 4, "items": map[string]any{"type": "string", "enum": ids}}}, "required": []any{"skill_ids"}}
	workspace := session.Workspace
	if request.Workspace != nil {
		workspace = request.Workspace
	}
	encoded, err := json.Marshal(map[string]any{"input": request.Text, "history": intentHistory(session, request), "workspace": scrubExternal(normalizeJSONValue(workspace)), "pending_intent": session.PendingIntent, "skills": g.skillCatalog()})
	if err != nil {
		return nil, invalidIntent()
	}
	g.requestMu.Lock()
	provider := g.provider
	g.requestMu.Unlock()
	var selected []string
	err = completeStructuredIntent(ctx, provider, CompletionRequest{Stage: "skill_selection", Messages: []ProviderMessage{{Role: "system", Content: promptText("selection")}, {Role: "user", Content: string(encoded)}}, Tools: []ProviderTool{{Type: "function", Function: ProviderFunction{Name: "select_skills", Description: "Select relevant skills from metadata only.", Parameters: schema}}}}, func(raw string) error {
		if len(raw) > 8192 {
			return invalidIntent()
		}
		decoder := json.NewDecoder(strings.NewReader(raw))
		value, decodeErr := decodeIntentJSON(decoder, 0)
		if decodeErr != nil || validateJSONSchema(schema, value) != nil {
			return invalidIntent()
		}
		if err := decoder.Decode(new(any)); err != io.EOF {
			return invalidIntent()
		}
		selected = dataStrings(mapValue(value), "skill_ids")
		seen := map[string]bool{}
		for _, id := range selected {
			if seen[id] {
				return invalidIntent()
			}
			seen[id] = true
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return selected, nil
}

type intentProgressKey struct{}

func withIntentProgress(ctx context.Context, callback func(string)) context.Context {
	return context.WithValue(ctx, intentProgressKey{}, callback)
}

// Only repair structural output once, before any business tools are exposed.
// Never echo malformed output back as instructions or relax the closed schema.
func completeStructuredIntent(ctx context.Context, provider Provider, request CompletionRequest, decode func(string) error) error {
	for attempt := 0; attempt < 2; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		response, err := provider.Complete(ctx, request)
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		raw := response.Message.Content
		validCall := true
		if calls := response.Message.ToolCalls; len(calls) > 0 {
			validCall = len(calls) == 1 && calls[0].Function.Name == request.Tools[0].Function.Name
			if validCall {
				raw = calls[0].Function.Arguments
			}
		}
		if validCall && decode(raw) == nil {
			return nil
		}
		if attempt == 0 {
			if callback, ok := ctx.Value(intentProgressKey{}).(func(string)); ok {
				callback(request.Stage)
			}
			request.Messages = append(request.Messages, ProviderMessage{Role: "system", Content: "上次返回未通过格式校验。请重新读取原始输入，只调用指定工具一次，严格遵循闭合 schema：字段名、类型、枚举、必填项均须正确，不增加字段，不输出 Markdown。无法判断需求时使用 schema 允许的澄清字段。"})
		}
	}
	label := "需求解析"
	if request.Stage == "skill_selection" {
		label = "操作识别"
	}
	return &IntentError{Code: "AI_INTENT_INVALID", Stage: request.Stage, Message: label + "返回格式无效，自动纠正一次后仍未通过；本轮未执行任何操作。请明确操作和对象，例如：检测当前框选区域 2025年1月和5月的差异；或查看上次检测结果。"}
}
