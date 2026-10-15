package ai

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

type Gateway struct {
	repo             *repository
	registry         *Registry
	provider         Provider
	config           ProviderConfig
	providerOverride bool
	skills           map[string]SkillManifest
	domainWorkflows  WorkflowProvider
	acks             map[string]chan ActionAck
	ackMu            sync.Mutex
	closed           chan struct{}
	runMu            sync.Mutex
	runs             map[string]context.CancelFunc
	workers          sync.WaitGroup
	requestMu        sync.Mutex
}

func NewGateway(dataDir string, specs []ToolSpec, executor Executor) (*Gateway, error) {
	config := ProviderConfigFromEnv()
	repo, err := newRepository(dataDir)
	if err != nil {
		return nil, err
	}
	all := append([]ToolSpec{}, specs...)
	all = append(all, uiToolSpecs()...)
	registry, err := NewRegistry(all, executor)
	if err != nil {
		return nil, err
	}
	skills, err := loadSkillManifests()
	if err != nil {
		return nil, err
	}
	g := &Gateway{repo: repo, registry: registry, provider: NewOpenAIProvider(config), config: config, skills: skills, acks: make(map[string]chan ActionAck), closed: make(chan struct{}), runs: make(map[string]context.CancelFunc)}
	if provider, ok := executor.(WorkflowProvider); ok {
		g.domainWorkflows = provider
	}
	return g, nil
}

func (g *Gateway) SetProvider(provider Provider) {
	if provider != nil {
		g.provider = provider
		g.providerOverride = true
	}
}

// ConfigureProvider updates the managed provider without changing test or
// embedding overrides installed through SetProvider.
func (g *Gateway) ConfigureProvider(config ProviderConfig) {
	if config.Timeout <= 0 {
		config.Timeout = 45 * time.Second
	}
	g.requestMu.Lock()
	defer g.requestMu.Unlock()
	g.config = config
	if !g.providerOverride {
		g.provider = NewOpenAIProvider(config)
	}
}
func (g *Gateway) SetExecutor(executor Executor) {
	if executor != nil {
		g.registry.executor = executor
		if provider, ok := executor.(WorkflowProvider); ok {
			g.domainWorkflows = provider
		}
	}
}
func (g *Gateway) Close() {
	select {
	case <-g.closed:
	default:
		close(g.closed)
	}
	g.runMu.Lock()
	for _, cancel := range g.runs {
		cancel()
	}
	g.runMu.Unlock()
	g.workers.Wait()
}

func (g *Gateway) Register(group *gin.RouterGroup) {
	group.GET("/ai/status", g.status)
	group.GET("/ai/tools", g.tools)
	group.POST("/ai/tools/call", g.toolCall)
	group.GET("/ai/skills", g.skillsHandler)
	group.POST("/ai/sessions", g.createSession)
	group.GET("/ai/sessions", g.listSessions)
	group.GET("/ai/sessions/:id", g.getSession)
	group.POST("/ai/sessions/:id/messages", g.message)
	group.GET("/ai/workflows/:id", g.getWorkflow)
	group.GET("/ai/workflows/:id/events", g.workflowEvents)
	group.POST("/ai/workflows/:id/cancel", g.cancelWorkflow)
	group.POST("/ai/sessions/:id/actions/:actionID", g.actionAck)
}

func (g *Gateway) status(c *gin.Context) {
	g.requestMu.Lock()
	config, override := g.config, g.providerOverride
	g.requestMu.Unlock()
	c.JSON(http.StatusOK, gin.H{"configured": config.Configured() || override, "model": config.Model, "provider": config.BaseURL != "" || override})
}
func (g *Gateway) tools(c *gin.Context) {
	g.requestMu.Lock()
	config, override := g.config, g.providerOverride
	g.requestMu.Unlock()
	c.JSON(http.StatusOK, gin.H{"configured": config.Configured() || override, "tools": g.registry.Specs()})
}
func (g *Gateway) skillsHandler(c *gin.Context) {
	out := make([]SkillManifest, 0, len(g.skills))
	for _, s := range g.skills {
		out = append(out, s)
	}
	c.JSON(http.StatusOK, gin.H{"skills": out})
}

