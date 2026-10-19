package ai

import (
	"context"
	"sync"
	"testing"
)

type operationExecutor struct {
	mu    sync.Mutex
	steps int
}

func (e *operationExecutor) Execute(_ context.Context, call Call) Result {
	switch call.Tool {
	case "imagery.prepare":
		return Result{Status: "ok", Data: map[string]any{"operation_id": "op-1", "status": "queued"}}
	case "operation.status":
		e.mu.Lock()
		e.steps++
		step := e.steps
		e.mu.Unlock()
		status := "pending"
		if step > 1 {
			status = "succeeded"
		}
		return Result{Status: "ok", Data: map[string]any{"operation_id": "op-1", "status": status, "batch_id": "batch-1"}}
	default:
		return Result{Status: "ok", Data: map[string]any{}}
	}
}

func operationSpecs() []ToolSpec {
	object := func(properties map[string]any) map[string]any {
		return map[string]any{"$schema": "https://json-schema.org/draft/2020-12/schema", "type": "object", "additionalProperties": false, "properties": properties}
	}
	return []ToolSpec{
		{Name: "imagery.prepare", Version: "1.0.0", Description: "prepare", Effect: "prepare", MaxOutputSize: 1024, InputSchema: object(map[string]any{"plan_id": map[string]any{"type": "string"}}), OutputSchema: object(map[string]any{"operation_id": map[string]any{"type": "string"}, "status": map[string]any{"type": "string"}})},
		{Name: "operation.status", Version: "1.0.0", Description: "status", Effect: "query", MaxOutputSize: 1024, InputSchema: object(map[string]any{"operation_id": map[string]any{"type": "string"}}), OutputSchema: object(map[string]any{"operation_id": map[string]any{"type": "string"}, "status": map[string]any{"type": "string"}, "batch_id": map[string]any{"type": "string"}})},
	}
}

