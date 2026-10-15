package ai

import (
	"encoding/json"
	"io"
	"strings"
)

// ExecutionClarification stops an operation and the remainder of its composed
// workflow. It is an expected user-input state, not a provider failure.
type ExecutionClarification struct{ Question string }

func (e *ExecutionClarification) Error() string { return e.Question }

func clarificationProviderTool() ProviderTool {
	return ProviderTool{Type: "function", Function: ProviderFunction{
		Name:        "request_clarification",
		Description: "当当前操作缺少必要条件或引用不唯一时，提出一个简短中文问题并立即暂停当前及后续操作。不能把澄清当作成功完成。",
		Parameters: map[string]any{
			"$schema": "https://json-schema.org/draft/2020-12/schema",
			"type":    "object", "additionalProperties": false,
			"properties": map[string]any{"question": map[string]any{"type": "string", "minLength": 1, "maxLength": 1000}},
			"required":   []any{"question"},
		},
	}}
}

func decodeExecutionClarification(call ProviderToolCall) error {
	if call.Function.Name != "request_clarification" || len(call.Function.Arguments) > 8192 {
		return invalidIntent()
	}
	decoder := json.NewDecoder(strings.NewReader(call.Function.Arguments))
	value, err := decodeIntentJSON(decoder, 0)
	if err != nil || validateIntentSlots(clarificationProviderTool().Function.Parameters, value) != nil {
		return invalidIntent()
	}
	if _, err := decoder.Token(); err != io.EOF {
		return invalidIntent()
	}
	object, ok := value.(map[string]any)
	if !ok {
		return invalidIntent()
	}
	question := strings.TrimSpace(stringValue(object["question"]))
	if question == "" || len([]rune(question)) > 1000 {
		return invalidIntent()
	}
	return &ExecutionClarification{Question: question}
}

// verifiedOperationRefs exposes only identifiers returned by one operation's
// successful calls. Queued submission has already created durable tasks and is
// a valid source of references, even though it is not a completed computation.
func verifiedOperationRefs(workflow *Workflow, index int) map[string]any {
	refs := make(map[string]any)
	if workflow == nil || index < 0 {
		return refs
	}
	taskIDs := make([]string, 0)
	for _, call := range workflow.ToolCalls {
		if call.OperationIndex != index || !operationCallHasResult(call) {
			continue
		}
		data := mapValue(normalizeJSONValue(call.Data))
		for _, key := range []string{"task_id", "result_id", "workflow_id", "operation_id", "download_ref"} {
			if value := stringValue(data[key]); value != "" {
				refs[key] = value
				if key == "task_id" && !containsString(taskIDs, value) {
					taskIDs = append(taskIDs, value)
				}
			}
		}
		for _, id := range dataStrings(data, "task_ids") {
			if !containsString(taskIDs, id) {
				taskIDs = append(taskIDs, id)
			}
		}
	}
	if len(taskIDs) > 0 {
		refs["task_ids"] = taskIDs
		if len(taskIDs) == 1 {
			refs["task_id"] = taskIDs[0]
		} else {
			delete(refs, "task_id")
		}
	}
	return refs
}

func operationCallHasResult(call WorkflowToolCall) bool {
	if call.Data == nil {
		return false
	}
	if call.Status == "ok" {
		return true
	}
	return call.Status == "pending" && (call.Tool == "detection.workflow.submit" || call.Tool == "task.status")
}

