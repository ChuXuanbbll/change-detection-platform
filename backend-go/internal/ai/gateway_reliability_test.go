package ai

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

type reliabilityDomain struct {
	tools   atomic.Int32
	cancels atomic.Int32
}

func (d *reliabilityDomain) Execute(_ context.Context, call Call) Result {
	d.tools.Add(1)
	return Result{Status: "ok", Data: map[string]any{"revision": stringValue(call.Context.Workspace["revision"]), "domain_workflow_id": "submitted-detection"}}
}
func (d *reliabilityDomain) GetWorkflow(context.Context, string) (any, error) {
	return map[string]any{"status": "running"}, nil
}
func (d *reliabilityDomain) GetEvents(context.Context, string, int64) ([]map[string]any, error) {
	return nil, nil
}
func (d *reliabilityDomain) CancelWorkflow(context.Context, string) error {
	d.cancels.Add(1)
	return nil
}

func reliabilityGateway(t *testing.T, provider Provider) (*Gateway, *gin.Engine, Session, *reliabilityDomain) {
	t.Helper()
	domain := &reliabilityDomain{}
	spec := testToolSpecNamed("workspace.context")
	spec.RetryPolicy = map[string]any{"safe": true}
	gateway, err := NewGateway(t.TempDir(), []ToolSpec{spec}, domain)
	if err != nil {
		t.Fatal(err)
	}
	gateway.SetProvider(provider)
	t.Cleanup(gateway.Close)
	router := gin.New()
	gateway.Register(router.Group("/api"))
	created := reliabilityRequest(router, "POST", "/api/ai/sessions", "", "")
	var session Session
	if created.Code != http.StatusCreated || json.Unmarshal(created.Body.Bytes(), &session) != nil {
		t.Fatalf("create session: %d %s", created.Code, created.Body.String())
	}
	return gateway, router, session, domain
}

func reliabilityRequest(router http.Handler, method, path, body, cursor string) *httptest.ResponseRecorder {
	response := httptest.NewRecorder()
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	if cursor != "" {
		request.Header.Set("Last-Event-ID", cursor)
	}
	router.ServeHTTP(response, request)
	return response
}

func reliabilityBody(requestID, revision string) string {
	body, _ := json.Marshal(MessageRequest{Text: "查询当前工作区影像", RequestID: requestID, Workspace: map[string]any{"revision": revision}})
	return string(body)
}

func reliabilityWait(t *testing.T, channel <-chan struct{}, label string) {
	t.Helper()
	select {
	case <-channel:
	case <-time.After(3 * time.Second):
		t.Fatalf("timed out waiting for %s", label)
	}
}

func reliabilityWaitWorkers(t *testing.T, gateway *Gateway) {
	t.Helper()
	done := make(chan struct{})
	go func() { gateway.workers.Wait(); close(done) }()
	reliabilityWait(t, done, "worker completion")
}

type reliabilityBlockingProvider struct {
	started   chan struct{}
	release   chan struct{}
	cancelled chan struct{}
	ignore    bool
	calls     atomic.Int32
}

func (p *reliabilityBlockingProvider) Complete(ctx context.Context, request CompletionRequest) (CompletionResponse, error) {
	if request.Stage == "skill_selection" {
		return fixtureSkillSelection(request), nil
	}
	if request.Stage == "intent" {
		return gatewayTestIntentResponse(), nil
	}
	p.calls.Add(1)
	close(p.started)
	if p.ignore {
		<-p.release
		return CompletionResponse{Message: ProviderMessage{Role: "assistant", Content: "迟到的结果"}}, nil
	}
	select {
	case <-ctx.Done():
		close(p.cancelled)
		return CompletionResponse{}, ctx.Err()
	case <-p.release:
		return CompletionResponse{Message: ProviderMessage{Role: "assistant", Content: "已完成"}}, nil
	}
}