func TestExecuteSkillToolWaitsForPreparedOperation(t *testing.T) {
	executor := &operationExecutor{}
	registry, err := NewRegistry(operationSpecs(), executor)
	if err != nil {
		t.Fatal(err)
	}
	repo, err := newRepository(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	g := &Gateway{registry: registry, repo: repo}
	workflow := &Workflow{ID: "w1", SkillID: "change.detect.v1"}
	result := g.executeSkillTool(context.Background(), workflow, Call{SchemaVersion: callSchemaVersion, RequestID: "r1", Tool: "imagery.prepare", ToolVersion: "1.0.0", Input: map[string]any{"plan_id": "p1"}})
	if result.Status != "ok" || dataString(result.Data, "status") != "succeeded" {
		t.Fatalf("result = %+v", result)
	}
	if len(workflow.ToolCalls) != 1 || workflow.ToolCalls[0].Tool != "operation.status" {
		t.Fatalf("internal operation call not recorded: %+v", workflow.ToolCalls)
	}
}

func TestExecuteSkillToolOperationWaitHonorsCancellation(t *testing.T) {
	registry, err := NewRegistry(operationSpecs(), &operationExecutor{})
	if err != nil {
		t.Fatal(err)
	}
	g := &Gateway{registry: registry}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	result := g.executeSkillTool(ctx, &Workflow{}, Call{SchemaVersion: callSchemaVersion, RequestID: "r1", Tool: "imagery.prepare", ToolVersion: "1.0.0", Input: map[string]any{"plan_id": "p1"}})
	if result.Error == nil || result.Error.Code != "OPERATION_WAIT_CANCELLED" {
		t.Fatalf("result = %+v", result)
	}
}

func TestSkillPrerequisitesMatchOpaqueResources(t *testing.T) {
	g := &Gateway{skills: map[string]SkillManifest{"test": {Steps: []SkillStep{{ID: "plan", Tool: "detection.plan.create"}, {ID: "prepare", Tool: "imagery.prepare", Requires: []string{"plan"}}}}}}
	w := &Workflow{SkillID: "test", ToolCalls: []WorkflowToolCall{{Tool: "detection.plan.create", Status: "ok", Data: map[string]any{"plan_id": "plan-1"}}}}
	if !g.skillPrerequisitesSatisfied(w, "imagery.prepare", map[string]any{"plan_id": "plan-1"}) {
		t.Fatal("matching plan was rejected")
	}
	if g.skillPrerequisitesSatisfied(w, "imagery.prepare", map[string]any{"plan_id": "plan-2"}) {
		t.Fatal("wrong plan id was accepted")
	}
}

func TestSkillPrerequisitesAllowTypedAOIUICommand(t *testing.T) {
	g := &Gateway{skills: map[string]SkillManifest{"test": {Steps: []SkillStep{
		{ID: "selection", Tool: "selection.validate"},
		{ID: "set_aoi", Tool: "ui.set_existing_aoi", Requires: []string{"selection"}},
	}}}}
	aoi := map[string]any{"type": "Polygon", "coordinates": []any{[]any{[]any{114.0, 38.0}, []any{115.0, 38.0}, []any{115.0, 39.0}, []any{114.0, 39.0}, []any{114.0, 38.0}}}}
	w := &Workflow{SkillID: "test", ToolCalls: []WorkflowToolCall{{
		Tool: "selection.validate", Status: "ok",
		Input: map[string]any{"region_id": "r1", "scene_ids": []any{"s1", "s2"}, "aoi": aoi},
		Data:  map[string]any{"region_id": "r1"},
	}}}
	if !g.skillPrerequisitesSatisfied(w, "ui.set_existing_aoi", map[string]any{"aoi": aoi}) {
		t.Fatal("typed set_existing_aoi command was rejected")
	}
}

func TestSkillPrerequisitesBindMapActionsToDiscoveredResources(t *testing.T) {
	g := &Gateway{skills: map[string]SkillManifest{"test": {Steps: []SkillStep{
		{ID: "region", Tool: "region.search"},
		{ID: "scenes", Tool: "imagery.scenes"},
		{ID: "select", Tool: "ui.select_region", Requires: []string{"region"}},
		{ID: "load", Tool: "ui.load_phases", Requires: []string{"scenes"}},
		{ID: "fly", Tool: "ui.fly_to_bounds", Requires: []string{"region"}},
	}}}}
	regionBounds := []any{114.0, 38.0, 116.0, 40.0}
	phases := []any{map[string]any{"id": "r1:2025-01", "scenes": []any{map[string]any{"id": "s1"}}}}
	w := &Workflow{SkillID: "test", ToolCalls: []WorkflowToolCall{
		{Tool: "region.search", Status: "ok", Data: map[string]any{"matches": []any{map[string]any{"region_id": "r1", "bounds": regionBounds}}}},
		{Tool: "imagery.scenes", Status: "ok", Data: map[string]any{"region_id": "r1", "phases": phases, "scenes": []any{map[string]any{"id": "s1"}}}},
	}}
	for _, tc := range []struct {
		tool  string
		input map[string]any
	}{
		{"ui.select_region", map[string]any{"region_id": "r1"}},
		{"ui.load_phases", map[string]any{"phase_ids": []any{"r1:2025-01"}, "scene_ids": []any{"s1"}}},
		{"ui.fly_to_bounds", map[string]any{"bounds": regionBounds}},
	} {
		if !g.skillPrerequisitesSatisfied(w, tc.tool, tc.input) {
			t.Fatalf("discovered resource was rejected by %s", tc.tool)
		}
	}
	if g.skillPrerequisitesSatisfied(w, "ui.select_region", map[string]any{"region_id": "other"}) {
		t.Fatal("unknown region was accepted")
	}
	if g.skillPrerequisitesSatisfied(w, "ui.load_phases", map[string]any{"phase_ids": []any{"r1:2025-02"}}) {
		t.Fatal("unknown phase was accepted")
	}
	if g.skillPrerequisitesSatisfied(w, "ui.fly_to_bounds", map[string]any{"bounds": []any{0.0, 0.0, 1.0, 1.0}}) {
		t.Fatal("unknown bounds were accepted")
	}
}

func TestSkillPrerequisitesAllowSelectRegionFromSearchResult(t *testing.T) {
	g := &Gateway{skills: map[string]SkillManifest{"test": {Steps: []SkillStep{
		{ID: "region", Tool: "region.search"},
		{ID: "select", Tool: "ui.select_region", Requires: []string{"region"}},
	}}}}
	w := &Workflow{SkillID: "test", ToolCalls: []WorkflowToolCall{{
		Tool: "region.search", Status: "ok",
		Data: map[string]any{"matches": []any{map[string]any{"region_id": "r1"}}},
	}}}
	if !g.skillPrerequisitesSatisfied(w, "ui.select_region", map[string]any{"region_id": "r1"}) {
		t.Fatal("matching region was rejected")
	}
	if g.skillPrerequisitesSatisfied(w, "ui.select_region", map[string]any{"region_id": "r2"}) {
		t.Fatal("unknown region was accepted")
	}
}