// operationCompleted guards composed execution. A model's plain-text claim is
// not sufficient to unlock later operations; the requested business goal must
// have a matching successful tool result in this operation.
func operationCompleted(workflow *Workflow, operation IntentOperation) bool {
	if workflow == nil {
		return false
	}
	targets := []string{}
	switch operation.SkillID {
	case "change.detect.v1":
		targets = []string{"detection.workflow.submit", "ui.submit_detection"}
	case "imagery.explore.v1":
		switch stringValue(operation.Slots["operation"]) {
		case "query":
			targets = []string{"imagery.catalog", "imagery.scenes"}
		case "locate":
			targets = []string{"ui.fly_to_bounds", "ui.select_region"}
		case "models":
			targets = []string{"model.list"}
		case "images":
			targets = []string{"image.list"}
		case "phase":
			targets = []string{"imagery.phase"}
		}
	case "result.inspect.v1":
		switch stringValue(operation.Slots["operation"]) {
		case "query_tasks":
			targets = []string{"task.search"}
		case "status":
			targets = []string{"task.status"}
		case "summary":
			targets = []string{"result.summary"}
		case "filter":
			targets = []string{"result.patches.query"}
		case "export":
			targets = []string{"result.export.prepare"}
		}
	}
	for _, call := range workflow.ToolCalls {
		if call.OperationIndex != workflow.OperationIndex || !containsString(targets, call.Tool) || !operationCallHasResult(call) {
			continue
		}
		data := mapValue(normalizeJSONValue(call.Data))
		if call.Tool == "detection.workflow.submit" && (stringValue(data["workflow_id"]) == "" || len(dataStrings(data, "task_ids")) == 0) {
			continue
		}
		if call.Tool == "ui.submit_detection" && (stringValue(data["task_id"]) == "" || data["pair_count"] == nil) {
			continue
		}
		if call.Tool == "result.export.prepare" && stringValue(data["download_ref"]) == "" {
			continue
		}
		return true
	}
	return false
}

// bindPreviousOperationRefs resolves a dependent operation without falling
// back to an older workspace task. Multiple submitted tasks require a choice.
func bindPreviousOperationRefs(workflow *Workflow, index int, operation IntentOperation) (IntentOperation, error) {
	inherited, _ := operation.Slots["use_previous_result"].(bool)
	if !inherited {
		return operation, nil
	}
	if index <= 0 || workflow == nil {
		return IntentOperation{}, &ExecutionClarification{Question: "请指定要使用的任务或结果。"}
	}
	for _, call := range workflow.ToolCalls {
		if call.OperationIndex == index-1 && call.Tool == "ui.submit_detection" && operationCallHasResult(call) {
			if count, ok := number(mapValue(normalizeJSONValue(call.Data))["pair_count"]); ok && count > 1 {
				return IntentOperation{}, &ExecutionClarification{Question: "上一操作提交了多组时相检测，请在工作区选择要查看或导出的任务。"}
			}
		}
	}
	refs := verifiedOperationRefs(workflow, index-1)
	operation.Slots = cloneMap(operation.Slots)
	if operation.SkillID != "result.inspect.v1" {
		return IntentOperation{}, &ExecutionClarification{Question: "上一操作没有提供该步骤所需的影像或区域，请明确选择资源。"}
	}
	taskIDs := dataStrings(refs, "task_ids")
	if len(taskIDs) > 1 {
		return IntentOperation{}, &ExecutionClarification{Question: "上一操作创建了多个检测任务，请指定要查看或导出的任务。"}
	}
	taskID := stringValue(refs["task_id"])
	if taskID == "" {
		return IntentOperation{}, &ExecutionClarification{Question: "上一操作尚未提供可用的检测任务，请先指定任务或等待检测提交。"}
	}
	if existing := stringValue(operation.Slots["task_id"]); existing != "" && existing != taskID {
		return IntentOperation{}, &ExecutionClarification{Question: "指定任务与上一操作的任务不同，请明确要使用哪一个。"}
	}
	resultID := stringValue(refs["result_id"])
	if existing := stringValue(operation.Slots["result_id"]); existing != "" && existing != resultID {
		return IntentOperation{}, &ExecutionClarification{Question: "指定结果与上一操作的结果不同，请明确要使用哪一个。"}
	}
	operation.Slots["task_id"] = taskID
	if resultID != "" {
		operation.Slots["result_id"] = resultID
	}
	delete(operation.Slots, "latest")
	delete(operation.Slots, "use_workspace_context")
	delete(operation.Slots, "use_previous_result")
	return operation, nil
}