func TestGatewayCloseSessionIsIdempotentAndStopsOnlyChat(t *testing.T) {
	for _, ignore := range []bool{false, true} {
		t.Run(strconv.FormatBool(ignore), func(t *testing.T) {
			provider := &reliabilityBlockingProvider{started: make(chan struct{}), release: make(chan struct{}), cancelled: make(chan struct{}), ignore: ignore}
			gateway, router, session, domain := reliabilityGateway(t, provider)
			// Release even on a failed assertion so cleanup cannot wait forever.
			t.Cleanup(func() {
				select {
				case <-provider.release:
				default:
					close(provider.release)
				}
			})
			response := httptest.NewRecorder()
			done := make(chan struct{})
			go func() {
				router.ServeHTTP(response, httptest.NewRequest("POST", "/api/ai/sessions/"+session.ID+"/messages", strings.NewReader(reliabilityBody("closing-turn", "r1"))))
				close(done)
			}()
			reliabilityWait(t, provider.started, "provider request")
			stored, _ := gateway.repo.getSession(session.ID)
			workflowID := stored.WorkflowIDs[0]
			before, _ := gateway.repo.getWorkflow(workflowID)
			if len(before.Events) < 2 || before.Events[0].Type != "workflow_started" {
				t.Fatalf("initial events=%+v", before.Events)
			}
			firstClose := reliabilityRequest(router, "POST", "/api/ai/sessions/"+session.ID+"/close", "", "")
			secondClose := reliabilityRequest(router, "POST", "/api/ai/sessions/"+session.ID+"/close", "", "")
			if firstClose.Code != 200 || secondClose.Code != 200 || firstClose.Body.String() != secondClose.Body.String() {
				t.Fatalf("close not idempotent: first=%s second=%s", firstClose.Body.String(), secondClose.Body.String())
			}
			if ignore {
				close(provider.release)
			} else {
				reliabilityWait(t, provider.cancelled, "provider cancellation")
			}
			reliabilityWaitWorkers(t, gateway)
			reliabilityWait(t, done, "cancelled SSE stream")
			workflow, _ := gateway.repo.getWorkflow(workflowID)
			stored, _ = gateway.repo.getSession(session.ID)
			cancelEvents := 0
			for _, event := range workflow.Events {
				if event.Type == "cancelled" {
					cancelEvents++
				}
				if event.Type == "error" || event.Type == "final" {
					t.Fatalf("late terminal overwrote cancellation: %+v", workflow)
				}
			}
			if workflow.Status != "cancelled" || stored.ClosedAt == nil || cancelEvents != 1 || domain.cancels.Load() != 0 {
				t.Fatalf("workflow=%+v closed=%v domainCancels=%d", workflow, stored.ClosedAt, domain.cancels.Load())
			}
			for _, message := range stored.Messages {
				if message.Role == "assistant" {
					t.Fatalf("late provider result was persisted: %+v", message)
				}
			}
			rejected := reliabilityRequest(router, "POST", "/api/ai/sessions/"+session.ID+"/messages", reliabilityBody("another-turn", "r1"), "")
			if rejected.Code != http.StatusConflict || !strings.Contains(rejected.Body.String(), "AI_SESSION_CLOSED") || provider.calls.Load() != 1 {
				t.Fatalf("closed session accepted message: %d %s", rejected.Code, rejected.Body.String())
			}
		})
	}
}

func TestGatewayWorkflowStartedStreamsBeforeBlockedProviderFinishes(t *testing.T) {
	provider := &reliabilityBlockingProvider{started: make(chan struct{}), release: make(chan struct{}), cancelled: make(chan struct{})}
	gateway, router, session, _ := reliabilityGateway(t, provider)
	t.Cleanup(func() {
		select {
		case <-provider.release:
		default:
			close(provider.release)
		}
	})
	server := httptest.NewServer(router)
	defer server.Close()
	client := server.Client()
	client.Timeout = 3 * time.Second
	response, err := client.Post(server.URL+"/api/ai/sessions/"+session.ID+"/messages", "application/json", strings.NewReader(reliabilityBody("early-start", "r1")))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	reliabilityWait(t, provider.started, "blocked provider")
	reader := bufio.NewReader(response.Body)
	var firstEvent strings.Builder
	for {
		line, readErr := reader.ReadString('\n')
		if readErr != nil {
			t.Fatal(readErr)
		}
		firstEvent.WriteString(line)
		if line == "\n" {
			break
		}
	}
	if !strings.Contains(firstEvent.String(), "event: workflow_started") || !strings.Contains(firstEvent.String(), `"request_id":"early-start"`) || !strings.Contains(firstEvent.String(), session.ID) {
		t.Fatalf("first streamed event=%s", firstEvent.String())
	}
	closed := reliabilityRequest(router, "POST", "/api/ai/sessions/"+session.ID+"/close", "", "")
	if closed.Code != 200 {
		t.Fatalf("close=%s", closed.Body.String())
	}
	remaining, err := io.ReadAll(reader)
	if err != nil || !bytes.Contains(remaining, []byte("event: cancelled")) {
		t.Fatalf("remaining=%s err=%v", remaining, err)
	}
	reliabilityWaitWorkers(t, gateway)
}

