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
	call.Context.Workspace["catalog_status"] = "ready"
	call.Context.Workspace["available_phases"] = []any{
		map[string]any{"id": "p0", "month": "2025-01", "scene_ids": []any{"s0"}},
		map[string]any{"id": "p1", "month": "2025-05", "scene_ids": []any{"s1"}},
	}
	call.Context.Workspace["selection_error"] = ""
	result := registry.Invoke(context.Background(), call)
	if result.Status != "ok" {
		t.Fatalf("workspace failed its actual domain output schema: %+v", result)
	}
	encoded, _ := json.Marshal(result.Data)
	if !strings.Contains(string(encoded), `"threshold":0.73`) || !strings.Contains(string(encoded), `"month":"2025-01"`) || !strings.Contains(string(encoded), `"catalog_status":"ready"`) || strings.Contains(string(encoded), "model_path") {
		t.Fatalf("selection missing or unsafe: %s", encoded)
	}
}
