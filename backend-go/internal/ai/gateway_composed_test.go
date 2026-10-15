package ai

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"
)

type composedProvider struct {
	intent   string
	replies  []ProviderMessage
	requests []CompletionRequest
}

func (p *composedProvider) Complete(_ context.Context, request CompletionRequest) (CompletionResponse, error) {
	if request.Stage == "skill_selection" {
		return fixtureSkillSelection(request), nil
	}
	if request.Stage == "intent" {
		return CompletionResponse{Message: ProviderMessage{Role: "assistant", Content: p.intent}}, nil
	}
	p.requests = append(p.requests, request)
	index := len(p.requests) - 1
	if index >= len(p.replies) {
		return CompletionResponse{Message: ProviderMessage{Role: "assistant", Content: "完成"}}, nil
	}
	return CompletionResponse{Message: p.replies[index]}, nil
}

type composedExecutor struct{ calls []Call }

func (e *composedExecutor) Execute(_ context.Context, call Call) Result {
	e.calls = append(e.calls, call)
	data := map[string]any{}
	switch call.Tool {
	case "task.search":
		data = map[string]any{"items": []any{map[string]any{"task_id": "new-task"}, map[string]any{"task_id": "old-task"}}}
	case "task.status":
		data = map[string]any{"task_id": stringValue(call.Input["task_id"]), "status": "completed"}
	case "result.summary":
		data = map[string]any{"task_id": stringValue(call.Input["task_id"]), "result_id": "new-result"}
	case "result.export.prepare":
		data = map[string]any{"result_id": "new-result", "download_ref": "controlled-download"}
	case "model.list":
		data = map[string]any{"items": []any{}}
	}
	return Result{Status: "ok", Data: data}
}

func composedCall(id, name, arguments string) ProviderToolCall {
	call := ProviderToolCall{ID: id, Type: "function"}
	call.Function.Name, call.Function.Arguments = name, arguments
	return call
}

func composedToolMessage(calls ...ProviderToolCall) ProviderMessage {
	return ProviderMessage{Role: "assistant", ToolCalls: calls}
}

func composedGateway(t *testing.T, provider *composedProvider) (*Gateway, *Session, *Workflow, *composedExecutor) {
	t.Helper()
	executor := &composedExecutor{}
	specs := []ToolSpec{}
	for _, tool := range []string{"workspace.context", "model.list", "task.search", "task.status", "result.summary", "result.export.prepare", "detection.workflow.submit"} {
		spec := testToolSpecNamed(tool)
		spec.InputSchema["properties"] = map[string]any{
			"task_id": map[string]any{"type": "string"}, "format": map[string]any{"type": "string"},
		}
		specs = append(specs, spec)
	}
	gateway, err := NewGateway(t.TempDir(), specs, executor)
	if err != nil {
		t.Fatal(err)
	}
	gateway.SetProvider(provider)
	t.Cleanup(gateway.Close)
	session := &Session{ID: "composed-session", Workspace: map[string]any{"task_id": "old-task"}}
	workflow := &Workflow{ID: "composed-workflow", SessionID: session.ID, Status: "running"}
	if err := gateway.repo.upsertSession(*session); err != nil {
		t.Fatal(err)
	}
	if err := gateway.repo.upsertWorkflow(*workflow); err != nil {
		t.Fatal(err)
	}
	return gateway, session, workflow, executor
}

func composedAssertClarification(t *testing.T, session *Session, workflow *Workflow) {
	t.Helper()
	if session.PendingIntent == nil || !session.PendingIntent.NeedsClarification || session.PendingIntent.Question == "" {
		t.Fatalf("clarification was not retained: %+v", session.PendingIntent)
	}
	if len(workflow.Events) == 0 {
		t.Fatal("no final event")
	}
	event := workflow.Events[len(workflow.Events)-1]
	if event.Type != "final" || stringValue(mapValue(event.Data)["status"]) != "needs_clarification" {
		t.Fatalf("clarification reported as completion: %+v", event)
	}
}