type reliabilityScriptedProvider struct {
	intents    atomic.Int32
	executions atomic.Int32
}

func (p *reliabilityScriptedProvider) Complete(_ context.Context, request CompletionRequest) (CompletionResponse, error) {
	if request.Stage == "skill_selection" {
		return fixtureSkillSelection(request), nil
	}
	if request.Stage == "intent" {
		p.intents.Add(1)
		return gatewayTestIntentResponse(), nil
	}
	if p.executions.Add(1) == 1 {
		call := ProviderToolCall{ID: "workspace-call", Type: "function"}
		call.Function.Name, call.Function.Arguments = "workspace__context", "{}"
		return CompletionResponse{Message: ProviderMessage{Role: "assistant", Content: "正在检查工作区", ToolCalls: []ProviderToolCall{call}}}, nil
	}
	return CompletionResponse{Message: ProviderMessage{Role: "assistant", Content: "**完成**：影像已核实。"}}, nil
}

func TestGatewayReplayPreservesTurnFinalAndDoesNotExecuteAgain(t *testing.T) {
	provider := &reliabilityScriptedProvider{}
	gateway, router, session, domain := reliabilityGateway(t, provider)
	body := reliabilityBody("replay-turn", "r1")
	path := "/api/ai/sessions/" + session.ID + "/messages"
	response := reliabilityRequest(router, "POST", path, body, "")
	if response.Code != 200 || !strings.Contains(response.Body.String(), "event: final") {
		t.Fatalf("turn failed: %d %s", response.Code, response.Body.String())
	}
	reliabilityWaitWorkers(t, gateway)
	stored, _ := gateway.repo.getSession(session.ID)
	if len(stored.WorkflowIDs) != 1 {
		t.Fatalf("workflow IDs=%v", stored.WorkflowIDs)
	}
	workflow, _ := gateway.repo.getWorkflow(stored.WorkflowIDs[0])
	finalID := int64(0)
	for _, event := range workflow.Events {
		if event.Type == "final" {
			finalID = event.ID
			data, _ := event.Data.(map[string]any)
			if data["message"] != "**完成**：影像已核实。" {
				t.Fatalf("final data=%+v", data)
			}
		}
	}
	if finalID == 0 || workflow.Status != "succeeded" {
		t.Fatalf("workflow=%+v", workflow)
	}
	intermediate, final, tool := 0, 0, 0
	for _, message := range stored.Messages {
		if message.WorkflowID != workflow.ID {
			t.Fatalf("ungrouped message=%+v", message)
		}
		if message.Role == "assistant" && message.Phase == "intermediate" {
			intermediate++
		}
		if message.Role == "assistant" && message.Phase == "final" {
			final++
			if message.Content != "**完成**：影像已核实。" {
				t.Fatalf("persisted final=%q", message.Content)
			}
		}
		if message.Role == "tool" {
			tool++
		}
	}
	if intermediate != 1 || final != 1 || tool != 1 || domain.tools.Load() != 1 {
		t.Fatalf("phases=%d/%d tool=%d executions=%d", intermediate, final, tool, domain.tools.Load())
	}
	for _, cursor := range []string{"", strconv.FormatInt(finalID-1, 10), strconv.FormatInt(finalID, 10)} {
		replay := reliabilityRequest(router, "POST", path, body, cursor)
		if replay.Code != 200 {
			t.Fatalf("replay=%d %s", replay.Code, replay.Body.String())
		}
		if cursor == strconv.FormatInt(finalID, 10) && replay.Body.Len() != 0 {
			t.Fatalf("cursor replayed consumed events=%s", replay.Body.String())
		}
	}
	if domain.tools.Load() != 1 || provider.intents.Load() != 1 || provider.executions.Load() != 2 {
		t.Fatalf("replay re-executed: tools=%d intent=%d execution=%d", domain.tools.Load(), provider.intents.Load(), provider.executions.Load())
	}
	conflict := reliabilityRequest(router, "POST", path, reliabilityBody("replay-turn", "r2"), "")
	if conflict.Code != http.StatusConflict {
		t.Fatalf("changed workspace did not conflict: %d %s", conflict.Code, conflict.Body.String())
	}
}