func (g *Gateway) createSession(c *gin.Context) {
	s := Session{ID: uuid.NewString(), CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(), Actions: make(map[string]PendingAction)}
	if err := g.repo.upsertSession(s); err != nil {
		c.JSON(500, gin.H{"error": "session persistence failed"})
		return
	}
	c.JSON(http.StatusCreated, s)
}
func (g *Gateway) listSessions(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"sessions": g.repo.listSessions()})
}
func (g *Gateway) getSession(c *gin.Context) {
	s, ok := g.repo.getSession(c.Param("id"))
	if !ok {
		c.JSON(404, gin.H{"error": "session not found"})
		return
	}
	c.JSON(http.StatusOK, s)
}

func (g *Gateway) toolCall(c *gin.Context) {
	var call Call
	dec := json.NewDecoder(io.LimitReader(c.Request.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&call); err != nil {
		c.JSON(400, gin.H{"error": "invalid tool call"})
		return
	}
	result := g.registry.Invoke(c.Request.Context(), call)
	code := 200
	if result.Status == "error" {
		code = 400
	}
	c.JSON(code, result)
}

func (g *Gateway) message(c *gin.Context) {
	var request MessageRequest
	dec := json.NewDecoder(io.LimitReader(c.Request.Body, 256<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&request); err != nil || strings.TrimSpace(request.Text) == "" {
		c.JSON(400, gin.H{"error": "text is required"})
		return
	}
	sessionID := c.Param("id")
	if _, ok := g.repo.getSession(sessionID); !ok {
		c.JSON(404, gin.H{"error": "session not found"})
		return
	}
	if request.SkillID != "" {
		if _, exists := g.skills[request.SkillID]; !exists {
			c.JSON(400, gin.H{"error": "unknown skill"})
			return
		}
	}
	if request.RequestID == "" {
		request.RequestID = uuid.NewString()
	}
	fingerprint := messageFingerprint(request.Text)
	g.requestMu.Lock()
	// Re-read under the request lock. The first read can be stale when two
	// messages for one session arrive concurrently; using it would overwrite
	// the other request's updated history or workspace.
	s, ok := g.repo.getSession(sessionID)
	if !ok {
		g.requestMu.Unlock()
		c.JSON(404, gin.H{"error": "session not found"})
		return
	}
	if existing := g.findWorkflowByRequest(s.ID, request.RequestID); existing != nil {
		if existing.RequestFingerprint != "" && existing.RequestFingerprint != fingerprint {
			g.requestMu.Unlock()
			c.JSON(http.StatusConflict, gin.H{"error": "request_id was already used with different text"})
			return
		}
		if existing.RequestFingerprint == "" && workflowTextFingerprint(existing) != fingerprint {
			g.requestMu.Unlock()
			c.JSON(http.StatusConflict, gin.H{"error": "request_id was already used with different text"})
			return
		}
		g.requestMu.Unlock()
		g.sseHeaders(c)
		g.streamWorkflow(c, existing.ID)
		return
	}
	for _, activeID := range s.WorkflowIDs {
		if active, exists := g.repo.getWorkflow(activeID); exists && (active.Status == "running" || active.Status == "queued") {
			g.requestMu.Unlock()
			c.JSON(http.StatusConflict, gin.H{"error": "another message is still running for this session", "workflow_id": active.ID})
			return
		}
	}
	if request.Workspace != nil {
		s.Workspace = request.Workspace
	}
	s.UpdatedAt = time.Now().UTC()
	s.Messages = append(s.Messages, SessionMessage{ID: uuid.NewString(), Role: "user", Content: request.Text, CreatedAt: time.Now().UTC(), Data: map[string]any{"request_id": request.RequestID}})
	_ = g.repo.upsertSession(s)
	skillID := request.SkillID
	if skillID == "" {
		skillID = selectSkill(request.Text)
	}
	s.SkillID = skillID
	workflow := Workflow{ID: uuid.NewString(), SessionID: s.ID, SkillID: skillID, RequestID: request.RequestID, RequestFingerprint: fingerprint, Status: "running", CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
	workflow.Events = append(workflow.Events, WorkflowEvent{ID: 1, Type: "message", Data: map[string]any{"text": request.Text}, CreatedAt: time.Now().UTC()})
	s.WorkflowIDs = append(s.WorkflowIDs, workflow.ID)
	_ = g.repo.upsertWorkflow(workflow)
	_ = g.repo.upsertSession(s)
	g.requestMu.Unlock()
	g.sseHeaders(c)
	runCtx, cancel := context.WithCancel(context.Background())
	g.runMu.Lock()
	g.runs[workflow.ID] = cancel
	g.runMu.Unlock()
	g.workers.Add(1)
	go func() {
		defer g.workers.Done()
		defer cancel()
		defer func() { g.runMu.Lock(); delete(g.runs, workflow.ID); g.runMu.Unlock() }()
		if err := g.runConversation(runCtx, &s, &workflow, request, io.Discard); err != nil {
			if runCtx.Err() != nil {
				workflow.Status = "cancelled"
			} else {
				workflow.Status = "failed"
			}
			g.emitEvent(&workflow, io.Discard, "error", map[string]any{"code": "AI_REQUEST_FAILED", "message": "AI request could not be completed"})
		}
	}()
	g.streamWorkflow(c, workflow.ID)
}

func (g *Gateway) runConversation(ctx context.Context, session *Session, workflow *Workflow, request MessageRequest, w io.Writer) error {
	g.requestMu.Lock()
	config, provider, override := g.config, g.provider, g.providerOverride
	g.requestMu.Unlock()
	if !config.Configured() && !override {
		g.emitEvent(workflow, w, "error", map[string]any{"code": "AI_NOT_CONFIGURED", "message": "AI provider is not configured; set AI_API_BASE, AI_API_KEY, and AI_MODEL"})
		workflow.Status = "failed"
		return g.repo.upsertWorkflow(*workflow)
	}
	allowedTools := []string{}
	if skill, ok := g.skills[workflow.SkillID]; ok {
		allowedTools = skill.AllowedTools
	} else {
		for _, spec := range g.registry.Specs() {
			allowedTools = append(allowedTools, spec.Name)
		}
	}
	tools, err := g.registry.ProviderTools(allowedTools)
	if err != nil {
		workflow.Status = "failed"
		_ = g.repo.upsertWorkflow(*workflow)
		return fmt.Errorf("build provider tools: %w", err)
	}
	systemContent := "You are the change detection workspace assistant. Use only registered tools and return concise Chinese explanations. Never invent resource IDs or coordinates. The server enforces the Skill state machine, prerequisites, resource IDs, retries, and bounded outputs."
	if skill, ok := g.skills[workflow.SkillID]; ok {
		manifest := map[string]any{"skill_id": skill.ID, "skill_version": skill.Version, "allowed_tools": skill.AllowedTools, "steps": skill.Steps, "clarification_rules": skill.ClarificationRules, "failure_map": skill.FailureMap}
		if encoded, encodeErr := json.Marshal(manifest); encodeErr == nil {
			systemContent += " Selected Skill manifest (follow it and ask when clarification is required): " + string(encoded)
		}
	}
	messages := []ProviderMessage{{Role: "system", Content: systemContent}}
	for _, prior := range session.Messages {
		if prior.Provider != nil {
			messages = append(messages, *prior.Provider)
			continue
		}
		if prior.Role == "user" || prior.Role == "assistant" || prior.Role == "tool" {
			messages = append(messages, ProviderMessage{Role: prior.Role, Content: prior.Content, ToolCallID: prior.ToolCallID})
		}
	}
	if len(messages) == 1 {
		messages = append(messages, ProviderMessage{Role: "user", Content: request.Text})
	}
	maxSteps := 12
	if skill, ok := g.skills[workflow.SkillID]; ok && skill.MaxSteps > 0 {
		maxSteps = skill.MaxSteps
	}
	for step := 0; step <= maxSteps; step++ {
		response, err := provider.Complete(ctx, CompletionRequest{Messages: messages, Tools: tools})
		if err != nil {
			workflow.Status = "failed"
			_ = g.repo.upsertWorkflow(*workflow)
			return err
		}
		if response.Message.Content != "" || len(response.Message.ToolCalls) > 0 {
			session.Messages = append(session.Messages, SessionMessage{ID: uuid.NewString(), Role: "assistant", Content: response.Message.Content, CreatedAt: time.Now().UTC(), Provider: &response.Message})
			_ = g.repo.upsertSession(*session)
			if response.Message.Content != "" {
				g.emitEvent(workflow, w, "text_delta", map[string]any{"text": response.Message.Content})
			}
		}
		if len(response.Message.ToolCalls) == 0 {
			workflow.Status = "succeeded"
			workflow.UpdatedAt = time.Now().UTC()
			_ = g.repo.upsertSession(*session)
			_ = g.repo.upsertWorkflow(*workflow)
			g.emitEvent(workflow, w, "final", map[string]any{"status": "succeeded"})
			return nil
		}
		messages = append(messages, response.Message)
		for _, toolCall := range response.Message.ToolCalls {
			if workflow.StepsUsed >= maxSteps {
				name := internalToolName(toolCall.Function.Name)
				result := Result{SchemaVersion: schemaVersion, RequestID: uuid.NewString(), Tool: name, ToolVersion: "1.0.0", Status: "error", Error: &ToolError{Code: "SKILL_STEP_LIMIT", Message: "Skill step limit has been reached", Retryable: false}}
				g.emitEvent(workflow, w, "tool_finished", result)
				encoded, _ := json.Marshal(result)
				g.appendToolReply(session, &messages, toolCall.ID, name, string(encoded))
				continue
			}
			workflow.StepsUsed++
			workflow.UpdatedAt = time.Now().UTC()
			_ = g.repo.upsertWorkflow(*workflow)
			input := map[string]any{}
			if err := json.Unmarshal([]byte(toolCall.Function.Arguments), &input); err != nil {
				g.appendToolReply(session, &messages, toolCall.ID, internalToolName(toolCall.Function.Name), "invalid tool arguments")
				continue
			}
			toolCall.Function.Name = internalToolName(toolCall.Function.Name)
			if strings.HasPrefix(toolCall.Function.Name, "ui.") {
				if !g.skillAllows(workflow, toolCall.Function.Name) {
					g.appendToolReply(session, &messages, toolCall.ID, toolCall.Function.Name, "UI tool is not allowed by the selected skill")
					continue
				}
				if spec, exists := g.registry.Lookup(toolCall.Function.Name, "1.0.0"); !exists {
					result := Result{SchemaVersion: schemaVersion, RequestID: uuid.NewString(), Tool: toolCall.Function.Name, ToolVersion: "1.0.0", Status: "error", Error: &ToolError{Code: "TOOL_NOT_FOUND", Message: "UI tool is not registered", Retryable: false}}
					g.emitEvent(workflow, w, "tool_finished", result)
					encoded, _ := json.Marshal(result)
					g.appendToolReply(session, &messages, toolCall.ID, toolCall.Function.Name, string(encoded))
					continue
				} else if err := validateJSONSchema(spec.InputSchema, input); err != nil {
					result := Result{SchemaVersion: schemaVersion, RequestID: uuid.NewString(), Tool: toolCall.Function.Name, ToolVersion: "1.0.0", Status: "error", Error: &ToolError{Code: "INVALID_INPUT", Message: err.Error(), Retryable: false}}
					g.emitEvent(workflow, w, "tool_finished", result)
					encoded, _ := json.Marshal(result)
					g.appendToolReply(session, &messages, toolCall.ID, toolCall.Function.Name, string(encoded))
					continue
				}
				if !g.skillPrerequisitesSatisfied(workflow, toolCall.Function.Name, input) {
					result := Result{SchemaVersion: schemaVersion, RequestID: uuid.NewString(), Tool: toolCall.Function.Name, ToolVersion: "1.0.0", Status: "error", Error: &ToolError{Code: "SKILL_PREREQUISITE_FAILED", Message: "a prerequisite tool must succeed before this UI action", Retryable: false}}
					g.emitEvent(workflow, w, "tool_finished", result)
					encoded, _ := json.Marshal(result)
					g.appendToolReply(session, &messages, toolCall.ID, toolCall.Function.Name, string(encoded))
					continue
				}
				if err := g.handleUICommand(ctx, session, workflow, w, toolCall, input); err != nil {
					result := Result{SchemaVersion: schemaVersion, RequestID: uuid.NewString(), Tool: toolCall.Function.Name, ToolVersion: "1.0.0", Status: "error", Error: &ToolError{Code: "UI_ACTION_FAILED", Message: "UI action was not applied: " + err.Error(), Retryable: true}}
					g.emitEvent(workflow, w, "tool_finished", result)
					encoded, _ := json.Marshal(result)
					g.appendToolReply(session, &messages, toolCall.ID, toolCall.Function.Name, string(encoded))
					continue
				}
				uiCall := WorkflowToolCall{Tool: toolCall.Function.Name, Version: "1.0.0", Status: "ok", RequestID: uuid.NewString(), Input: input, Data: map[string]any{"status": "applied"}}
				workflow.ToolCalls = append(workflow.ToolCalls, uiCall)
				workflow.UpdatedAt = time.Now().UTC()
				_ = g.repo.upsertWorkflow(*workflow)
				uiResult := Result{SchemaVersion: schemaVersion, RequestID: uiCall.RequestID, Tool: uiCall.Tool, ToolVersion: uiCall.Version, Status: "ok", Data: map[string]any{"status": "applied", "action_id": latestAppliedActionID(session)}}
				if spec, exists := g.registry.Lookup(uiCall.Tool, uiCall.Version); exists {
					if err := validateJSONSchema(spec.OutputSchema, uiResult.Data); err != nil {
						uiResult.Status = "error"
						uiResult.Error = &ToolError{Code: "INVALID_TOOL_OUTPUT", Message: "UI acknowledgement does not satisfy its declared schema", Retryable: false}
					}
				}
				encoded, _ := json.Marshal(uiResult)
				g.appendToolReply(session, &messages, toolCall.ID, uiCall.Tool, string(encoded))
				continue
			}
			name, version := splitToolName(toolCall.Function.Name)
			if version == "" {
				version = "1.0.0"
			}
			if !g.skillAllows(workflow, name) {
				result := Result{SchemaVersion: schemaVersion, RequestID: uuid.NewString(), Tool: name, ToolVersion: version, Status: "error", Error: &ToolError{Code: "SKILL_TOOL_NOT_ALLOWED", Message: "tool is not allowed by the selected skill", Retryable: false}}
				g.emitEvent(workflow, w, "tool_finished", result)
				encoded, _ := json.Marshal(result)
				g.appendToolReply(session, &messages, toolCall.ID, name, string(encoded))
				continue
			}
			if !g.skillPrerequisitesSatisfied(workflow, name, input) {
				result := Result{SchemaVersion: schemaVersion, RequestID: uuid.NewString(), Tool: name, ToolVersion: version, Status: "error", Error: &ToolError{Code: "SKILL_PREREQUISITE_FAILED", Message: "a prerequisite tool must succeed before this tool", Retryable: false}}
				g.emitEvent(workflow, w, "tool_finished", result)
				encoded, _ := json.Marshal(result)
				g.appendToolReply(session, &messages, toolCall.ID, name, string(encoded))
				continue
			}
			call := Call{SchemaVersion: callSchemaVersion, RequestID: uuid.NewString(), Tool: name, ToolVersion: version, Input: input, Context: Context{SessionID: session.ID, WorkflowID: workflow.ID, UIContextRevision: workspaceRevision(session.Workspace), Workspace: session.Workspace}}
			g.emitEvent(workflow, w, "tool_started", map[string]any{"tool": name, "request_id": call.RequestID})
			result := g.executeSkillTool(ctx, workflow, call)
			workflow.ToolCalls = append(workflow.ToolCalls, WorkflowToolCall{Tool: name, Version: version, Status: workflowCallStatus(result), RequestID: call.RequestID, Input: call.Input, Data: result.Data})
			workflow.UpdatedAt = time.Now().UTC()
			_ = g.repo.upsertWorkflow(*workflow)
			g.emitEvent(workflow, w, "tool_finished", result)
			encoded, _ := json.Marshal(result)
			g.appendToolReply(session, &messages, toolCall.ID, name, string(encoded))
		}
	}
	workflow.Status = "failed"
	_ = g.repo.upsertWorkflow(*workflow)
	return fmt.Errorf("conversation exceeded tool step limit")
}

// appendToolReply keeps the provider conversation and durable session history
// in lockstep. Every assistant tool call must have a persisted matching tool
// reply, including validation and policy errors, so a later message can be
// reconstructed as a valid provider transcript.
func (g *Gateway) appendToolReply(session *Session, messages *[]ProviderMessage, toolCallID, tool, content string) {
	reply := ProviderMessage{Role: "tool", ToolCallID: toolCallID, Content: content}
	*messages = append(*messages, reply)
	session.Messages = append(session.Messages, SessionMessage{ID: uuid.NewString(), Role: "tool", Content: content, CreatedAt: time.Now().UTC(), Tool: tool, ToolCallID: toolCallID})
	_ = g.repo.upsertSession(*session)
}

func workflowCallStatus(result Result) string {
	if result.Status != "ok" {
		return result.Status
	}
	if data, ok := result.Data.(map[string]any); ok {
		if status, ok := data["status"].(string); ok {
			switch strings.ToLower(status) {
			case "queued", "pending", "running":
				return "pending"
			case "failed", "cancelled":
				return "error"
			}
		}
	}
	return "ok"
}

func splitToolName(name string) (string, string) {
	parts := strings.Split(name, "@")
	if len(parts) == 2 {
		return parts[0], parts[1]
	}
	return name, ""
}
func workspaceRevision(workspace map[string]any) string {
	if workspace == nil {
		return ""
	}
	if v, ok := workspace["revision"].(string); ok {
		return v
	}
	return ""
}
func (g *Gateway) sseHeaders(c *gin.Context) {
	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")
	c.Status(http.StatusOK)
}
func (g *Gateway) sendEvent(w io.Writer, typ string, data any) {
	encoded, _ := json.Marshal(data)
	_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", typ, encoded)
}

func (g *Gateway) writeWorkflowEvent(w io.Writer, event WorkflowEvent) {
	encoded, _ := json.Marshal(event.Data)
	_, _ = fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n", event.ID, event.Type, encoded)
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (g *Gateway) streamWorkflow(c *gin.Context, id string) {
	after := parseLastEventID(c)
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		workflow, ok := g.repo.getWorkflow(id)
		if !ok {
			return
		}
		for _, event := range workflow.Events {
			if event.ID > after {
				g.writeWorkflowEvent(c.Writer, event)
				after = event.ID
			}
		}
		if workflow.Status != "running" && workflow.Status != "queued" {
			return
		}
		select {
		case <-c.Request.Context().Done():
			return
		case <-g.closed:
			return
		case <-ticker.C:
		}
	}
}

func parseLastEventID(c *gin.Context) int64 {
	value := c.GetHeader("Last-Event-ID")
	if value == "" {
		value = c.Query("after")
	}
	parsed, _ := strconv.ParseInt(value, 10, 64)
	return parsed
}

func (g *Gateway) findWorkflowByRequest(sessionID, requestID string) *Workflow {
	for _, candidate := range g.repo.listWorkflows() {
		if candidate.SessionID == sessionID && candidate.RequestID == requestID {
			copy := candidate
			return &copy
		}
	}
	return nil
}

func messageFingerprint(text string) string {
	return fmt.Sprintf("%x", sha256.Sum256([]byte(strings.TrimSpace(text))))
}

func workflowTextFingerprint(workflow *Workflow) string {
	for _, event := range workflow.Events {
		if event.Type != "message" {
			continue
		}
		if data, ok := event.Data.(map[string]any); ok {
			if text, ok := data["text"].(string); ok {
				return messageFingerprint(text)
			}
		}
	}
	return ""
}

func selectSkill(text string) string {
	lower := strings.ToLower(text)
	if strings.Contains(lower, "变化") || strings.Contains(lower, "检测") || strings.Contains(lower, "巡检") || strings.Contains(lower, "比较") {
		return "change.detect.v1"
	}
	if strings.Contains(lower, "导出") || strings.Contains(lower, "结果") || strings.Contains(lower, "图斑") || strings.Contains(lower, "上次") || strings.Contains(lower, "筛选") {
		return "result.inspect.v1"
	}
	if strings.Contains(lower, "查") || strings.Contains(lower, "影像") || strings.Contains(lower, "可用") || strings.Contains(lower, "浏览") {
		return "imagery.explore.v1"
	}
	return "change.detect.v1"
}

func (g *Gateway) emitEvent(workflow *Workflow, w io.Writer, typ string, data any) {
	next := int64(len(workflow.Events) + 1)
	event := WorkflowEvent{ID: next, Type: typ, Data: data, CreatedAt: time.Now().UTC()}
	workflow.Events = append(workflow.Events, event)
	workflow.UpdatedAt = event.CreatedAt
	_ = g.repo.upsertWorkflow(*workflow)
	encoded, _ := json.Marshal(data)
	_, _ = fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n", event.ID, typ, encoded)
}

func (g *Gateway) getWorkflow(c *gin.Context) {
	workflow, ok := g.repo.getWorkflow(c.Param("id"))
	if !ok {
		if g.domainWorkflows != nil {
			value, err := g.domainWorkflows.GetWorkflow(c.Request.Context(), c.Param("id"))
			if err == nil {
				c.JSON(http.StatusOK, value)
				return
			}
		}
		c.JSON(404, gin.H{"error": "workflow not found"})
		return
	}
	c.JSON(http.StatusOK, workflow)
}
func (g *Gateway) workflowEvents(c *gin.Context) {
	workflow, ok := g.repo.getWorkflow(c.Param("id"))
	if !ok {
		if g.domainWorkflows != nil {
			if strings.Contains(c.GetHeader("Accept"), "text/event-stream") {
				g.streamDomainWorkflow(c, c.Param("id"))
				return
			}
			after := parseLastEventID(c)
			values, err := g.domainWorkflows.GetEvents(c.Request.Context(), c.Param("id"), after)
			if err == nil {
				c.JSON(http.StatusOK, gin.H{"events": values})
				return
			}
		}
		c.JSON(404, gin.H{"error": "workflow not found"})
		return
	}
	if strings.Contains(c.GetHeader("Accept"), "text/event-stream") {
		g.sseHeaders(c)
		g.streamWorkflow(c, workflow.ID)
		return
	}
	after := parseLastEventID(c)
	events := make([]WorkflowEvent, 0, len(workflow.Events))
	for _, event := range workflow.Events {
		if event.ID > after {
			events = append(events, event)
		}
	}
	c.JSON(http.StatusOK, gin.H{"events": events})
}

const domainWorkflowPollInterval = 250 * time.Millisecond
const domainWorkflowMaxPollInterval = 2 * time.Second

// streamDomainWorkflow adapts the durable domain workflow provider to the
// gateway's SSE contract. Domain workflows expose a current snapshot rather
// than chat-style events, so unchanged snapshots are suppressed while the
// client remains connected. The provider is polled on a bounded ticker to
// avoid a tight loop, and both request cancellation and Gateway.Close stop
// the stream promptly.
func (g *Gateway) streamDomainWorkflow(c *gin.Context, id string) {
	after := parseLastEventID(c)
	lastFingerprint := ""
	ctx, cancel := context.WithCancel(c.Request.Context())
	defer cancel()
	go func() {
		select {
		case <-g.closed:
			cancel()
		case <-ctx.Done():
		}
	}()
	started := false
	pollInterval := domainWorkflowPollInterval

	for {
		if ctx.Err() != nil {
			return
		}
		events, err := g.domainWorkflows.GetEvents(ctx, id, after)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			if !started {
				c.JSON(http.StatusNotFound, gin.H{"error": "workflow not found"})
				return
			}
			g.sendEvent(c.Writer, "error", map[string]any{"code": "WORKFLOW_EVENTS_FAILED", "message": "workflow events are unavailable"})
			c.Writer.Flush()
			return
		}
		if !started {
			g.sseHeaders(c)
			started = true
		}

		changed := false
		terminal := false
		for _, event := range events {
			eventID := domainEventID(event, after+1)
			data := event["data"]
			if data == nil {
				data = event
			}
			if workflowStatusTerminal(data) {
				terminal = true
			}
			// Last-Event-ID is exclusive. Keep the cursor stable for an unchanged
			// snapshot so the next real state change receives the next event ID.
			if eventID <= after {
				continue
			}
			fingerprint := domainEventFingerprint(event)
			if fingerprint == lastFingerprint {
				continue
			}
			writeDomainWorkflowEvent(c.Writer, event, eventID, data)
			lastFingerprint = fingerprint
			after = eventID
			changed = true
		}
		if terminal {
			return
		}
		if changed {
			pollInterval = domainWorkflowPollInterval
		} else if pollInterval < domainWorkflowMaxPollInterval {
			pollInterval *= 2
			if pollInterval > domainWorkflowMaxPollInterval {
				pollInterval = domainWorkflowMaxPollInterval
			}
		}
		timer := time.NewTimer(pollInterval)

		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
			continue
		}
	}
}

func writeDomainWorkflowEvent(w io.Writer, event map[string]any, id int64, data any) {
	typ, _ := event["type"].(string)
	if typ == "" {
		typ = "workflow"
	}
	encoded, _ := json.Marshal(data)
	_, _ = fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n", id, typ, encoded)
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
}

func domainEventID(event map[string]any, fallback int64) int64 {
	switch value := event["id"].(type) {
	case int:
		return int64(value)
	case int64:
		return value
	case float64:
		return int64(value)
	case json.Number:
		if parsed, err := strconv.ParseInt(string(value), 10, 64); err == nil {
			return parsed
		}
	case string:
		if parsed, err := strconv.ParseInt(value, 10, 64); err == nil {
			return parsed
		}
	}
	return fallback
}

func domainEventFingerprint(event map[string]any) string {
	value := struct {
		Type string `json:"type"`
		Data any    `json:"data"`
	}{Type: stringValue(event["type"]), Data: event["data"]}
	encoded, _ := json.Marshal(value)
	return string(encoded)
}

func workflowStatusTerminal(value any) bool {
	status := ""
	if object, ok := value.(map[string]any); ok {
		status = stringValue(object["status"])
	}
	switch strings.ToLower(status) {
	case "succeeded", "failed", "cancelled", "partial_failed":
		return true
	default:
		return false
	}
}

func (g *Gateway) cancelWorkflow(c *gin.Context) {
	workflow, ok := g.repo.getWorkflow(c.Param("id"))
	if !ok {
		if g.domainWorkflows != nil {
			err := g.domainWorkflows.CancelWorkflow(c.Request.Context(), c.Param("id"))
			if err == nil {
				if value, getErr := g.domainWorkflows.GetWorkflow(c.Request.Context(), c.Param("id")); getErr == nil {
					c.JSON(http.StatusOK, value)
				} else {
					c.JSON(http.StatusOK, gin.H{"id": c.Param("id"), "status": "cancel_requested"})
				}
				return
			}
			code := ""
			if coded, ok := err.(interface{ Code() string }); ok {
				code = coded.Code()
			}
			message := strings.ToLower(err.Error())
			if code == "WORKFLOW_NOT_FOUND" || strings.Contains(message, "not found") {
				c.JSON(http.StatusNotFound, gin.H{"error": "workflow not found"})
			} else if code == "WORKFLOW_NOT_CANCELLABLE" || code == "WORKFLOW_FINISHED" || strings.Contains(message, "unsupported") || strings.Contains(message, "rejected") || strings.Contains(message, "cannot") {
				c.JSON(http.StatusConflict, gin.H{"error": "workflow cancellation was rejected"})
			} else {
				c.JSON(http.StatusInternalServerError, gin.H{"error": "workflow cancellation failed"})
			}
			return
		}
		c.JSON(404, gin.H{"error": "workflow not found"})
		return
	}
	if workflow.Status == "succeeded" || workflow.Status == "failed" || workflow.Status == "cancelled" {
		c.JSON(http.StatusConflict, gin.H{"error": "workflow already finished"})
		return
	}
	workflow.Status = "cancelled"
	g.runMu.Lock()
	if cancel := g.runs[workflow.ID]; cancel != nil {
		cancel()
	}
	g.runMu.Unlock()
	workflow.UpdatedAt = time.Now().UTC()
	if err := g.repo.upsertWorkflow(workflow); err != nil {
		c.JSON(500, gin.H{"error": "workflow persistence failed"})
		return
	}
	c.JSON(http.StatusOK, workflow)
}
