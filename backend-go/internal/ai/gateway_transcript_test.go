package ai

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

const gatewayTranscriptTestSkillID = "ui.transcript.test.v1"
const gatewayTranscriptTestToolCallID = "ui-call-1"

type gatewayTranscriptTestProvider struct {
	mu           sync.Mutex
	requests     []CompletionRequest
	emitToolCall bool
}

func (p *gatewayTranscriptTestProvider) Complete(_ context.Context, request CompletionRequest) (CompletionResponse, error) {
	p.mu.Lock()
	p.requests = append(p.requests, cloneGatewayTranscriptRequest(request))
	callNumber := len(p.requests)
	p.mu.Unlock()

	if p.emitToolCall && callNumber == 1 {
		var call ProviderToolCall
		call.ID = gatewayTranscriptTestToolCallID
		call.Type = "function"
		call.Function.Name = "ui__select_region"
		call.Function.Arguments = `{"region_id":"region-1"}`
		return CompletionResponse{Message: ProviderMessage{Role: "assistant", ToolCalls: []ProviderToolCall{call}}}, nil
	}
	return CompletionResponse{Message: ProviderMessage{Role: "assistant", Content: "已处理"}}, nil
}

func (p *gatewayTranscriptTestProvider) request(number int) (CompletionRequest, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if number < 1 || number > len(p.requests) {
		return CompletionRequest{}, false
	}
	return cloneGatewayTranscriptRequest(p.requests[number-1]), true
}

func cloneGatewayTranscriptRequest(request CompletionRequest) CompletionRequest {
	encoded, _ := json.Marshal(request.Messages)
	var messages []ProviderMessage
	_ = json.Unmarshal(encoded, &messages)
	return CompletionRequest{Messages: messages, Tools: append([]ProviderTool(nil), request.Tools...)}
}

func TestGatewayUIAckTranscriptSurvivesLaterTurnsAndRestart(t *testing.T) {
	previousGinMode := gin.Mode()
	gin.SetMode(gin.TestMode)
	t.Cleanup(func() { gin.SetMode(previousGinMode) })
	for _, testCase := range []struct {
		name         string
		ackStatus    string
		resultStatus string
		resultData   string
		resultError  string
	}{
		{name: "applied", ackStatus: "applied", resultStatus: "ok", resultData: "applied"},
		{name: "rejected", ackStatus: "rejected", resultStatus: "error", resultError: "UI_ACTION_FAILED"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			dataDir := t.TempDir()
			gateway, err := NewGateway(dataDir, []ToolSpec{testSpec()}, testExecutor{})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(gateway.Close)
			gateway.skills[gatewayTranscriptTestSkillID] = SkillManifest{
				ID: gatewayTranscriptTestSkillID, Version: "1.0.0", MaxSteps: 4,
				AllowedTools: []string{"ui.select_region"},
			}
			provider := &gatewayTranscriptTestProvider{emitToolCall: true}
			gateway.SetProvider(provider)
			router := gin.New()
			gateway.Register(router.Group("/api"))

			created := httptest.NewRecorder()
			router.ServeHTTP(created, httptest.NewRequest("POST", "/api/ai/sessions", nil))
			if created.Code != 201 {
				t.Fatalf("create session status=%d body=%s", created.Code, created.Body.String())
			}
			var session Session
			if err := json.Unmarshal(created.Body.Bytes(), &session); err != nil {
				t.Fatal(err)
			}

			firstResponse := httptest.NewRecorder()
			firstDone := make(chan struct{})
			go func() {
				router.ServeHTTP(firstResponse, httptest.NewRequest("POST", "/api/ai/sessions/"+session.ID+"/messages", strings.NewReader(
					`{"text":"定位区域","request_id":"turn-1","skill_id":"`+gatewayTranscriptTestSkillID+`"}`,
				)))
				close(firstDone)
			}()

			actionID := gatewayTranscriptWaitForPendingAction(t, gateway, session.ID)
			ackBody, err := json.Marshal(ActionAck{
				Status: testCase.ackStatus, Workspace: map[string]any{"revision": "revision-2"},
			})
			if err != nil {
				t.Fatal(err)
			}
			ackResponse := httptest.NewRecorder()
			router.ServeHTTP(ackResponse, httptest.NewRequest("POST", "/api/ai/sessions/"+session.ID+"/actions/"+actionID, strings.NewReader(string(ackBody))))
			if ackResponse.Code != 200 {
				t.Fatalf("action ack status=%d body=%s", ackResponse.Code, ackResponse.Body.String())
			}
			select {
			case <-firstDone:
			case <-time.After(2 * time.Second):
				t.Fatal("first conversation did not finish after action acknowledgement")
			}
			if firstResponse.Code != 200 || !strings.Contains(firstResponse.Body.String(), "event: final") {
				t.Fatalf("first conversation did not finish: status=%d body=%s", firstResponse.Code, firstResponse.Body.String())
			}
			gatewayTranscriptAssertPair(t, provider, 2, testCase.resultStatus, testCase.resultData, testCase.resultError)

			secondResponse := httptest.NewRecorder()
			router.ServeHTTP(secondResponse, httptest.NewRequest("POST", "/api/ai/sessions/"+session.ID+"/messages", strings.NewReader(
				`{"text":"继续说明","request_id":"turn-2","skill_id":"`+gatewayTranscriptTestSkillID+`"}`,
			)))
			if secondResponse.Code != 200 || !strings.Contains(secondResponse.Body.String(), "event: final") {
				t.Fatalf("second conversation did not finish: status=%d body=%s", secondResponse.Code, secondResponse.Body.String())
			}
			gatewayTranscriptAssertPair(t, provider, 3, testCase.resultStatus, testCase.resultData, testCase.resultError)

			gateway.Close()
			restored, err := NewGateway(dataDir, []ToolSpec{testSpec()}, testExecutor{})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(restored.Close)
			restored.skills[gatewayTranscriptTestSkillID] = SkillManifest{
				ID: gatewayTranscriptTestSkillID, Version: "1.0.0", MaxSteps: 4,
				AllowedTools: []string{"ui.select_region"},
			}
			restoredProvider := &gatewayTranscriptTestProvider{}
			restored.SetProvider(restoredProvider)
			restoredRouter := gin.New()
			restored.Register(restoredRouter.Group("/api"))
			thirdResponse := httptest.NewRecorder()
			restoredRouter.ServeHTTP(thirdResponse, httptest.NewRequest("POST", "/api/ai/sessions/"+session.ID+"/messages", strings.NewReader(
				`{"text":"再继续","request_id":"turn-3","skill_id":"`+gatewayTranscriptTestSkillID+`"}`,
			)))
			if thirdResponse.Code != 200 || !strings.Contains(thirdResponse.Body.String(), "event: final") {
				t.Fatalf("restored conversation did not finish: status=%d body=%s", thirdResponse.Code, thirdResponse.Body.String())
			}
			gatewayTranscriptAssertPair(t, restoredProvider, 1, testCase.resultStatus, testCase.resultData, testCase.resultError)
		})
	}
}