func TestGatewayProviderRetryAfterToolDoesNotRepeatTool(t *testing.T) {
	var intentCalls, executionCalls atomic.Int32
	var afterToolBodies [][]byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var payload struct {
			Tools []ProviderTool `json:"tools"`
		}
		if err := json.Unmarshal(body, &payload); err != nil {
			t.Error(err)
		}
		if len(payload.Tools) > 0 && payload.Tools[0].Function.Name == "select_skills" {
			_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": ProviderMessage{Role: "assistant", Content: `{"skill_ids":["imagery.explore.v1"]}`}}}})
			return
		}
		if len(payload.Tools) > 0 && payload.Tools[0].Function.Name == "route_intent" {
			intentCalls.Add(1)
			_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": gatewayTestIntentResponse().Message}}})
			return
		}
		count := executionCalls.Add(1)
		if count == 1 {
			_, _ = io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"检查中","tool_calls":[{"id":"context-1","type":"function","function":{"name":"workspace__context","arguments":"{}"}}]}}]}`)
			return
		}
		afterToolBodies = append(afterToolBodies, body)
		if count == 2 {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = io.WriteString(w, `{"error":{"code":"overloaded"}}`)
			return
		}
		_, _ = io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"核实完成"}}]}`)
	}))
	defer server.Close()
	provider := providerWithoutWait(server.URL)
	gateway, router, session, domain := reliabilityGateway(t, provider)
	response := reliabilityRequest(router, "POST", "/api/ai/sessions/"+session.ID+"/messages", reliabilityBody("retry-after-tool", "r1"), "")
	reliabilityWaitWorkers(t, gateway)
	if response.Code != 200 || !strings.Contains(response.Body.String(), "event: final") || !strings.Contains(response.Body.String(), "event: tool_progress") {
		t.Fatalf("response=%d %s", response.Code, response.Body.String())
	}
	if domain.tools.Load() != 1 || intentCalls.Load() != 1 || executionCalls.Load() != 3 || len(afterToolBodies) != 2 || !bytes.Equal(afterToolBodies[0], afterToolBodies[1]) {
		t.Fatalf("tools=%d intent=%d execution=%d retryBodies=%d", domain.tools.Load(), intentCalls.Load(), executionCalls.Load(), len(afterToolBodies))
	}
	var payload struct {
		Messages []ProviderMessage `json:"messages"`
	}
	if err := json.Unmarshal(afterToolBodies[1], &payload); err != nil {
		t.Fatal(err)
	}
	pairs := 0
	for _, message := range payload.Messages {
		if message.Role == "tool" && message.ToolCallID == "context-1" {
			pairs++
		}
	}
	if pairs != 1 {
		t.Fatalf("tool reply count=%d", pairs)
	}
}

type reliabilityLateToolExecutor struct {
	started chan struct{}
	release chan struct{}
}

func (e *reliabilityLateToolExecutor) Execute(context.Context, Call) Result {
	close(e.started)
	<-e.release // Emulate a domain operation that finishes after cancellation.
	return Result{Status: "ok", Data: map[string]any{"revision": "late-old-revision"}}
}

type reliabilityCancelTranscriptProvider struct {
	requests []CompletionRequest
}