func TestGatewayComposedClarificationPreemptsAllSiblingTools(t *testing.T) {
	provider := &composedProvider{
		intent: `{"operations":[{"skill_id":"imagery.explore.v1","slots":{"operation":"models"}},{"skill_id":"result.inspect.v1","slots":{"operation":"query_tasks"}}],"needs_clarification":false,"question":""}`,
		replies: []ProviderMessage{composedToolMessage(
			composedCall("dangerous-first", "detection__workflow__submit", `{}`),
			composedCall("clarification", "request_clarification", `{"question":"请先选择检测范围。"}`),
			composedCall("query-last", "workspace__context", `{}`),
		)},
	}
	gateway, session, workflow, executor := composedGateway(t, provider)
	if err := gateway.runConversation(context.Background(), session, workflow, MessageRequest{Text: "查看模型，然后查询任务"}, io.Discard); err != nil {
		t.Fatal(err)
	}
	composedAssertClarification(t, session, workflow)
	if len(executor.calls) != 0 || len(provider.requests) != 1 || len(workflow.ToolCalls) != 0 {
		t.Fatalf("clarification executed tools: calls=%+v requests=%d workflow=%+v", executor.calls, len(provider.requests), workflow.ToolCalls)
	}
	for _, message := range session.Messages {
		if message.Provider != nil && len(message.Provider.ToolCalls) != 0 {
			t.Fatal("unexecuted sibling calls persisted into provider transcript")
		}
	}
}

func TestGatewayComposedPlainTextDoesNotUnlockNextOperation(t *testing.T) {
	provider := &composedProvider{
		intent:  `{"operations":[{"skill_id":"imagery.explore.v1","slots":{"operation":"models"}},{"skill_id":"result.inspect.v1","slots":{"operation":"query_tasks"}}],"needs_clarification":false,"question":""}`,
		replies: []ProviderMessage{{Role: "assistant", Content: "已完成，接下来查询任务。"}},
	}
	gateway, session, workflow, executor := composedGateway(t, provider)
	if err := gateway.runConversation(context.Background(), session, workflow, MessageRequest{Text: "查看模型，然后查询任务"}, io.Discard); err != nil {
		t.Fatal(err)
	}
	composedAssertClarification(t, session, workflow)
	if len(provider.requests) != 1 || len(executor.calls) != 0 || workflow.OperationIndex != 0 {
		t.Fatal("plain-text claim advanced composed execution")
	}
}

func TestGatewayComposedInheritedTaskCannotUseOlderWorkspaceTask(t *testing.T) {
	provider := &composedProvider{
		intent: `{"operations":[{"skill_id":"result.inspect.v1","slots":{"operation":"summary","latest":true}},{"skill_id":"result.inspect.v1","slots":{"operation":"export","use_previous_result":true}}],"needs_clarification":false,"question":""}`,
		replies: []ProviderMessage{
			composedToolMessage(composedCall("context-0", "workspace__context", `{}`), composedCall("search-0", "task__search", `{}`), composedCall("status-0", "task__status", `{"task_id":"new-task"}`), composedCall("summary-0", "result__summary", `{"task_id":"new-task"}`)),
			{Role: "assistant", Content: "已查到 new-task 的 new-result。"},
			composedToolMessage(composedCall("context-1", "workspace__context", `{}`), composedCall("search-1", "task__search", `{}`), composedCall("wrong-task", "task__status", `{"task_id":"old-task"}`), composedCall("status-1", "task__status", `{"task_id":"new-task"}`), composedCall("summary-1", "result__summary", `{"task_id":"new-task"}`), composedCall("export-1", "result__export__prepare", `{"task_id":"new-task","format":"geojson"}`)),
			{Role: "assistant", Content: "导出完成。"},
		},
	}
	gateway, session, workflow, executor := composedGateway(t, provider)
	if err := gateway.runConversation(context.Background(), session, workflow, MessageRequest{Text: "查看上次结果，然后导出该结果"}, io.Discard); err != nil {
		t.Fatal(err)
	}
	if workflow.Status != "succeeded" || session.PendingIntent != nil || len(provider.requests) != 4 {
		t.Fatalf("composed result failed: workflow=%+v pending=%+v", workflow, session.PendingIntent)
	}
	for _, call := range executor.calls {
		if stringValue(call.Input["task_id"]) == "old-task" {
			t.Fatalf("bound operation executed old workspace task: %+v", call)
		}
	}
	var contextValue struct {
		Operation IntentOperation `json:"operation"`
	}
	content := provider.requests[2].Messages[0].Content
	start := strings.Index(content, "\nNormalized operation: ")
	if start < 0 {
		t.Fatal("normalized operation missing")
	}
	data := strings.TrimPrefix(content[start:], "\nNormalized operation: ")
	decoder := json.NewDecoder(strings.NewReader(data))
	if err := decoder.Decode(&contextValue); err != nil || stringValue(contextValue.Operation.Slots["task_id"]) != "new-task" {
		t.Fatalf("fresh task not bound in execution context: %+v, %v", contextValue, err)
	}
	if contextValue.Operation.Slots["use_previous_result"] != nil {
		t.Fatal("resolved inheritance flag leaked into resumable intent")
	}
	indexes := map[int]int{}
	for _, call := range workflow.ToolCalls {
		indexes[call.OperationIndex]++
	}
	if indexes[0] != 4 || indexes[1] != 5 {
		t.Fatalf("operation calls missing ownership: %+v", indexes)
	}
}

