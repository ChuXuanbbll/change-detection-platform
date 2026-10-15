package api

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"cdp/backend-go/internal/ai"
)

func TestAIWorkspaceContextIncludesSafeDetectionSelection(t *testing.T) {
	_, registry, _ := newDomainFixture(t)
	call := domainCall("workspace.context", "workspace-config", map[string]any{})
	call.Context = ai.Context{Workspace: map[string]any{"revision": "r1", "region_id": "130600", "phase_ids": []any{"p1", "p2"}, "detection": map[string]any{"model_id": "m", "config_revision": 1, "settings": map[string]any{"threshold": 0.73, "resolution": 0.8, "kernel_size": 3, "min_area_pixels": 10}}}}
	result := registry.Invoke(context.Background(), call)
	if result.Status != "ok" {
		t.Fatalf("workspace failed its actual domain output schema: %+v", result)
	}
	encoded, _ := json.Marshal(result.Data)
	if !strings.Contains(string(encoded), `"threshold":0.73`) || strings.Contains(string(encoded), "model_path") {
		t.Fatalf("selection missing or unsafe: %s", encoded)
	}
}