func (p *reliabilityCancelTranscriptProvider) Complete(_ context.Context, request CompletionRequest) (CompletionResponse, error) {
	if request.Stage == "skill_selection" {
		return fixtureSkillSelection(request), nil
	}
	if request.Stage == "intent" {
		return gatewayTestIntentResponse(), nil
	}
	p.requests = append(p.requests, cloneGatewayTranscriptRequest(request))
	if len(p.requests) == 1 {
		call := ProviderToolCall{ID: "cancelled-tool-call", Type: "function"}
		call.Function.Name, call.Function.Arguments = "workspace__context", "{}"
		return CompletionResponse{Message: ProviderMessage{Role: "assistant", ToolCalls: []ProviderToolCall{call}}}, nil
	}
	return CompletionResponse{Message: ProviderMessage{Role: "assistant", Content: "新任务结果"}}, nil
}

func TestGatewayCancelledWorkerMustExitBeforeNextTurnAndRepairsTranscript(t *testing.T) {
	provider := &reliabilityCancelTranscriptProvider{}
	gateway, router, session, domain := reliabilityGateway(t, provider)
	executor := &reliabilityLateToolExecutor{started: make(chan struct{}), release: make(chan struct{})}
	gateway.SetExecutor(executor)
	t.Cleanup(func() {
		select {
		case <-executor.release:
		default:
			close(executor.release)
		}
	})
	path := "/api/ai/sessions/" + session.ID + "/messages"
	response := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		router.ServeHTTP(response, httptest.NewRequest("POST", path, strings.NewReader(reliabilityBody("old-turn", "old-revision"))))
		close(done)
	}()
	reliabilityWait(t, executor.started, "in-flight old tool")
	stored, _ := gateway.repo.getSession(session.ID)
	oldWorkflowID := stored.WorkflowIDs[0]
	cancelled := reliabilityRequest(router, "POST", "/api/ai/workflows/"+oldWorkflowID+"/cancel", "", "")
	if cancelled.Code != 200 {
		t.Fatalf("cancel=%d %s", cancelled.Code, cancelled.Body.String())
	}
	reliabilityWait(t, done, "cancelled stream")
	tooEarly := reliabilityRequest(router, "POST", path, reliabilityBody("new-turn", "new-revision"), "")
	if tooEarly.Code != http.StatusConflict {
		t.Fatalf("live cancelled worker accepted another turn: %d %s", tooEarly.Code, tooEarly.Body.String())
	}
	stored, _ = gateway.repo.getSession(session.ID)
	if len(stored.WorkflowIDs) != 1 || stringValue(stored.Workspace["revision"]) != "old-revision" {
		t.Fatalf("rejected turn changed session=%+v", stored)
	}
	close(executor.release)
	reliabilityWaitWorkers(t, gateway)
	newResponse := reliabilityRequest(router, "POST", path, reliabilityBody("new-turn", "new-revision"), "")
	reliabilityWaitWorkers(t, gateway)
	if newResponse.Code != 200 || !strings.Contains(newResponse.Body.String(), "event: final") {
		t.Fatalf("new turn=%d %s", newResponse.Code, newResponse.Body.String())
	}
	stored, _ = gateway.repo.getSession(session.ID)
	if len(stored.WorkflowIDs) != 2 || stringValue(stored.Workspace["revision"]) != "new-revision" || domain.cancels.Load() != 0 {
		t.Fatalf("old worker overwrote new session=%+v", stored)
	}
	oldWorkflow, _ := gateway.repo.getWorkflow(oldWorkflowID)
	newWorkflow, _ := gateway.repo.getWorkflow(stored.WorkflowIDs[1])
	if oldWorkflow.Status != "cancelled" || newWorkflow.Status != "succeeded" {
		t.Fatalf("statuses old=%s new=%s", oldWorkflow.Status, newWorkflow.Status)
	}
	if len(provider.requests) != 2 {
		t.Fatalf("execution requests=%d", len(provider.requests))
	}
	callCount, replyCount, userCount := 0, 0, 0
	for _, message := range provider.requests[1].Messages {
		if message.Role == "user" {
			userCount++
		}
		for _, call := range message.ToolCalls {
			if call.ID == "cancelled-tool-call" {
				callCount++
			}
		}
		if message.Role == "tool" && message.ToolCallID == "cancelled-tool-call" {
			replyCount++
			var result Result
			if json.Unmarshal([]byte(message.Content), &result) != nil || result.Status != "error" || result.Error == nil || result.Error.Code != "AI_CANCELLED" {
				t.Fatalf("cancelled call was not safely paired: %s", message.Content)
			}
		}
	}
	if callCount != 0 || replyCount != 0 || userCount != 2 {
		t.Fatalf("transcript calls/replies/users=%d/%d/%d", callCount, replyCount, userCount)
	}
	// Cancellation still repairs the persisted audit transcript; a new turn
	// must not reuse that old evidence as current workspace state.
	persistedReply := false
	for _, message := range stored.Messages {
		if message.Role == "tool" && message.ToolCallID == "cancelled-tool-call" {
			var result Result
			if json.Unmarshal([]byte(message.Content), &result) != nil || result.Error == nil || result.Error.Code != "AI_CANCELLED" {
				t.Fatalf("cancelled call lost its persisted error: %s", message.Content)
			}
			persistedReply = true
		}
	}
	if !persistedReply {
		t.Fatal("cancelled call was not repaired in storage")
	}
	finalCount := 0
	for _, message := range stored.Messages {
		if message.Phase == "final" && message.Content == "新任务结果" {
			finalCount++
		}
	}
	if finalCount != 1 {
		t.Fatalf("new task history lost=%+v", stored.Messages)
	}
}

