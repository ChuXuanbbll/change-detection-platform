package ai

import (
	"context"
	"io"
	"testing"
	"time"
)

func TestCountyDetectionBindsOnlyExplicitResolvedBoundary(t *testing.T) {
	for _, test := range []struct {
		name                           string
		explicit, resolved, sameSource bool
		wantError                      bool
	}{
		{"explicit county", true, true, true, false},
		{"browse preserves manual selection", false, true, true, false},
		{"unresolved county", true, false, true, true},
		{"wrong source", true, true, false, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			gateway, session, workflow, _ := composedGateway(t, &composedProvider{})
			session.Workspace = map[string]any{"revision": "r1", "region_id": "xiongan"}
			path := []any{"河北省", "雄安新区", "容城县"}
			manual := map[string]any{"type": "Polygon", "coordinates": []any{}}
			workflow.Intent = &Intent{Operations: []IntentOperation{{SkillID: "change.detect.v1", Slots: map[string]any{"operation": "detect", "admin_path": path, "use_admin_boundary": test.explicit, "aoi": manual}}}}
			if test.resolved {
				source := "xiongan"
				if !test.sameSource {
					source = "130600"
				}
				workflow.ToolCalls = []WorkflowToolCall{{Tool: "region.search", Status: "ok", Input: map[string]any{"admin_path": path}, Data: map[string]any{"matches": []any{map[string]any{"region_id": source, "admin_code": "130629", "level": "county", "boundary_version": "immutable-v1"}}}}}
			}
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			call := composedCall("submit", "ui.submit_detection", "{}")
			if test.wantError {
				if gateway.handleUICommand(ctx, session, workflow, io.Discard, call, map[string]any{}) == nil {
					t.Fatal("untrusted county accepted")
				}
				return
			}
			done := make(chan error, 1)
			go func() { done <- gateway.handleUICommand(ctx, session, workflow, io.Discard, call, map[string]any{}) }()
			var pending PendingAction
			for deadline := time.Now().Add(time.Second); time.Now().Before(deadline); {
				stored, _ := gateway.repo.getSession(session.ID)
				for _, action := range stored.Actions {
					pending = action
				}
				if pending.ID != "" {
					break
				}
				time.Sleep(time.Millisecond)
			}
			if pending.ID == "" {
				t.Fatal("county command missing")
			}
			args := pending.Command.Args
			if test.explicit {
				if args["admin_code"] != "130629" || args["admin_version"] != "immutable-v1" || args["region_id"] != "xiongan" || args["aoi"] != nil {
					t.Fatalf("trusted reference not bound: %+v", args)
				}
			} else if args["admin_code"] != nil || !sameJSON(args["aoi"], manual) {
				t.Fatalf("browsing replaced manual selection: %+v", args)
			}
			gateway.ackMu.Lock()
			ack := gateway.acks[pending.ID]
			gateway.ackMu.Unlock()
			ack <- ActionAck{Status: "applied"}
			if err := <-done; err != nil {
				t.Fatal(err)
			}
		})
	}
}
