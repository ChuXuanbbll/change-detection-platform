package ai

import (
	"context"
	"errors"
	"strings"
	"testing"
)

type recoveryProvider struct {
	requests []CompletionRequest
	replies  []ProviderMessage
}

func (p *recoveryProvider) Complete(_ context.Context, request CompletionRequest) (CompletionResponse, error) {
	p.requests = append(p.requests, cloneGatewayTranscriptRequest(request))
	index := len(p.requests) - 1
	if index >= len(p.replies) {
		index = len(p.replies) - 1
	}
	return CompletionResponse{Message: p.replies[index]}, nil
}

func TestStructuredIntentRepairsOnceWithoutExecutingTools(t *testing.T) {
	for _, stage := range []string{"intent", "skill_selection"} {
		t.Run(stage, func(t *testing.T) {
			provider := &recoveryProvider{replies: []ProviderMessage{
				{Role: "assistant", Content: "```json invalid```"},
				composedToolMessage(composedCall("fixed", "route_intent", `{"operations":[],"needs_clarification":true,"question":"要比较哪两个月？"}`)),
			}}
			g, _ := intentGateway(t, "", false)
			notices := 0
			ctx := withIntentProgress(context.Background(), func(got string) {
				if got != stage {
					t.Fatalf("stage=%s", got)
				}
				notices++
			})
			request := CompletionRequest{Stage: stage, Messages: []ProviderMessage{{Role: "user", Content: "检测当前框选"}}, Tools: []ProviderTool{{Type: "function", Function: ProviderFunction{Name: "route_intent", Parameters: g.intentSchema()}}}}
			err := completeStructuredIntent(ctx, provider, request, func(raw string) error { _, err := g.decodeIntent(raw, g.intentSchema()); return err })
			if err != nil || len(provider.requests) != 2 || notices != 1 {
				t.Fatalf("err=%v calls=%d notices=%d", err, len(provider.requests), notices)
			}
			last := provider.requests[1]
			if len(last.Tools) != 1 || strings.Contains(last.Messages[len(last.Messages)-1].Content, "invalid```") {
				t.Fatal("repair echoed malformed output or exposed extra tools")
			}
		})
	}
}

func TestStructuredIntentStopsAfterRepairAndReportsStage(t *testing.T) {
	provider := &recoveryProvider{replies: []ProviderMessage{{Role: "assistant", Content: "invalid"}}}
	request := CompletionRequest{Stage: "skill_selection", Tools: []ProviderTool{{Function: ProviderFunction{Name: "select_skills"}}}}
	err := completeStructuredIntent(context.Background(), provider, request, func(string) error { return invalidIntent() })
	var intentErr *IntentError
	if !errors.As(err, &intentErr) || intentErr.Stage != "skill_selection" || len(provider.requests) != 2 || !strings.Contains(intentErr.Message, "本轮未执行") {
		t.Fatalf("err=%v requests=%d", err, len(provider.requests))
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if !errors.Is(completeStructuredIntent(ctx, provider, request, func(string) error { return nil }), context.Canceled) || len(provider.requests) != 2 {
		t.Fatal("cancelled parse contacted provider")
	}
}

func TestDetectionIntentUsesUnloadedCatalogMonthsWithoutChangingLoadedSelection(t *testing.T) {
	g, provider := intentGateway(t, `{"operations":[{"skill_id":"change.detect.v1","slots":{"operation":"detect","use_workspace_context":true,"phase_dates":["2025-01","2025-05"]}}],"needs_clarification":false,"question":""}`, true)
	workspace := map[string]any{"region_id": "r1", "phase_ids": []any{"may", "nov"}, "catalog_status": "ready", "available_phases": []any{map[string]any{"id": "jan", "month": "2025-01"}, map[string]any{"id": "mar", "month": "2025-03"}}}
	intent, err := g.parseIntent(context.Background(), &Session{Workspace: workspace}, MessageRequest{Text: "检测当前框选2025年1月和5月的差异"})
	if err != nil || intent.NeedsClarification || !sameJSON(intent.Operations[0].Slots["phase_dates"], []any{"2025-01", "2025-05"}) {
		t.Fatalf("intent=%+v err=%v", intent, err)
	}
	if !strings.Contains(provider.requests[0].Messages[1].Content, `"available_phases"`) || !sameJSON(workspace["phase_ids"], []any{"may", "nov"}) {
		t.Fatal("full catalog missing or loaded selection changed during parsing")
	}
}