func gatewayTranscriptWaitForPendingAction(t *testing.T, gateway *Gateway, sessionID string) string {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		session, ok := gateway.repo.getSession(sessionID)
		if ok {
			for id, action := range session.Actions {
				if action.Status == "pending" {
					return id
				}
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("UI action was not persisted as pending")
	return ""
}

func gatewayTranscriptAssertPair(t *testing.T, provider *gatewayTranscriptTestProvider, requestNumber int, expectedStatus, expectedData, expectedError string) {
	t.Helper()
	request, ok := provider.request(requestNumber)
	if !ok {
		t.Fatalf("provider request %d was not captured", requestNumber)
	}
	callCount, replyCount := 0, 0
	for _, message := range request.Messages {
		if message.Role == "assistant" {
			for _, call := range message.ToolCalls {
				if call.ID == gatewayTranscriptTestToolCallID {
					callCount++
				}
			}
		}
		if message.Role != "tool" || message.ToolCallID != gatewayTranscriptTestToolCallID {
			continue
		}
		replyCount++
		var result Result
		if err := json.Unmarshal([]byte(message.Content), &result); err != nil {
			t.Fatalf("tool reply is not a serialized result: %v (%s)", err, message.Content)
		}
		if result.Status != expectedStatus {
			t.Fatalf("tool result status=%q want %q: %+v", result.Status, expectedStatus, result)
		}
		if expectedData != "" {
			data, _ := result.Data.(map[string]any)
			if data["status"] != expectedData {
				t.Fatalf("tool result data status=%v want %q", data["status"], expectedData)
			}
		}
		if expectedError != "" && (result.Error == nil || result.Error.Code != expectedError) {
			t.Fatalf("tool result error=%+v want code %q", result.Error, expectedError)
		}
	}
	if callCount != 1 || replyCount != 1 {
		t.Fatalf("provider request %d has tool-call/result counts %d/%d, want 1/1: %#v", requestNumber, callCount, replyCount, request.Messages)
	}
}
