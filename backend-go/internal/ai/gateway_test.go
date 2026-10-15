package ai

import (
	"context"
	"encoding/json"
	"io"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

type domainWorkflowStub struct {
	mu     sync.Mutex
	calls  int
	events func(context.Context, string, int64) ([]map[string]any, error)
}

func (s *domainWorkflowStub) Execute(context.Context, Call) Result {
	return Result{Status: "ok", Data: map[string]any{}}
}

func (s *domainWorkflowStub) GetWorkflow(context.Context, string) (any, error) {
	return map[string]any{"status": "running"}, nil
}

func (s *domainWorkflowStub) GetEvents(ctx context.Context, id string, after int64) ([]map[string]any, error) {
	if s.events != nil {
		return s.events(ctx, id, after)
	}
	s.mu.Lock()
	s.calls++
	calls := s.calls
	s.mu.Unlock()
	if after >= 1 || calls > 1 {
		return []map[string]any{{"id": 2, "type": "workflow", "data": map[string]any{"status": "succeeded", "progress": 100}}}, nil
	}
	return []map[string]any{{"id": 1, "type": "workflow", "data": map[string]any{"status": "running", "progress": 50}}}, nil
}

func (s *domainWorkflowStub) CancelWorkflow(context.Context, string) error { return nil }

type transcriptProvider struct {
	mu       sync.Mutex
	calls    int
	requests []CompletionRequest
}

func gatewayTestIntentResponse() CompletionResponse {
	return CompletionResponse{Message: ProviderMessage{Role: "assistant", Content: `{"operations":[{"skill_id":"imagery.explore.v1","slots":{"operation":"query"}}],"needs_clarification":false,"question":""}`}}
}

func (p *transcriptProvider) Complete(_ context.Context, request CompletionRequest) (CompletionResponse, error) {
	if request.Stage == "skill_selection" {
		return fixtureSkillSelection(request), nil
	}
	if request.Stage == "intent" {
		return gatewayTestIntentResponse(), nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls++
	p.requests = append(p.requests, request)
	if p.calls == 1 {
		var toolCall ProviderToolCall
		toolCall.ID = "malformed-1"
		toolCall.Function.Name = "ui.select_region"
		toolCall.Function.Arguments = "{" // force the gateway's invalid-arguments path
		return CompletionResponse{Message: ProviderMessage{Role: "assistant", ToolCalls: []ProviderToolCall{toolCall}}}, nil
	}
	return CompletionResponse{Message: ProviderMessage{Role: "assistant", Content: "已处理"}}, nil
}

type scriptedProvider struct{ calls int }

func (p *scriptedProvider) Complete(_ context.Context, request CompletionRequest) (CompletionResponse, error) {
	if request.Stage == "skill_selection" {
		return fixtureSkillSelection(request), nil
	}
	if request.Stage == "intent" {
		return gatewayTestIntentResponse(), nil
	}
	p.calls++
	return CompletionResponse{Message: ProviderMessage{Role: "assistant", Content: "已完成"}}, nil
}

func TestGatewayMessagePersistsHistoryAndReplaysEvents(t *testing.T) {
	oldBase, oldKey, oldModel := os.Getenv("AI_API_BASE"), os.Getenv("AI_API_KEY"), os.Getenv("AI_MODEL")
	defer func() {
		_ = os.Setenv("AI_API_BASE", oldBase)
		_ = os.Setenv("AI_API_KEY", oldKey)
		_ = os.Setenv("AI_MODEL", oldModel)
	}()
	_ = os.Setenv("AI_API_BASE", "http://configured")
	_ = os.Setenv("AI_API_KEY", "key")
	_ = os.Setenv("AI_MODEL", "model")
	g, err := NewGateway(t.TempDir(), []ToolSpec{testSpec()}, testExecutor{})
	if err != nil {
		t.Fatal(err)
	}
	p := &scriptedProvider{}
	g.SetProvider(p)
	r := gin.New()
	group := r.Group("/api")
	g.Register(group)
	create := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/ai/sessions", nil)
	r.ServeHTTP(create, req)
	if create.Code != 201 {
		t.Fatalf("create status %d", create.Code)
	}
	var session Session
	if err := json.Unmarshal(create.Body.Bytes(), &session); err != nil {
		t.Fatal(err)
	}
	body := strings.NewReader(`{"text":"查影像","request_id":"req-1"}`)
	message := httptest.NewRecorder()
	r.ServeHTTP(message, httptest.NewRequest("POST", "/api/ai/sessions/"+session.ID+"/messages", body))
	if message.Code != 200 {
		t.Fatalf("message status %d", message.Code)
	}
	if !strings.Contains(message.Body.String(), "event: final") || !strings.Contains(message.Body.String(), "id:") {
		t.Fatalf("missing SSE events: %s", message.Body.String())
	}
	loaded := httptest.NewRecorder()
	r.ServeHTTP(loaded, httptest.NewRequest("GET", "/api/ai/sessions/"+session.ID, nil))
	if !strings.Contains(loaded.Body.String(), "已完成") {
		t.Fatalf("history missing assistant content: %s", loaded.Body.String())
	}
	if p.calls != 1 {
		t.Fatalf("provider calls = %d", p.calls)
	}
	duplicate := httptest.NewRecorder()
	r.ServeHTTP(duplicate, httptest.NewRequest("POST", "/api/ai/sessions/"+session.ID+"/messages", strings.NewReader(`{"text":"查影像","request_id":"req-1"}`)))
	if duplicate.Code != 200 || p.calls != 1 {
		t.Fatalf("duplicate request was not replayed: status=%d calls=%d", duplicate.Code, p.calls)
	}
	conflict := httptest.NewRecorder()
	r.ServeHTTP(conflict, httptest.NewRequest("POST", "/api/ai/sessions/"+session.ID+"/messages", strings.NewReader(`{"text":"查变化","request_id":"req-1"}`)))
	if conflict.Code != 409 {
		t.Fatalf("request id with different text status = %d", conflict.Code)
	}

	stored, _ := g.repo.getSession(session.ID)
	replay := httptest.NewRecorder()
	replayReq := httptest.NewRequest("GET", "/api/ai/workflows/"+stored.WorkflowIDs[0]+"/events", nil)
	replayReq.Header.Set("Last-Event-ID", "1")
	r.ServeHTTP(replay, replayReq)
	if replay.Code != 200 || strings.Contains(replay.Body.String(), `"id":1`) {
		t.Fatalf("unexpected replay: %s", replay.Body.String())
	}
}

type waitingProvider struct {
	started chan struct{}
	release chan struct{}
}

func (p *waitingProvider) Complete(_ context.Context, request CompletionRequest) (CompletionResponse, error) {
	if request.Stage == "skill_selection" {
		return fixtureSkillSelection(request), nil
	}
	if request.Stage == "intent" {
		return gatewayTestIntentResponse(), nil
	}
	select {
	case <-p.started:
	default:
		close(p.started)
	}
	<-p.release
	return CompletionResponse{Message: ProviderMessage{Role: "assistant", Content: "done"}}, nil
}

func TestGatewayRejectsConcurrentMessagesInOneSession(t *testing.T) {
	g, err := NewGateway(t.TempDir(), []ToolSpec{testSpec()}, testExecutor{})
	if err != nil {
		t.Fatal(err)
	}
	provider := &waitingProvider{started: make(chan struct{}), release: make(chan struct{})}
	g.SetProvider(provider)
	if err := g.repo.upsertSession(Session{ID: "session-1"}); err != nil {
		t.Fatal(err)
	}
	r := gin.New()
	g.Register(r.Group("/api"))
	firstDone := make(chan int, 1)
	go func() {
		response := httptest.NewRecorder()
		r.ServeHTTP(response, httptest.NewRequest("POST", "/api/ai/sessions/session-1/messages", strings.NewReader(`{"text":"查影像","request_id":"req-1"}`)))
		firstDone <- response.Code
	}()
	select {
	case <-provider.started:
	case <-time.After(time.Second):
		t.Fatal("first provider call did not start")
	}
	second := httptest.NewRecorder()
	r.ServeHTTP(second, httptest.NewRequest("POST", "/api/ai/sessions/session-1/messages", strings.NewReader(`{"text":"另一条查询","request_id":"req-2"}`)))
	if second.Code != 409 {
		t.Fatalf("concurrent message status = %d body=%s", second.Code, second.Body.String())
	}
	close(provider.release)
	select {
	case status := <-firstDone:
		if status != 200 {
			t.Fatalf("first message status = %d", status)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("first message did not finish")
	}
}

func TestGatewayDomainWorkflowEventsStreamProgressAndTerminalState(t *testing.T) {
	g, err := NewGateway(t.TempDir(), []ToolSpec{testSpec()}, &domainWorkflowStub{})
	if err != nil {
		t.Fatal(err)
	}
	r := gin.New()
	g.Register(r.Group("/api"))
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest("GET", "/api/ai/workflows/domain-1/events", nil)
	request.Header.Set("Accept", "text/event-stream")
	done := make(chan struct{})
	go func() {
		r.ServeHTTP(recorder, request)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("domain workflow SSE did not reach terminal state")
	}
	body := recorder.Body.String()
	if recorder.Code != 200 || recorder.Header().Get("Content-Type") != "text/event-stream" {
		t.Fatalf("unexpected SSE response: code=%d headers=%v body=%s", recorder.Code, recorder.Header(), body)
	}
	if !strings.Contains(body, "id: 1\nevent: workflow\ndata: {\"progress\":50,\"status\":\"running\"}") ||
		!strings.Contains(body, "id: 2\nevent: workflow\ndata: {\"progress\":100,\"status\":\"succeeded\"}") {
		t.Fatalf("missing progress or terminal events: %s", body)
	}
}

func TestGatewayDomainWorkflowEventsHonorsLastEventID(t *testing.T) {
	g, err := NewGateway(t.TempDir(), []ToolSpec{testSpec()}, &domainWorkflowStub{})
	if err != nil {
		t.Fatal(err)
	}
	r := gin.New()
	g.Register(r.Group("/api"))
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest("GET", "/api/ai/workflows/domain-1/events", nil)
	request.Header.Set("Accept", "text/event-stream")
	request.Header.Set("Last-Event-ID", "1")
	r.ServeHTTP(recorder, request)
	body := recorder.Body.String()
	if recorder.Code != 200 || strings.Contains(body, "id: 1\n") || !strings.Contains(body, "id: 2\n") {
		t.Fatalf("Last-Event-ID was not applied: code=%d body=%s", recorder.Code, body)
	}
}

func TestGatewayPersistsToolRepliesForLaterProviderTurns(t *testing.T) {
	provider := &transcriptProvider{}
	g, err := NewGateway(t.TempDir(), []ToolSpec{testSpec()}, testExecutor{})
	if err != nil {
		t.Fatal(err)
	}
	g.SetProvider(provider)
	r := gin.New()
	g.Register(r.Group("/api"))
	created := httptest.NewRecorder()
	r.ServeHTTP(created, httptest.NewRequest("POST", "/api/ai/sessions", nil))
	var session Session
	if err := json.Unmarshal(created.Body.Bytes(), &session); err != nil {
		t.Fatal(err)
	}
	for _, request := range []string{
		`{"text":"查影像","request_id":"first"}`,
		`{"text":"继续","request_id":"second"}`,
	} {
		response := httptest.NewRecorder()
		r.ServeHTTP(response, httptest.NewRequest("POST", "/api/ai/sessions/"+session.ID+"/messages", strings.NewReader(request)))
		if response.Code != 200 {
			t.Fatalf("message status=%d body=%s", response.Code, response.Body.String())
		}
	}
	provider.mu.Lock()
	requests := append([]CompletionRequest(nil), provider.requests...)
	provider.mu.Unlock()
	if len(requests) < 3 {
		t.Fatalf("provider calls=%d, expected a second user turn after the tool error", len(requests))
	}
	secondTurn := requests[2].Messages
	foundCall, foundReply := false, false
	for _, message := range secondTurn {
		if len(message.ToolCalls) > 0 && message.ToolCalls[0].ID == "malformed-1" {
			foundCall = true
		}
		if message.Role == "tool" && message.ToolCallID == "malformed-1" {
			foundReply = true
		}
	}
	if !foundCall || !foundReply {
		t.Fatalf("second provider turn lost tool transcript: %#v", secondTurn)
	}
}

func TestGatewayUIActionRequiresExplicitAck(t *testing.T) {
	g, err := NewGateway(t.TempDir(), []ToolSpec{testSpec()}, testExecutor{})
	if err != nil {
		t.Fatal(err)
	}
	s := Session{ID: "s1", Actions: map[string]PendingAction{}, Workspace: map[string]any{"revision": "r1"}}
	if err := g.repo.upsertSession(s); err != nil {
		t.Fatal(err)
	}
	w := Workflow{ID: "w1", SessionID: "s1", Status: "running", CreatedAt: time.Now(), UpdatedAt: time.Now()}
	if err := g.repo.upsertWorkflow(w); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		done <- g.handleUICommand(context.Background(), &s, &w, io.Discard, ProviderToolCall{Function: struct {
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		}{Name: "ui.select_region"}}, map[string]any{"region_id": "r"})
	}()
	deadline := time.Now().Add(time.Second)
	var actionID string
	for time.Now().Before(deadline) {
		current, _ := g.repo.getSession("s1")
		for id := range current.Actions {
			actionID = id
		}
		if actionID != "" {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if actionID == "" {
		t.Fatal("action was not persisted")
	}
	current, _ := g.repo.getSession("s1")
	if current.Actions[actionID].Status != "pending" {
		t.Fatal("action was auto-applied")
	}
	ack := ActionAck{Status: "rejected"}
	pending := current.Actions[actionID]
	pending.Ack = &ack
	pending.Status = "rejected"
	current.Actions[actionID] = pending
	_ = g.repo.upsertSession(current)
	g.ackMu.Lock()
	if ch := g.acks[actionID]; ch != nil {
		ch <- ack
	}
	g.ackMu.Unlock()
	if err := <-done; err == nil {
		t.Fatal("rejected action reported success")
	}
}
