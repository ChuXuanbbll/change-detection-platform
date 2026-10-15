package ai

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
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
	group.POST("/ai/sessions/:id/close", g.closeSession)
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
	c.JSON(http.StatusOK, gin.H{"skills": g.skillCatalog()})
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

func (g *Gateway) closeSession(c *gin.Context) {
	g.requestMu.Lock()
	defer g.requestMu.Unlock()
	s, ok := g.repo.getSession(c.Param("id"))
	if !ok {
		c.JSON(http.StatusNotFound, gin.H{"error": "session not found"})
		return
	}
	if s.ClosedAt == nil {
		now := time.Now().UTC()
		s.ClosedAt, s.UpdatedAt = &now, now
		for id, action := range s.Actions {
			if action.Status == "pending" {
				action.Status = "cancelled"
				s.Actions[id] = action
			}
		}
		if err := g.repo.upsertSession(s); err != nil {
			c.JSON(500, gin.H{"error": "session persistence failed"})
			return
		}
	}
	for _, id := range s.WorkflowIDs {
		g.runMu.Lock()
		if cancel := g.runs[id]; cancel != nil {
			cancel()
		}
		g.runMu.Unlock()
		if _, err := g.repo.cancelChatWorkflow(id); err != nil {
			c.JSON(500, gin.H{"error": "workflow cancellation persistence failed"})
			return
		}
	}
	c.JSON(http.StatusOK, gin.H{"id": s.ID, "closed_at": s.ClosedAt, "status": "closed"})
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
	fingerprint := messageRequestFingerprint(request)
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
	if s.ClosedAt != nil {
		g.requestMu.Unlock()
		c.JSON(http.StatusConflict, gin.H{"code": "AI_SESSION_CLOSED", "error": "此对话已关闭，请开启新对话。"})
		return
	}
	if existing := g.findWorkflowByRequest(s.ID, request.RequestID); existing != nil {
		expected := fingerprint
		if !strings.HasPrefix(existing.RequestFingerprint, "v2:") {
			expected = messageFingerprint(request.Text)
		}
		if existing.RequestFingerprint != "" && existing.RequestFingerprint != expected {
			g.requestMu.Unlock()
			c.JSON(http.StatusConflict, gin.H{"error": "request_id was already used with different text"})
			return
		}
		if existing.RequestFingerprint == "" && workflowTextFingerprint(existing) != messageFingerprint(request.Text) {
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
		g.runMu.Lock()
		workerActive := g.runs[activeID] != nil
		g.runMu.Unlock()
		if active, exists := g.repo.getWorkflow(activeID); exists && (workerActive || active.Status == "running" || active.Status == "queued") {
			g.requestMu.Unlock()
			c.JSON(http.StatusConflict, gin.H{"error": "another message is still running for this session", "workflow_id": active.ID})
			return
		}
	}
	if request.Workspace != nil {
		s.Workspace = request.Workspace
	}
	s.UpdatedAt = time.Now().UTC()
	skillID := request.SkillID
	s.SkillID = skillID
	workflow := Workflow{ID: uuid.NewString(), SessionID: s.ID, SkillID: skillID, RequestID: request.RequestID, RequestFingerprint: fingerprint, Status: "running", CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
	s.Messages = append(s.Messages, SessionMessage{ID: uuid.NewString(), Role: "user", Content: request.Text, CreatedAt: time.Now().UTC(), WorkflowID: workflow.ID, Data: map[string]any{"request_id": request.RequestID}})
	workflow.Events = append(workflow.Events,
		WorkflowEvent{ID: 1, Type: "workflow_started", Data: map[string]any{"session_id": s.ID, "workflow_id": workflow.ID, "request_id": request.RequestID}, CreatedAt: workflow.CreatedAt},
		WorkflowEvent{ID: 2, Type: "message", Data: map[string]any{"text": request.Text}, CreatedAt: workflow.CreatedAt})
	s.WorkflowIDs = append(s.WorkflowIDs, workflow.ID)
	if err := g.repo.upsertWorkflow(workflow); err != nil {
		g.requestMu.Unlock()
		c.JSON(500, gin.H{"error": "workflow persistence failed"})
		return
	}
	if err := g.repo.upsertSession(s); err != nil {
		_, _ = g.repo.cancelChatWorkflow(workflow.ID)
		g.requestMu.Unlock()
		c.JSON(500, gin.H{"error": "session persistence failed"})
		return
	}
	runCtx, cancel := context.WithCancel(context.Background())
	g.runMu.Lock()
	g.runs[workflow.ID] = cancel
	g.runMu.Unlock()
	g.workers.Add(1)
	g.requestMu.Unlock()
	g.sseHeaders(c)
	go func() {
		defer g.workers.Done()
		defer cancel()
		defer func() { g.runMu.Lock(); delete(g.runs, workflow.ID); g.runMu.Unlock() }()
		runCtx = withRetryObserver(runCtx, func(notice RetryNotice) { g.emitEvent(&workflow, io.Discard, "tool_progress", notice) })
		if err := g.runConversation(runCtx, &s, &workflow, request, io.Discard); err != nil {
			if runCtx.Err() != nil {
				g.completeCancelledTranscript(workflow.ID, s.ID)
			} else {
				g.completeToolTranscript(workflow.ID, s.ID, "AI_OPERATION_STOPPED", "编排已停止，此工具调用未执行或未确认完成。")
			}
			g.failConversation(runCtx, &workflow, err)
		}
	}()
	g.streamWorkflow(c, workflow.ID)
}

func (g *Gateway) runConversation(ctx context.Context, session *Session, workflow *Workflow, request MessageRequest, w io.Writer) error {
	intent, err := g.parseIntent(ctx, session, request)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	workflow.Intent = &intent
	if intent.NeedsClarification {
		session.PendingIntent = &intent
		session.Messages = append(session.Messages, SessionMessage{ID: uuid.NewString(), Role: "assistant", Content: intent.Question, WorkflowID: workflow.ID, Phase: "final", CreatedAt: time.Now().UTC()})
		if err := g.repo.upsertSession(*session); err != nil {
			return err
		}
		workflow.Status = "succeeded"
		return g.emitEvent(workflow, w, "final", map[string]any{"status": "needs_clarification", "message": intent.Question})
	}
	session.PendingIntent = nil
	if err := g.repo.upsertSession(*session); err != nil {
		return err
	}
	results := make([]string, 0, len(intent.Operations))
	for index, operation := range intent.Operations {
		if err := ctx.Err(); err != nil {
			return err
		}
		workflow.SkillID, workflow.OperationIndex, workflow.StepsUsed = operation.SkillID, index, 0
		bound, bindErr := bindPreviousOperationRefs(workflow, index, operation)
		if bindErr != nil {
			return g.handleExecutionClarification(ctx, session, workflow, intent, index, bindErr, w)
		}
		operation = bound
		intent.Operations[index] = operation
		session.SkillID = operation.SkillID
		g.emitEvent(workflow, w, "plan", map[string]any{"message": fmt.Sprintf("执行步骤 %d/%d", index+1, len(intent.Operations)), "skill_id": operation.SkillID, "operation_index": index, "slots": operation.Slots})
		if err := g.runOperation(ctx, session, workflow, request, operation, index == len(intent.Operations)-1, w); err != nil {
			return g.handleExecutionClarification(ctx, session, workflow, intent, index, err, w)
		}
		if index < len(intent.Operations)-1 && !operationCompleted(workflow, operation) {
			return g.handleExecutionClarification(ctx, session, workflow, intent, index, &ExecutionClarification{Question: "当前步骤尚未产生所需结果，请补充条件后再继续后续操作。"}, w)
		}
		for i := len(session.Messages) - 1; i >= 0; i-- {
			message := session.Messages[i]
			if message.WorkflowID == workflow.ID && message.Role == "assistant" && (message.Provider == nil || len(message.Provider.ToolCalls) == 0) {
				if message.Content != "" {
					results = append(results, message.Content)
				}
				break
			}
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	workflow.Status = "succeeded"
	finalText := strings.Join(results, "\n\n")
	// Persist the same complete answer that live clients receive, including
	// composed operations, so a refresh never loses an earlier operation result.
	for i := len(session.Messages) - 1; i >= 0; i-- {
		if session.Messages[i].WorkflowID == workflow.ID && session.Messages[i].Phase == "final" {
			session.Messages[i].Content = finalText
			break
		}
	}
	if err := g.repo.upsertSession(*session); err != nil {
		return err
	}
	return g.emitEvent(workflow, w, "final", map[string]any{"status": "succeeded", "message": finalText})
}

func (g *Gateway) handleExecutionClarification(ctx context.Context, session *Session, workflow *Workflow, intent Intent, index int, err error, w io.Writer) error {
	var clarification *ExecutionClarification
	if !errors.As(err, &clarification) {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	pending := Intent{Operations: append([]IntentOperation(nil), intent.Operations[index:]...), NeedsClarification: true, Question: clarification.Question}
	session.PendingIntent = &pending
	session.Messages = append(session.Messages, SessionMessage{ID: uuid.NewString(), Role: "assistant", Content: clarification.Question, WorkflowID: workflow.ID, Phase: "final", CreatedAt: time.Now().UTC()})
	if err := g.repo.upsertSession(*session); err != nil {
		return err
	}
	workflow.Status = "succeeded"
	return g.emitEvent(workflow, w, "final", map[string]any{"status": "needs_clarification", "message": clarification.Question})
}

func (g *Gateway) runOperation(ctx context.Context, session *Session, workflow *Workflow, request MessageRequest, operation IntentOperation, lastOperation bool, w io.Writer) error {
	g.requestMu.Lock()
	config, provider, override := g.config, g.provider, g.providerOverride
	g.requestMu.Unlock()
	if !config.Configured() && !override {
		return &ProviderError{Code: "AI_NOT_CONFIGURED", Message: "AI 服务尚未配置，请检查管理页中的服务地址、密钥和模型。"}
	}
	allowedTools := []string{}
	if skill, ok := g.skills[workflow.SkillID]; ok {
		allowedTools = skill.AllowedTools
	} else {
		for _, spec := range g.registry.Specs() {
			allowedTools = append(allowedTools, spec.Name)
		}
	}
	if workflow.SkillID == "result.inspect.v1" && stringValue(operation.Slots["operation"]) != "export" && request.SkillID == "" {
		filtered := make([]string, 0, len(allowedTools))
		for _, tool := range allowedTools {
			if tool != "result.export.prepare" {
				filtered = append(filtered, tool)
			}
		}
		allowedTools = filtered
	}
	allowedSet := make(map[string]bool, len(allowedTools))
	for _, name := range allowedTools {
		allowedSet[name] = true
	}
	tools, err := g.registry.ProviderTools(allowedTools)
	if err != nil {
		return fmt.Errorf("build provider tools: %w", err)
	}
	tools = append(tools, clarificationProviderTool())
	systemContent := promptText("execution")
	operationContext, _ := json.Marshal(map[string]any{"operation": operation, "current_workspace": session.Workspace, "operation_index": workflow.OperationIndex})
	systemContent += "\nNormalized operation: " + string(operationContext)
	if skill, ok := g.skills[workflow.SkillID]; ok {
		systemContent += "\n" + skill.Body
	}
	tools = append(tools, skillReferenceTool())
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
	failedUI := make(map[string]bool)
	for step := 0; step <= maxSteps; step++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		response, err := provider.Complete(ctx, CompletionRequest{Messages: messages, Tools: tools, Stage: "execution"})
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if len(response.Message.ToolCalls) == 0 && operation.SkillID == "change.detect.v1" && !operationCompleted(workflow, operation) {
			return fmt.Errorf("检测尚未提交，不能标记为执行成功")
		}
		for _, call := range response.Message.ToolCalls {
			if call.Function.Name == "request_clarification" {
				return decodeExecutionClarification(call)
			}
		}
		if response.Message.Content != "" || len(response.Message.ToolCalls) > 0 {
			phase := "intermediate"
			if len(response.Message.ToolCalls) == 0 && lastOperation {
				phase = "final"
			}
			session.Messages = append(session.Messages, SessionMessage{ID: uuid.NewString(), Role: "assistant", Content: response.Message.Content, CreatedAt: time.Now().UTC(), Provider: &response.Message, WorkflowID: workflow.ID, Phase: phase})
			if err := g.repo.upsertSession(*session); err != nil {
				return err
			}
			if response.Message.Content != "" {
				g.emitEvent(workflow, w, "text_delta", map[string]any{"text": response.Message.Content})
			}
		}
		if len(response.Message.ToolCalls) == 0 {
			return nil
		}
		messages = append(messages, response.Message)
		for _, toolCall := range response.Message.ToolCalls {
			if err := ctx.Err(); err != nil {
				return err
			}
			if workflow.StepsUsed >= maxSteps {
				name := internalToolName(toolCall.Function.Name)
				result := Result{SchemaVersion: schemaVersion, RequestID: uuid.NewString(), Tool: name, ToolVersion: "1.0.0", Status: "error", Error: &ToolError{Code: "SKILL_STEP_LIMIT", Message: "Skill step limit has been reached", Retryable: false}}
				g.emitEvent(workflow, w, "tool_finished", result)
				encoded, _ := json.Marshal(result)
				g.appendToolReply(session, &messages, toolCall.ID, name, string(encoded))
				return fmt.Errorf("SKILL_STEP_LIMIT: conversation exceeded tool step limit")
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
			requestedTool, _ := splitToolName(toolCall.Function.Name)
			if toolCall.Function.Name == "skill.read_reference" {
				result := g.readSkillReference(workflow.SkillID, input)
				g.emitEvent(workflow, w, "tool_finished", result)
				encoded, _ := json.Marshal(result)
				g.appendToolReply(session, &messages, toolCall.ID, result.Tool, string(encoded))
				continue
			}
			if !allowedSet[requestedTool] {
				result := Result{SchemaVersion: schemaVersion, RequestID: uuid.NewString(), Tool: requestedTool, ToolVersion: "1.0.0", Status: "error", Error: &ToolError{Code: "SKILL_TOOL_NOT_ALLOWED", Message: "tool is not allowed for this operation", Retryable: false}}
				g.emitEvent(workflow, w, "tool_finished", result)
				encoded, _ := json.Marshal(result)
				g.appendToolReply(session, &messages, toolCall.ID, requestedTool, string(encoded))
				continue
			}
			mismatch := false
			for _, key := range []string{"task_id", "result_id"} {
				if boundID := stringValue(operation.Slots[key]); boundID != "" && stringValue(input[key]) != "" && stringValue(input[key]) != boundID {
					mismatch = true
				}
			}
			if request.SkillID == "" && (requestedTool == "selection.validate" || requestedTool == "detection.plan.create") && input["aoi"] != nil && !operationAOIGrounded(workflow, operation, session.Workspace, input) {
				mismatch = true
			}
			if mismatch {
				result := Result{SchemaVersion: schemaVersion, RequestID: uuid.NewString(), Tool: requestedTool, ToolVersion: "1.0.0", Status: "error", Error: &ToolError{Code: "INTENT_RESOURCE_MISMATCH", Message: "工具请求与当前指令指定的任务或结果不一致，请使用当前操作绑定的资源。", Retryable: false}}
				g.emitEvent(workflow, w, "tool_finished", result)
				encoded, _ := json.Marshal(result)
				g.appendToolReply(session, &messages, toolCall.ID, requestedTool, string(encoded))
				continue
			}
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
				fingerprintBytes, _ := json.Marshal(map[string]any{"tool": toolCall.Function.Name, "input": input, "revision": workspaceRevision(session.Workspace)})
				fingerprint := string(fingerprintBytes)
				if failedUI[fingerprint] {
					return fmt.Errorf("UI_REPEAT_FAILURE: identical failed action requires corrected input or clarification")
				}
				if err := g.handleUICommand(ctx, session, workflow, w, toolCall, input); err != nil {
					result := Result{SchemaVersion: schemaVersion, RequestID: uuid.NewString(), Tool: toolCall.Function.Name, ToolVersion: "1.0.0", Status: "error", Data: map[string]any{"context_revision": workspaceRevision(session.Workspace), "workspace": scrubExternal(normalizeJSONValue(session.Workspace))}, Error: &ToolError{Code: "UI_ACTION_FAILED", Message: err.Error(), Retryable: false}}
					action := latestOperationAction(session, workflow, strings.TrimPrefix(toolCall.Function.Name, "ui."))
					if action.Ack != nil && action.Ack.Error != nil {
						copyError := *action.Ack.Error
						result.Error = &copyError
					}

					failedUI[fingerprint] = true
					workflow.ToolCalls = append(workflow.ToolCalls, WorkflowToolCall{Tool: toolCall.Function.Name, Version: "1.0.0", Status: "error", Input: input, Data: result.Data, OperationIndex: workflow.OperationIndex})
					_ = g.repo.upsertWorkflow(*workflow)
					g.emitEvent(workflow, w, "tool_finished", result)
					encoded, _ := json.Marshal(result)
					g.appendToolReply(session, &messages, toolCall.ID, toolCall.Function.Name, string(encoded))
					continue
				}
				action := latestOperationAction(session, workflow, strings.TrimPrefix(toolCall.Function.Name, "ui."))
				data := map[string]any{"status": "applied", "action_id": action.ID, "context_revision": workspaceRevision(session.Workspace), "workspace": scrubExternal(normalizeJSONValue(session.Workspace))}
				if action.Ack != nil && action.Ack.Result != nil {
					data["task_id"] = action.Ack.Result.TaskID
					data["pair_count"] = action.Ack.Result.PairCount
				}
				uiCall := WorkflowToolCall{Tool: toolCall.Function.Name, Version: "1.0.0", Status: "ok", RequestID: uuid.NewString(), Input: input, Data: data, OperationIndex: workflow.OperationIndex}
				workflow.ToolCalls = append(workflow.ToolCalls, uiCall)
				workflow.UpdatedAt = time.Now().UTC()
				_ = g.repo.upsertWorkflow(*workflow)
				uiResult := Result{SchemaVersion: schemaVersion, RequestID: uiCall.RequestID, Tool: uiCall.Tool, ToolVersion: uiCall.Version, Status: "ok", Data: data}
				if spec, exists := g.registry.Lookup(uiCall.Tool, uiCall.Version); exists {
					if err := validateJSONSchema(spec.OutputSchema, uiResult.Data); err != nil {
						uiResult.Status = "error"
						uiResult.Error = &ToolError{Code: "INVALID_TOOL_OUTPUT", Message: "UI acknowledgement does not satisfy its declared schema", Retryable: false}
					}
				}
				g.emitEvent(workflow, w, "tool_finished", uiResult)
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
			if err := ctx.Err(); err != nil {
				return err
			}
			workflow.ToolCalls = append(workflow.ToolCalls, WorkflowToolCall{Tool: name, Version: version, Status: workflowCallStatus(result), RequestID: call.RequestID, Input: call.Input, Data: result.Data, OperationIndex: workflow.OperationIndex})
			workflow.UpdatedAt = time.Now().UTC()
			_ = g.repo.upsertWorkflow(*workflow)
			g.emitEvent(workflow, w, "tool_finished", result)
			encoded, _ := json.Marshal(result)
			g.appendToolReply(session, &messages, toolCall.ID, name, string(encoded))
		}
	}
	return fmt.Errorf("conversation exceeded tool step limit")
}

func operationAOIGrounded(workflow *Workflow, operation IntentOperation, workspace, input map[string]any) bool {
	aoi := input["aoi"]
	if aoi == nil {
		return false
	}
	if operation.Slots["aoi"] != nil {
		return sameJSON(aoi, operation.Slots["aoi"])
	}
	if workspace["aoi"] != nil && sameJSON(aoi, workspace["aoi"]) {
		return true
	}
	for _, call := range workflow.ToolCalls {
		if call.OperationIndex != workflow.OperationIndex || call.Tool != "region.search" || call.Status != "ok" {
			continue
		}
		for _, row := range dataRows(call.Data, "matches") {
			match := mapValue(row)
			if supported, _ := match["aoi_supported"].(bool); supported && stringValue(match["region_id"]) == stringValue(input["region_id"]) && sameJSON(aoi, match["geometry"]) {
				return true
			}
		}
	}
	return false
}

// appendToolReply keeps the provider conversation and durable session history
// in lockstep. Every assistant tool call must have a persisted matching tool
// reply, including validation and policy errors, so a later message can be
// reconstructed as a valid provider transcript.
func (g *Gateway) appendToolReply(session *Session, messages *[]ProviderMessage, toolCallID, tool, content string) {
	reply := ProviderMessage{Role: "tool", ToolCallID: toolCallID, Content: content}
	*messages = append(*messages, reply)
	workflowID := ""
	for i := len(session.Messages) - 1; i >= 0; i-- {
		if session.Messages[i].WorkflowID != "" {
			workflowID = session.Messages[i].WorkflowID
			break
		}
	}
	session.Messages = append(session.Messages, SessionMessage{ID: uuid.NewString(), Role: "tool", Content: content, CreatedAt: time.Now().UTC(), Tool: tool, ToolCallID: toolCallID, WorkflowID: workflowID, Phase: "intermediate"})
	_ = g.repo.upsertSession(*session)
}

func (g *Gateway) completeCancelledTranscript(workflowID, sessionID string) {
	g.completeToolTranscript(workflowID, sessionID, "AI_CANCELLED", "AI 编排已停止，此工具调用未确认完成。")
}

func (g *Gateway) completeToolTranscript(workflowID, sessionID, code, errorMessage string) {
	// New messages remain blocked by runs until this cleanup has completed.
	// Pair unexecuted calls with explicit cancellation errors, never success.
	session, ok := g.repo.getSession(sessionID)
	if !ok {
		return
	}
	replied := make(map[string]bool)
	for _, message := range session.Messages {
		if message.Role == "tool" {
			replied[message.ToolCallID] = true
		}
	}
	missing := []SessionMessage{}
	for _, message := range session.Messages {
		if message.WorkflowID != workflowID || message.Provider == nil {
			continue
		}
		for _, call := range message.Provider.ToolCalls {
			if replied[call.ID] {
				continue
			}
			name := internalToolName(call.Function.Name)
			result := Result{SchemaVersion: schemaVersion, RequestID: uuid.NewString(), Tool: name, ToolVersion: "1.0.0", Status: "error", Error: &ToolError{Code: code, Message: errorMessage, Retryable: false}}
			encoded, _ := json.Marshal(result)
			missing = append(missing, SessionMessage{ID: uuid.NewString(), Role: "tool", ToolCallID: call.ID, Tool: name, Content: string(encoded), WorkflowID: workflowID, Phase: "intermediate", CreatedAt: time.Now().UTC()})
			replied[call.ID] = true
		}
	}
	if len(missing) > 0 {
		session.Messages = append(session.Messages, missing...)
		_ = g.repo.upsertSession(session)
	}
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

func messageRequestFingerprint(request MessageRequest) string {
	encoded, _ := json.Marshal(struct {
		Text      string
		Workspace map[string]any
		SkillID   string
	}{strings.TrimSpace(request.Text), request.Workspace, request.SkillID})
	return fmt.Sprintf("v2:%x", sha256.Sum256(encoded))
}

func (g *Gateway) failConversation(ctx context.Context, workflow *Workflow, err error) {
	if ctx.Err() != nil || errors.Is(err, context.Canceled) {
		_, _ = g.repo.cancelChatWorkflow(workflow.ID)
		return
	}
	workflow.Status = "failed"
	payload := map[string]any{"code": "AI_REQUEST_FAILED", "message": "AI 请求未能完成，请查看已创建任务或补充说明后继续。", "retryable": false}
	var providerError *ProviderError
	var intentError *IntentError
	if errors.As(err, &providerError) {
		payload["code"], payload["message"], payload["retryable"] = providerError.Code, providerError.Message, providerError.Retryable
		payload["http_status"], payload["request_id"], payload["attempts"], payload["stage"] = providerError.HTTPStatus, providerError.RequestID, providerError.Attempts, providerError.Stage
	} else if errors.As(err, &intentError) {
		payload["code"], payload["message"] = intentError.Code, intentError.Message
	}
	refs := []map[string]any{}
	for _, call := range workflow.ToolCalls {
		if call.Status == "ok" || call.Status == "pending" {
			for _, key := range []string{"task_id", "task_ids", "result_id", "workflow_id", "operation_id", "download_ref"} {
				if data, ok := call.Data.(map[string]any); ok && data[key] != nil {
					refs = append(refs, map[string]any{key: data[key]})
				}
			}
		}
	}
	if len(refs) > 0 {
		payload["refs"] = refs
	}
	log.Printf("[ai] workflow=%s code=%s stage=%v attempts=%v", workflow.ID, payload["code"], payload["stage"], payload["attempts"])
	if err := g.emitEvent(workflow, io.Discard, "error", payload); err != nil {
		log.Printf("[ai] workflow=%s code=AI_PERSISTENCE_FAILED", workflow.ID)
		g.repo.failWorkflowInMemory(workflow.ID)
	}
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

func (g *Gateway) emitEvent(workflow *Workflow, w io.Writer, typ string, data any) error {
	if stored, ok := g.repo.getWorkflow(workflow.ID); ok && stored.Status == "cancelled" {
		return nil
	}
	next := int64(len(workflow.Events) + 1)
	event := WorkflowEvent{ID: next, Type: typ, Data: data, CreatedAt: time.Now().UTC()}
	workflow.Events = append(workflow.Events, event)
	workflow.UpdatedAt = event.CreatedAt
	if err := g.repo.upsertWorkflow(*workflow); err != nil {
		workflow.Events = workflow.Events[:len(workflow.Events)-1]
		return fmt.Errorf("AI workflow persistence failed")
	}
	encoded, _ := json.Marshal(data)
	_, _ = fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n", event.ID, typ, encoded)
	return nil
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
	g.requestMu.Lock()
	defer g.requestMu.Unlock()
	g.runMu.Lock()
	if cancel := g.runs[workflow.ID]; cancel != nil {
		cancel()
	}
	g.runMu.Unlock()
	workflow, err := g.repo.cancelChatWorkflow(workflow.ID)
	if err != nil {
		c.JSON(500, gin.H{"error": "workflow persistence failed"})
		return
	}
	c.JSON(http.StatusOK, workflow)
}
