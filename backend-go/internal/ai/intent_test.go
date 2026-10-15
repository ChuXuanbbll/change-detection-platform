package ai

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

type intentFixtureProvider struct {
	response CompletionResponse
	err      error
	requests []CompletionRequest
}

func (p *intentFixtureProvider) Complete(_ context.Context, request CompletionRequest) (CompletionResponse, error) {
	if request.Stage == "skill_selection" {
		return fixtureSkillSelection(request), nil
	}
	p.requests = append(p.requests, request)
	return p.response, p.err
}

func intentGateway(t *testing.T, raw string, tool bool) (*Gateway, *intentFixtureProvider) {
	t.Helper()
	skills, err := loadSkillManifests()
	if err != nil {
		t.Fatal(err)
	}
	provider := &intentFixtureProvider{response: CompletionResponse{Message: ProviderMessage{Role: "assistant", Content: raw}}}
	if tool {
		call := ProviderToolCall{ID: "route-1", Type: "function"}
		call.Function.Name = "route_intent"
		call.Function.Arguments = raw
		provider.response.Message = ProviderMessage{Role: "assistant", ToolCalls: []ProviderToolCall{call}}
	}
	return &Gateway{skills: skills, provider: provider}, provider
}

// These fixtures verify the routing contract and safe dispatch, not model
// accuracy. Live-provider semantic acceptance is a separate integration check.
func TestParseIntentFixtureRoutesQueriesWithoutDetection(t *testing.T) {
	for _, test := range []struct {
		name, text, skill, slots string
	}{
		{"last result", "查看上次检测结果", "result.inspect.v1", `{"operation":"summary","latest":true}`},
		{"change export", "导出变化图斑", "result.inspect.v1", `{"operation":"export","latest":true}`},
		{"negative detection", "不要检测，只查看上次结果", "result.inspect.v1", `{"operation":"summary","latest":true}`},
		{"status", "查看任务状态", "result.inspect.v1", `{"operation":"query_tasks"}`},
		{"models", "有哪些可用模型", "imagery.explore.v1", `{"operation":"models"}`},
		{"prepared images", "列出已准备的影像", "imagery.explore.v1", `{"operation":"images"}`},
		{"locate", "定位到河北省雄安新区", "imagery.explore.v1", `{"operation":"locate","admin_path":["河北省","雄安新区"]}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			raw := `{"operations":[{"skill_id":"` + test.skill + `","slots":` + test.slots + `}],"needs_clarification":false,"question":""}`
			g, provider := intentGateway(t, raw, true)
			intent, err := g.parseIntent(context.Background(), &Session{ID: "current"}, MessageRequest{Text: test.text})
			if err != nil || len(intent.Operations) != 1 || intent.Operations[0].SkillID != test.skill || intent.NeedsClarification {
				t.Fatalf("intent = %+v, err = %v", intent, err)
			}
			request := provider.requests[0]
			if request.Stage != "intent" || len(request.Tools) != 1 || request.Tools[0].Function.Name != "route_intent" {
				t.Fatalf("parser exposed business tools: %+v", request)
			}
			if err := validateProviderParameters(request.Tools[0].Function.Parameters); err != nil {
				t.Fatalf("route tool is not a closed provider schema: %v", err)
			}
		})
	}
}

func TestParseIntentRetainsPartialClarificationAndCompletesShortReply(t *testing.T) {
	g, provider := intentGateway(t, `{"operations":[{"skill_id":"change.detect.v1","slots":{"operation":"detect","admin_path":["河北省","雄安新区"]}}],"needs_clarification":true,"question":"要比较哪两个日期？"}`, true)
	session := &Session{ID: "current"}
	partial, err := g.parseIntent(context.Background(), session, MessageRequest{Text: "检测雄安的变化"})
	if err != nil || !partial.NeedsClarification || partial.Question == "" {
		t.Fatalf("partial = %+v, err = %v", partial, err)
	}
	session.PendingIntent = &partial
	session.Messages = []SessionMessage{{Role: "user", Content: "检测雄安的变化"}, {Role: "assistant", Content: partial.Question, Phase: "final"}}
	provider.response.Message.ToolCalls[0].Function.Arguments = `{"operations":[{"skill_id":"change.detect.v1","slots":{"operation":"detect","admin_path":["河北省","雄安新区"],"date_range":{"start":"2025-01","end":"2025-11","precision":"month"}}}],"needs_clarification":false,"question":""}`
	completed, err := g.parseIntent(context.Background(), session, MessageRequest{Text: "2025年1月至11月"})
	if err != nil || completed.NeedsClarification || completed.Operations[0].Slots["date_range"] == nil {
		t.Fatalf("completed = %+v, err = %v", completed, err)
	}
	var input map[string]any
	if err := json.Unmarshal([]byte(provider.requests[1].Messages[1].Content), &input); err != nil || input["pending_intent"] == nil {
		t.Fatalf("parser lost confirmed slots: %v", err)
	}
}

func TestParseIntentTaskSwitchDoesNotMergeOldDetectionSlots(t *testing.T) {
	g, provider := intentGateway(t, `{"operations":[{"skill_id":"result.inspect.v1","slots":{"operation":"summary","latest":true}}],"needs_clarification":false,"question":""}`, false)
	pending := &Intent{NeedsClarification: true, Question: "日期？", Operations: []IntentOperation{{SkillID: "change.detect.v1", Slots: map[string]any{"admin_path": []any{"雄安"}}}}}
	intent, err := g.parseIntent(context.Background(), &Session{PendingIntent: pending}, MessageRequest{Text: "先不检测，改为查看上次结果"})
	if err != nil || intent.Operations[0].SkillID != "result.inspect.v1" || intent.Operations[0].Slots["admin_path"] != nil {
		t.Fatalf("intent = %+v, err = %v", intent, err)
	}
	if !strings.Contains(provider.requests[0].Messages[0].Content, "丢弃旧任务的无关槽位") {
		t.Fatal("task switching rule missing from prompt")
	}
}

func TestParseIntentOrderedCombinationUsesReferencesWithoutInventedIDs(t *testing.T) {
	raw := `{"operations":[{"skill_id":"change.detect.v1","slots":{"operation":"detect","use_workspace_context":true}},{"skill_id":"result.inspect.v1","slots":{"operation":"export","output_format":"geojson","use_previous_result":true}}],"needs_clarification":false,"question":""}`
	g, _ := intentGateway(t, raw, true)
	workspace := map[string]any{"region_id": "known-region", "phase_ids": []any{"p1", "p2"}}
	intent, err := g.parseIntent(context.Background(), &Session{Workspace: workspace}, MessageRequest{Text: "检测当前区域，然后把结果导出为GeoJSON"})
	if err != nil || intent.NeedsClarification || len(intent.Operations) != 2 || intent.Operations[1].Slots["use_previous_result"] != true {
		t.Fatalf("intent = %+v, err = %v", intent, err)
	}
}

func TestParseIntentRejectsMalformedOrUnsafeResponses(t *testing.T) {
	for _, raw := range []string{
		`not json`,
		`{"operations":[],"needs_clarification":false,"question":""}`,
		`{"operations":[],"needs_clarification":true,"question":""}`,
		`{"operations":[],"needs_clarification":true,"question":"哪个结果？","extra":true}`,
		`{"operations":[{"skill_id":"unknown","slots":{}}],"needs_clarification":false,"question":""}`,
		`{"operations":[{"skill_id":"result.inspect.v1","slots":{"shell":"x"}}],"needs_clarification":false,"question":""}`,
		`{"operations":[{"skill_id":"result.inspect.v1","slots":{"operation":"detect"}}],"needs_clarification":false,"question":""}`,
		`{"operations":[{"skill_id":"result.inspect.v1","slots":{"confidence_min":2}}],"needs_clarification":false,"question":""}`,
		`{"operations":[{"skill_id":"result.inspect.v1","slots":{"task_id":"invented-task"}}],"needs_clarification":false,"question":""}`,
		`{"operations":[{"skill_id":"result.inspect.v1","slots":{"query":"` + strings.Repeat("x", 257) + `"}}],"needs_clarification":false,"question":""}`,
		`{"operations":[],"needs_clarification":true,"needs_clarification":false,"question":"哪个结果？"}`,
		`{"operations":[{"skill_id":"result.inspect.v1","slots":{"operation":"summary","operation":"export"}}],"needs_clarification":false,"question":""}`,
		`{"operations":[{"skill_id":"result.inspect.v1","slots":{}}],"needs_clarification":false,"question":""} {}`,
	} {
		t.Run(raw, func(t *testing.T) {
			g, _ := intentGateway(t, raw, true)
			_, err := g.parseIntent(context.Background(), &Session{}, MessageRequest{Text: "查看结果"})
			var safe *IntentError
			if !errors.As(err, &safe) || safe.Code != "AI_INTENT_INVALID" || strings.Contains(safe.Message, "invented-task") {
				t.Fatalf("expected safe validation error, got %v", err)
			}
		})
	}
	g, provider := intentGateway(t, `{}`, true)
	provider.response.Message.ToolCalls[0].Function.Name = "detection.workflow.submit"
	if _, err := g.parseIntent(context.Background(), &Session{}, MessageRequest{Text: "查看结果"}); err == nil {
		t.Fatal("parser accepted a business tool")
	}
}

func TestParseIntentMissingDetectionConditionsRequiresClarification(t *testing.T) {
	for _, slots := range []string{`{"operation":"detect"}`, `{"operation":"detect","admin_path":["雄安"]}`, `{"operation":"detect","use_workspace_context":true}`} {
		g, _ := intentGateway(t, `{"operations":[{"skill_id":"change.detect.v1","slots":`+slots+`}],"needs_clarification":false,"question":""}`, true)
		intent, err := g.parseIntent(context.Background(), &Session{}, MessageRequest{Text: "检测一下"})
		if err != nil || !intent.NeedsClarification || intent.Question == "" {
			t.Fatalf("missing conditions could execute: %+v, %v", intent, err)
		}
	}
}

func TestIntentHistoryContainsOnlyThisSessionUsersAndFinals(t *testing.T) {
	g, provider := intentGateway(t, `{"operations":[{"skill_id":"result.inspect.v1","slots":{"task_id":"known-task","operation":"summary"}}],"needs_clarification":false,"question":""}`, true)
	session := &Session{ID: "only-this-session", Messages: []SessionMessage{
		{Role: "user", Content: "旧需求"},
		{Role: "assistant", Content: "intermediate-secret", Phase: "intermediate"},
		{Role: "tool", Content: "tool-secret"},
		{Role: "assistant", Content: "已完成 known-task", Phase: "final"},
		{Role: "user", Content: "查看结果"},
	}, Workspace: map[string]any{"region_id": "r1", "api_key": "secret-never-send"}}
	intent, err := g.parseIntent(context.Background(), session, MessageRequest{Text: "查看结果", Workspace: map[string]any{"task_id": "known-task", "api_key": "secret-never-send"}})
	if err != nil || intent.NeedsClarification {
		t.Fatalf("known task rejected: %+v, %v", intent, err)
	}
	input := provider.requests[0].Messages[1].Content
	if strings.Contains(input, "intermediate-secret") || strings.Contains(input, "tool-secret") || strings.Contains(input, "secret-never-send") || strings.Contains(input, `"region_id":"r1"`) {
		t.Fatalf("parser received unscoped context: %s", input)
	}
	if strings.Count(input, "查看结果") != 1 || !strings.Contains(input, "已完成 known-task") {
		t.Fatalf("current input repeated or final missing: %s", input)
	}
}

func TestParseIntentControlledSkillAndCancellationAvoidProvider(t *testing.T) {
	g, provider := intentGateway(t, "unused", false)
	intent, err := g.parseIntent(context.Background(), &Session{}, MessageRequest{Text: "controlled", SkillID: "result.inspect.v1"})
	if err != nil || len(intent.Operations) != 1 || len(provider.requests) != 0 {
		t.Fatalf("explicit skill route = %+v, %v", intent, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := g.parseIntent(ctx, &Session{}, MessageRequest{Text: "查看结果"}); !errors.Is(err, context.Canceled) || len(provider.requests) != 0 {
		t.Fatalf("cancelled parse called provider: %v", err)
	}
	provider.err = errors.New("provider failed")
	if _, err := g.parseIntent(context.Background(), &Session{}, MessageRequest{Text: "查看结果"}); !errors.Is(err, provider.err) {
		t.Fatalf("provider diagnostic lost: %v", err)
	}
}

func TestParseIntentDoesNotInventAOIFromRegionName(t *testing.T) {
	aoi := map[string]any{"type": "Polygon", "coordinates": []any{[]any{[]any{114.0, 38.0}, []any{115.0, 38.0}, []any{115.0, 39.0}, []any{114.0, 38.0}}}}
	encoded, _ := json.Marshal(aoi)
	raw := `{"operations":[{"skill_id":"change.detect.v1","slots":{"operation":"detect","admin_path":["雄安"],"date_range":{"start":"2025-01","end":"2025-11","precision":"month"},"aoi":` + string(encoded) + `}}],"needs_clarification":false,"question":""}`
	g, _ := intentGateway(t, raw, true)
	if _, err := g.parseIntent(context.Background(), &Session{}, MessageRequest{Text: "检测雄安2025年1月至11月变化"}); err == nil {
		t.Fatal("invented coordinates passed routing")
	}
	if _, err := g.parseIntent(context.Background(), &Session{Workspace: map[string]any{"aoi": aoi}}, MessageRequest{Text: "检测雄安2025年1月至11月变化"}); err != nil {
		t.Fatalf("existing workspace polygon rejected: %v", err)
	}
	if !intentAOIIsGrounded(aoi, nil, nil, "使用这个范围: "+string(encoded), nil) {
		t.Fatal("explicit polygon rejected")
	}
}
