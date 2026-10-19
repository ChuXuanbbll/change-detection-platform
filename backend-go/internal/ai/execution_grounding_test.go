package ai

import "testing"

func TestOperationAOIMustHaveCurrentEvidence(t *testing.T) {
	known := map[string]any{"type": "Polygon", "coordinates": []any{[]any{[]any{114, 38}, []any{115, 38}, []any{115, 39}, []any{114, 38}}}}
	other := map[string]any{"type": "Polygon", "coordinates": []any{[]any{[]any{113, 38}, []any{114, 38}, []any{114, 39}, []any{113, 38}}}}
	workflow := &Workflow{OperationIndex: 1, ToolCalls: []WorkflowToolCall{{Tool: "region.search", Status: "ok", OperationIndex: 1, Data: map[string]any{"matches": []any{map[string]any{"region_id": "r1", "geometry": known, "aoi_supported": true}}}}}}
	op := IntentOperation{Slots: map[string]any{}}
	input := map[string]any{"region_id": "r1", "aoi": known}
	if !operationAOIGrounded(workflow, op, nil, input) {
		t.Fatal("verified current administrative polygon was rejected")
	}
	workflow.ToolCalls[0].OperationIndex = 0
	if operationAOIGrounded(workflow, op, nil, input) {
		t.Fatal("prior operation unlocked an invented selection")
	}
	if !operationAOIGrounded(workflow, op, map[string]any{"aoi": known}, input) {
		t.Fatal("existing workspace polygon was rejected")
	}
	op.Slots["aoi"] = other
	if operationAOIGrounded(workflow, op, map[string]any{"aoi": known}, input) {
		t.Fatal("explicit new polygon was silently replaced by workspace polygon")
	}
	input["aoi"] = other
	if !operationAOIGrounded(workflow, op, nil, input) {
		t.Fatal("explicit normalized polygon was rejected")
	}
}
