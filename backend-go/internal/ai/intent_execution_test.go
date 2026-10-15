package ai

import (
	"errors"
	"strings"
	"testing"
)

func TestExecutionClarificationStrictlyValidatesQuestion(t *testing.T) {
	tool := clarificationProviderTool()
	if err := validateProviderParameters(tool.Function.Parameters); err != nil {
		t.Fatal(err)
	}
	call := ProviderToolCall{Type: "function"}
	call.Function.Name = "request_clarification"
	call.Function.Arguments = `{"question":"  请绘制检测范围。  "}`
	var clarification *ExecutionClarification
	if err := decodeExecutionClarification(call); !errors.As(err, &clarification) || clarification.Question != "请绘制检测范围。" {
		t.Fatalf("clarification = %v", err)
	}
	for _, raw := range []string{
		`{}`, `{"question":""}`, `{"question":"  "}`, `{"question":123}`,
		`{"question":"指定任务？","task_id":"untrusted"}`,
		`{"question":"甲","question":"乙"}`, `{"question":"任务？"} invalid`,
		`{"question":"任务？"} {}`, `{"question":"` + strings.Repeat("问", 1001) + `"}`,
	} {
		call.Function.Arguments = raw
		var safe *IntentError
		if err := decodeExecutionClarification(call); !errors.As(err, &safe) {
			t.Fatalf("accepted unsafe clarification %q: %v", raw, err)
		}
	}
}

func TestVerifiedOperationRefsIgnoreOtherAndFailedOperations(t *testing.T) {
	workflow := &Workflow{ToolCalls: []WorkflowToolCall{
		{OperationIndex: 0, Tool: "result.summary", Status: "ok", Data: map[string]any{"task_id": "old-task", "result_id": "old-result"}},
		{OperationIndex: 1, Tool: "detection.workflow.submit", Status: "pending", Data: map[string]any{"workflow_id": "domain-workflow", "task_ids": []string{"new-task"}}},
		{OperationIndex: 1, Tool: "result.summary", Status: "error", Data: map[string]any{"task_id": "failed-task", "result_id": "failed-result"}},
		{OperationIndex: 1, Tool: "imagery.prepare", Status: "pending", Data: map[string]any{"operation_id": "unready-preparation"}},
	}}
	refs := verifiedOperationRefs(workflow, 1)
	if stringValue(refs["task_id"]) != "new-task" || stringValue(refs["workflow_id"]) != "domain-workflow" || refs["result_id"] != nil || refs["operation_id"] != nil {
		t.Fatalf("unverified reference exposed: %+v", refs)
	}
	if len(verifiedOperationRefs(nil, 0)) != 0 || len(verifiedOperationRefs(workflow, -1)) != 0 {
		t.Fatal("invalid operation leaked references")
	}
}

func TestOperationCompletedRequiresRequestedBusinessResult(t *testing.T) {
	for _, test := range []struct {
		skill, operation, tool string
		data                   map[string]any
	}{
		{"change.detect.v1", "detect", "detection.workflow.submit", map[string]any{"workflow_id": "w1", "task_ids": []any{"t1"}}},
		{"imagery.explore.v1", "query", "imagery.scenes", map[string]any{"scenes": []any{}}},
		{"imagery.explore.v1", "locate", "ui.fly_to_bounds", map[string]any{"status": "applied"}},
		{"imagery.explore.v1", "models", "model.list", map[string]any{"items": []any{}}},
		{"imagery.explore.v1", "images", "image.list", map[string]any{"items": []any{}}},
		{"imagery.explore.v1", "phase", "imagery.phase", map[string]any{"id": "p1"}},
		{"result.inspect.v1", "query_tasks", "task.search", map[string]any{"items": []any{}}},
		{"result.inspect.v1", "status", "task.status", map[string]any{"task_id": "t1", "status": "running"}},
		{"result.inspect.v1", "summary", "result.summary", map[string]any{"task_id": "t1"}},
		{"result.inspect.v1", "filter", "result.patches.query", map[string]any{"items": []any{}}},
		{"result.inspect.v1", "export", "result.export.prepare", map[string]any{"download_ref": "download-1"}},
	} {
		t.Run(test.operation, func(t *testing.T) {
			operation := IntentOperation{SkillID: test.skill, Slots: map[string]any{"operation": test.operation}}
			workflow := &Workflow{OperationIndex: 1, ToolCalls: []WorkflowToolCall{
				{OperationIndex: 0, Tool: test.tool, Status: "ok", Data: test.data},
				{OperationIndex: 1, Tool: "workspace.context", Status: "ok", Data: map[string]any{}},
			}}
			if operationCompleted(workflow, operation) {
				t.Fatal("earlier operation or workspace context satisfied business goal")
			}
			workflow.ToolCalls = append(workflow.ToolCalls, WorkflowToolCall{OperationIndex: 1, Tool: test.tool, Status: "error", Data: test.data})
			if operationCompleted(workflow, operation) {
				t.Fatal("failed tool satisfied business goal")
			}
			workflow.ToolCalls[len(workflow.ToolCalls)-1].Status = "ok"
			if !operationCompleted(workflow, operation) {
				t.Fatal("matching operation result rejected")
			}
		})
	}
	workflow := &Workflow{ToolCalls: []WorkflowToolCall{{Tool: "detection.workflow.submit", Status: "pending", Data: map[string]any{"task_ids": []string{"t1"}, "workflow_id": "w1"}}}}
	if !operationCompleted(workflow, IntentOperation{SkillID: "change.detect.v1", Slots: map[string]any{"operation": "detect"}}) {
		t.Fatal("already-submitted durable detection not recognized")
	}
}

func TestBindPreviousOperationRefsUsesOnlyUniqueFreshTask(t *testing.T) {
	operation := IntentOperation{SkillID: "result.inspect.v1", Slots: map[string]any{"operation": "export", "latest": true, "use_workspace_context": true, "use_previous_result": true}}
	workflow := &Workflow{ToolCalls: []WorkflowToolCall{{OperationIndex: 0, Tool: "detection.workflow.submit", Status: "pending", Data: map[string]any{"task_ids": []string{"new-task"}, "workflow_id": "new-workflow"}}}}
	bound, err := bindPreviousOperationRefs(workflow, 1, operation)
	if err != nil || bound.Slots["task_id"] != "new-task" || bound.Slots["latest"] != nil || bound.Slots["use_workspace_context"] != nil {
		t.Fatalf("bound = %+v, err = %v", bound, err)
	}
	if operation.Slots["task_id"] != nil || operation.Slots["latest"] != true {
		t.Fatal("binding mutated the parsed intent")
	}
	for _, test := range []struct {
		name string
		w    *Workflow
		op   IntentOperation
	}{
		{"no result", &Workflow{}, operation},
		{"ambiguous", &Workflow{ToolCalls: []WorkflowToolCall{{Tool: "detection.workflow.submit", Status: "pending", Data: map[string]any{"task_ids": []string{"t1", "t2"}}}}}, operation},
		{"wrong task", workflow, IntentOperation{SkillID: "result.inspect.v1", Slots: map[string]any{"use_previous_result": true, "task_id": "old-task"}}},
		{"wrong resource", workflow, IntentOperation{SkillID: "imagery.explore.v1", Slots: map[string]any{"use_previous_result": true}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			var clarification *ExecutionClarification
			if _, err := bindPreviousOperationRefs(test.w, 1, test.op); !errors.As(err, &clarification) {
				t.Fatalf("unsafe inheritance did not stop: %v", err)
			}
		})
	}
}