type reliabilityHookProvider struct {
	onExecution func()
}

func (p *reliabilityHookProvider) Complete(_ context.Context, request CompletionRequest) (CompletionResponse, error) {
	if request.Stage == "skill_selection" {
		return fixtureSkillSelection(request), nil
	}
	if request.Stage == "intent" {
		return gatewayTestIntentResponse(), nil
	}
	if p.onExecution != nil {
		p.onExecution()
	}
	return CompletionResponse{Message: ProviderMessage{Role: "assistant", Content: "已完成"}}, nil
}

func TestGatewayTerminalPersistenceFailureStopsStreamWithoutFakeFinal(t *testing.T) {
	provider := &reliabilityHookProvider{}
	gateway, router, session, _ := reliabilityGateway(t, provider)
	// Fail only workflow saves after the initial request has been accepted.
	// Session saves remain available, so the failure exercises final emission.
	blockedTempPath := filepath.Join(gateway.repo.dir, ".workflows.json.tmp")
	provider.onExecution = func() {
		if err := os.Mkdir(blockedTempPath, 0700); err != nil {
			t.Errorf("inject persistence failure: %v", err)
		}
		provider.onExecution = nil
	}
	path := "/api/ai/sessions/" + session.ID + "/messages"
	body := reliabilityBody("failed-persistence", "r1")
	response := reliabilityRequest(router, "POST", path, body, "")
	reliabilityWaitWorkers(t, gateway)
	if response.Code != 200 || !strings.Contains(response.Body.String(), "AI_PERSISTENCE_FAILED") || strings.Contains(response.Body.String(), "event: final") {
		t.Fatalf("persistence failure stream=%d %s", response.Code, response.Body.String())
	}
	stored, _ := gateway.repo.getSession(session.ID)
	workflow, _ := gateway.repo.getWorkflow(stored.WorkflowIDs[0])
	if workflow.Status != "failed" || len(workflow.Events) == 0 || workflow.Events[len(workflow.Events)-1].Type != "error" {
		t.Fatalf("persist failure left workflow running=%+v", workflow)
	}
	for _, event := range workflow.Events {
		if event.Type == "final" {
			t.Fatal("unpersisted final was reported as success")
		}
	}
	replay := reliabilityRequest(router, "POST", path, body, "")
	if !strings.Contains(replay.Body.String(), "AI_PERSISTENCE_FAILED") {
		t.Fatalf("failure replay=%s", replay.Body.String())
	}
	if err := os.Remove(blockedTempPath); err != nil {
		t.Fatal(err)
	}
	recovered := reliabilityRequest(router, "POST", path, reliabilityBody("after-storage-recovery", "r2"), "")
	if recovered.Code != 200 || !strings.Contains(recovered.Body.String(), "event: final") {
		t.Fatalf("storage recovery blocked by stale running workflow: %d %s", recovered.Code, recovered.Body.String())
	}
}