func TestGatewayComposedPrerequisitesNeverCrossOperationBoundary(t *testing.T) {
	skills, err := loadSkillManifests()
	if err != nil {
		t.Fatal(err)
	}
	gateway := &Gateway{skills: skills}
	workflow := &Workflow{SkillID: "result.inspect.v1", OperationIndex: 1, ToolCalls: []WorkflowToolCall{
		{OperationIndex: 0, Tool: "result.summary", Status: "ok", Data: map[string]any{"task_id": "same-task", "result_id": "same-result"}},
	}}
	if gateway.skillPrerequisitesSatisfied(workflow, "result.export.prepare", map[string]any{"task_id": "same-task"}) {
		t.Fatal("earlier same-skill summary unlocked a later export")
	}
	workflow.ToolCalls = append(workflow.ToolCalls, WorkflowToolCall{OperationIndex: 1, Tool: "result.summary", Status: "ok", Data: map[string]any{"task_id": "same-task", "result_id": "same-result"}})
	if !gateway.skillPrerequisitesSatisfied(workflow, "result.export.prepare", map[string]any{"task_id": "same-task"}) {
		t.Fatal("current operation's matching prerequisite rejected")
	}
}

func TestGatewayResultQueryRejectsUnrequestedExportDespiteValidPrerequisites(t *testing.T) {
	provider := &composedProvider{
		intent: `{"operations":[{"skill_id":"result.inspect.v1","slots":{"operation":"summary","latest":true}}],"needs_clarification":false,"question":""}`,
		replies: []ProviderMessage{
			composedToolMessage(composedCall("context", "workspace__context", `{}`), composedCall("search", "task__search", `{}`), composedCall("status", "task__status", `{"task_id":"new-task"}`), composedCall("summary", "result__summary", `{"task_id":"new-task"}`), composedCall("unrequested-export", "result__export__prepare", `{"task_id":"new-task","format":"geojson"}`)),
			{Role: "assistant", Content: "结果已查询。"},
		},
	}
	gateway, session, workflow, executor := composedGateway(t, provider)
	if err := gateway.runConversation(context.Background(), session, workflow, MessageRequest{Text: "只查询上次结果"}, io.Discard); err != nil {
		t.Fatal(err)
	}
	for _, call := range executor.calls {
		if call.Tool == "result.export.prepare" {
			t.Fatal("query authorized an unrequested export")
		}
	}
	if len(executor.calls) != 4 {
		t.Fatalf("expected the four allowed read calls, got %+v", executor.calls)
	}
}

func TestGatewayComposedClarificationRetainsOnlyBoundRemainingOperation(t *testing.T) {
	provider := &composedProvider{
		intent: `{"operations":[{"skill_id":"result.inspect.v1","slots":{"operation":"summary","latest":true}},{"skill_id":"result.inspect.v1","slots":{"operation":"export","use_previous_result":true}}],"needs_clarification":false,"question":""}`,
		replies: []ProviderMessage{
			composedToolMessage(composedCall("context", "workspace__context", `{}`), composedCall("search", "task__search", `{}`), composedCall("status", "task__status", `{"task_id":"new-task"}`), composedCall("summary", "result__summary", `{"task_id":"new-task"}`)),
			{Role: "assistant", Content: "已查到 new-task 的 new-result。"},
			composedToolMessage(composedCall("clarify", "request_clarification", `{"question":"要导出哪一种格式？"}`)),
		},
	}
	gateway, session, workflow, _ := composedGateway(t, provider)
	if err := gateway.runConversation(context.Background(), session, workflow, MessageRequest{Text: "查看上次结果，再导出该结果"}, io.Discard); err != nil {
		t.Fatal(err)
	}
	composedAssertClarification(t, session, workflow)
	if len(session.PendingIntent.Operations) != 1 {
		t.Fatalf("completed operation remained pending: %+v", session.PendingIntent)
	}
	remaining := session.PendingIntent.Operations[0]
	if remaining.Slots["task_id"] != "new-task" || remaining.Slots["result_id"] != "new-result" || remaining.Slots["use_previous_result"] != nil {
		t.Fatalf("remaining intent cannot resume safely: %+v", remaining)
	}
	if _, err := gateway.decodeIntent(`{"operations":[{"skill_id":"result.inspect.v1","slots":{"operation":"export","task_id":"new-task","result_id":"new-result","output_format":"csv"}}],"needs_clarification":false,"question":""}`, gateway.intentSchema()); err != nil {
		t.Fatalf("resolved remaining operation rejected on next turn: %v", err)
	}
}
