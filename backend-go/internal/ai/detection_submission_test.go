package ai

import (
	"context"
	"encoding/json"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

func TestDetectionSubmissionBindsIntentAndReturnsStableAckWithoutResubmission(t *testing.T) {
	provider := &composedProvider{intent: `{"operations":[{"skill_id":"change.detect.v1","slots":{"operation":"detect","use_workspace_context":true,"phase_dates":["2025-03","2025-09"],"detection_settings":{"threshold":0.73}}}],"needs_clarification":false,"question":""}`, replies: []ProviderMessage{
		composedToolMessage(composedCall("context", "workspace__context", "{}")),
		composedToolMessage(composedCall("submit", "ui__submit_detection", "{}")),
		composedToolMessage(composedCall("repeat", "ui__submit_detection", "{}")),
		{Role: "assistant", Content: "已提交任务 task-new。"},
	}}
	gateway, session, workflow, _ := composedGateway(t, provider)
	session.Workspace = map[string]any{"revision": "r1", "region_id": "r1", "phase_ids": []any{"p1", "p2"}}
	_ = gateway.repo.upsertSession(*session)
	router := gin.New()
	gateway.Register(router.Group("/api"))
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- gateway.runConversation(ctx, session, workflow, MessageRequest{Text: "当前框选检测2025-03和2025-09"}, io.Discard)
	}()
	var action PendingAction
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		current, _ := gateway.repo.getSession(session.ID)
		for _, pending := range current.Actions {
			if pending.Command.Action == "submit_detection" {
				action = pending
			}
		}
		if action.ID != "" {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if action.ID == "" {
		t.Fatal("submission command not emitted")
	}
	if !sameJSON(action.Command.Args["phase_dates"], []any{"2025-03", "2025-09"}) || dataString(action.Command.Args, "region_id") != "" || !sameJSON(action.Command.Args["detection_settings"], map[string]any{"threshold": 0.73}) {
		t.Fatalf("normalized selection not bound: %+v", action.Command)
	}
	ack := ActionAck{Status: "applied", ContextRevision: "r2", Workspace: map[string]any{"revision": "r2", "task_id": "task-new"}}
	endpoint := "/api/ai/sessions/" + session.ID + "/actions/" + action.ID
	send := func() int {
		encoded, _ := json.Marshal(ack)
		response := httptest.NewRecorder()
		router.ServeHTTP(response, httptest.NewRequest("POST", endpoint, strings.NewReader(string(encoded))))
		return response.Code
	}
	if send() != 400 {
		t.Fatal("applied submission without created task was accepted")
	}
	ack.Result = &DetectionSubmission{TaskID: "task-new", PairCount: 1}
	if status := send(); status != 200 {
		t.Fatalf("ack=%d", status)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if send() != 200 {
		t.Fatal("lost acknowledgement response cannot be retried idempotently")
	}
	ack.Result.TaskID = "conflicting-task"
	if send() != 409 {
		t.Fatal("conflicting acknowledgement replaced committed result")
	}
	stored, _ := gateway.repo.getSession(session.ID)
	if len(stored.Actions) != 1 || !operationCompleted(workflow, workflow.Intent.Operations[0]) {
		t.Fatalf("duplicate submission or no verified completion: %+v", stored.Actions)
	}
	if len(provider.requests) != 4 {
		t.Fatalf("requests=%d", len(provider.requests))
	}
	for _, request := range provider.requests[2:] {
		if !strings.Contains(request.Messages[len(request.Messages)-1].Content, "task-new") {
			t.Fatal("created task missing from tool reply")
		}
	}
	for _, call := range workflow.ToolCalls {
		if strings.HasPrefix(call.Tool, "detection.") || call.Tool == "imagery.prepare" {
			t.Fatalf("low-level tool used: %s", call.Tool)
		}
	}
}

func TestDetectionTextClaimWithoutSubmissionFails(t *testing.T) {
	provider := &composedProvider{intent: `{"operations":[{"skill_id":"change.detect.v1","slots":{"operation":"detect","use_workspace_context":true}}],"needs_clarification":false,"question":""}`, replies: []ProviderMessage{{Role: "assistant", Content: "检测成功"}}}
	gateway, session, workflow, _ := composedGateway(t, provider)
	session.Workspace = map[string]any{"region_id": "r1", "phase_ids": []any{"p1", "p2"}}
	if err := gateway.runConversation(context.Background(), session, workflow, MessageRequest{Text: "执行当前检测"}, io.Discard); err == nil {
		t.Fatal("text claim treated as submitted detection")
	}
	for _, event := range workflow.Events {
		if event.Type == "final" {
			t.Fatal("unsubmitted detection emitted final success")
		}
	}
}