type reliabilityLoopProvider struct{ attempts int }

func (p *reliabilityLoopProvider) Complete(_ context.Context, request CompletionRequest) (CompletionResponse, error) {
	if request.Stage == "skill_selection" {
		return fixtureSkillSelection(request), nil
	}
	if request.Stage == "intent" {
		return gatewayTestIntentResponse(), nil
	}
	p.attempts++
	call := ProviderToolCall{ID: "loop-" + strconv.Itoa(p.attempts), Type: "function"}
	call.Function.Name, call.Function.Arguments = "workspace__context", "{}"
	return CompletionResponse{Message: ProviderMessage{Role: "assistant", ToolCalls: []ProviderToolCall{call}}}, nil
}

func TestGatewayStepLimitPublishesErrorBeforeTerminalStreamCloses(t *testing.T) {
	provider := &reliabilityLoopProvider{}
	gateway, router, session, domain := reliabilityGateway(t, provider)
	skill := gateway.skills["imagery.explore.v1"]
	skill.MaxSteps = 1
	gateway.skills[skill.ID] = skill
	response := reliabilityRequest(router, "POST", "/api/ai/sessions/"+session.ID+"/messages", reliabilityBody("step-limit", "r1"), "")
	reliabilityWaitWorkers(t, gateway)
	if response.Code != 200 || !strings.Contains(response.Body.String(), "SKILL_STEP_LIMIT") || !strings.Contains(response.Body.String(), "event: error") || strings.Contains(response.Body.String(), "event: final") {
		t.Fatalf("step-limit stream omitted error=%d %s", response.Code, response.Body.String())
	}
	stored, _ := gateway.repo.getSession(session.ID)
	workflow, _ := gateway.repo.getWorkflow(stored.WorkflowIDs[0])
	if workflow.Status != "failed" || len(workflow.Events) == 0 || workflow.Events[len(workflow.Events)-1].Type != "error" || domain.tools.Load() != 1 {
		t.Fatalf("step-limit terminal=%+v executions=%d", workflow, domain.tools.Load())
	}
}

func TestGatewayLegacyRequestWithoutFingerprintReplaysByText(t *testing.T) {
	provider := &reliabilityScriptedProvider{}
	gateway, router, session, domain := reliabilityGateway(t, provider)
	session.WorkflowIDs = []string{"legacy-workflow"}
	if err := gateway.repo.upsertSession(session); err != nil {
		t.Fatal(err)
	}
	workflow := Workflow{ID: "legacy-workflow", SessionID: session.ID, RequestID: "legacy-turn", Status: "succeeded", Events: []WorkflowEvent{
		{ID: 1, Type: "message", Data: map[string]any{"text": "查询当前工作区影像"}},
		{ID: 2, Type: "final", Data: map[string]any{"status": "succeeded", "message": "旧版已完成"}},
	}}
	if err := gateway.repo.upsertWorkflow(workflow); err != nil {
		t.Fatal(err)
	}
	path := "/api/ai/sessions/" + session.ID + "/messages"
	replayed := reliabilityRequest(router, "POST", path, reliabilityBody("legacy-turn", "r1"), "1")
	if replayed.Code != 200 || !strings.Contains(replayed.Body.String(), "旧版已完成") || strings.Contains(replayed.Body.String(), "event: message") || provider.intents.Load() != 0 || domain.tools.Load() != 0 {
		t.Fatalf("legacy replay=%d %s", replayed.Code, replayed.Body.String())
	}
	conflict := reliabilityRequest(router, "POST", path, `{"text":"不同内容","request_id":"legacy-turn"}`, "")
	if conflict.Code != http.StatusConflict {
		t.Fatalf("legacy changed text=%d %s", conflict.Code, conflict.Body.String())
	}
}
