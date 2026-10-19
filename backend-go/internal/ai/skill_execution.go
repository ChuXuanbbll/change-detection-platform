package ai

import (
	"context"
	"github.com/google/uuid"
	"strings"
	"time"
)

const operationPollInterval = 100 * time.Millisecond
const operationPollLimit = 30 * time.Minute

func (g *Gateway) executeSkillTool(ctx context.Context, workflow *Workflow, call Call) Result {
	if err := ctx.Err(); err != nil {
		return Result{SchemaVersion: schemaVersion, RequestID: call.RequestID, Tool: call.Tool, ToolVersion: call.ToolVersion, Status: "error", Error: &ToolError{Code: "OPERATION_WAIT_CANCELLED", Message: "operation wait was cancelled", Retryable: true}}
	}
	result := g.registry.Invoke(ctx, call)
	if result.Status == "error" {
		return result
	}
	if call.Tool == "task.status" {
		if taskID := stringValue(call.Input["task_id"]); taskID != "" {
			if returned := dataString(result.Data, "task_id"); returned != "" && returned != taskID {
				return Result{SchemaVersion: schemaVersion, RequestID: call.RequestID, Tool: call.Tool, ToolVersion: call.ToolVersion, Status: "error", Error: &ToolError{Code: "TASK_ID_MISMATCH", Message: "task status returned a different task", Retryable: false}}
			}
			if workflow.Intent != nil && workflow.OperationIndex < len(workflow.Intent.Operations) {
				op := workflow.Intent.Operations[workflow.OperationIndex]
				if op.SkillID == "result.inspect.v1" && (stringValue(op.Slots["operation"]) == "status" || stringValue(op.Slots["operation"]) == "query_tasks") {
					return result
				}
			}
			if isPendingStatus(dataString(result.Data, "status")) {
				return g.waitForTask(ctx, call, taskID)
			}
		}
		return result
	}
	operationID := stringValue(call.Input["operation_id"])
	if call.Tool == "imagery.prepare" {
		operationID = dataString(result.Data, "operation_id")
	} else if call.Tool == "operation.status" {
		if returned := dataString(result.Data, "operation_id"); returned != "" && returned != operationID {
			return Result{SchemaVersion: schemaVersion, RequestID: call.RequestID, Tool: call.Tool, ToolVersion: call.ToolVersion, Status: "error", Error: &ToolError{Code: "OPERATION_ID_MISMATCH", Message: "operation status returned a different operation", Retryable: false}}
		}
	}
	if call.Tool == "detection.workflow.submit" {
		return result
	}
	if operationID == "" || !isPendingStatus(dataString(result.Data, "status")) {
		return result
	}
	if call.Tool != "operation.status" && call.Tool != "imagery.prepare" {
		return result
	}
	terminal, statusCall := g.waitForOperation(ctx, call, operationID)
	terminal = preserveCallIdentity(terminal, call)
	if call.Tool == "imagery.prepare" {
		workflow.ToolCalls = append(workflow.ToolCalls, WorkflowToolCall{Tool: "operation.status", Version: statusCall.ToolVersion, Status: workflowCallStatus(terminal), RequestID: statusCall.RequestID, Input: statusCall.Input, Data: terminal.Data, OperationIndex: workflow.OperationIndex})
		workflow.UpdatedAt = time.Now().UTC()
		_ = g.repo.upsertWorkflow(*workflow)
	}
	return terminal
}

func preserveCallIdentity(result Result, call Call) Result {
	result.SchemaVersion = schemaVersion
	result.RequestID = call.RequestID
	result.Tool = call.Tool
	result.ToolVersion = call.ToolVersion
	return result
}

func (g *Gateway) waitForTask(ctx context.Context, call Call, taskID string) Result {
	deadline := time.NewTimer(operationPollLimit)
	defer deadline.Stop()
	ticker := time.NewTicker(operationPollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return Result{SchemaVersion: schemaVersion, RequestID: call.RequestID, Tool: call.Tool, ToolVersion: call.ToolVersion, Status: "error", Error: &ToolError{Code: "TASK_WAIT_CANCELLED", Message: "task wait was cancelled", Retryable: true}}
		case <-deadline.C:
			return Result{SchemaVersion: schemaVersion, RequestID: call.RequestID, Tool: call.Tool, ToolVersion: call.ToolVersion, Status: "error", Error: &ToolError{Code: "TASK_TIMEOUT", Message: "task did not finish before the wait limit", Retryable: true}}
		case <-ticker.C:
			poll := call
			poll.RequestID = uuid.NewString()
			poll.Input = map[string]any{"task_id": taskID}
			result := g.registry.Invoke(ctx, poll)
			if result.Status == "error" {
				return preserveCallIdentity(result, call)
			}
			if returned := dataString(result.Data, "task_id"); returned != "" && returned != taskID {
				return Result{SchemaVersion: schemaVersion, RequestID: call.RequestID, Tool: call.Tool, ToolVersion: call.ToolVersion, Status: "error", Error: &ToolError{Code: "TASK_ID_MISMATCH", Message: "task status returned a different task", Retryable: false}}
			}
			status := dataString(result.Data, "status")
			if status == "" || !isPendingStatus(status) {
				return preserveCallIdentity(result, call)
			}
		}
	}
}

func (g *Gateway) waitForOperation(ctx context.Context, call Call, operationID string) (Result, Call) {
	statusCall := Call{SchemaVersion: callSchemaVersion, RequestID: uuid.NewString(), Tool: "operation.status", ToolVersion: "1.0.0", Input: map[string]any{"operation_id": operationID}, Context: call.Context}
	deadline := time.NewTimer(operationPollLimit)
	defer deadline.Stop()
	ticker := time.NewTicker(operationPollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return Result{SchemaVersion: schemaVersion, RequestID: call.RequestID, Tool: call.Tool, ToolVersion: call.ToolVersion, Status: "error", Error: &ToolError{Code: "OPERATION_WAIT_CANCELLED", Message: "operation wait was cancelled", Retryable: true}}, statusCall
		case <-deadline.C:
			return Result{SchemaVersion: schemaVersion, RequestID: call.RequestID, Tool: call.Tool, ToolVersion: call.ToolVersion, Status: "error", Error: &ToolError{Code: "OPERATION_TIMEOUT", Message: "operation did not finish before the wait limit", Retryable: true}}, statusCall
		case <-ticker.C:
			statusCall.RequestID = uuid.NewString()
			result := g.registry.Invoke(ctx, statusCall)
			if result.Status == "error" {
				return result, statusCall
			}
			if returned := dataString(result.Data, "operation_id"); returned != "" && returned != operationID {
				return Result{SchemaVersion: schemaVersion, RequestID: call.RequestID, Tool: call.Tool, ToolVersion: call.ToolVersion, Status: "error", Error: &ToolError{Code: "OPERATION_ID_MISMATCH", Message: "operation status returned a different operation", Retryable: false}}, statusCall
			}
			status := dataString(result.Data, "status")
			if status == "" || !isPendingStatus(status) {
				return result, statusCall
			}
		}
	}
}

func isPendingStatus(status string) bool {
	switch strings.ToLower(status) {
	case "queued", "pending", "running", "processing":
		return true
	}
	return false
}
func stringValue(value any) string { text, _ := value.(string); return text }
func dataString(data any, key string) string {
	if object, ok := data.(map[string]any); ok {
		if value, ok := object[key].(string); ok {
			return value
		}
	}
	return ""
}

func (g *Gateway) skillAllows(workflow *Workflow, tool string) bool {
	if workflow.SkillID == "" {
		return true
	}
	skill, ok := g.skills[workflow.SkillID]
	if !ok {
		return false
	}
	for _, allowed := range skill.AllowedTools {
		if allowed == tool {
			return true
		}
	}
	return false
}

func (g *Gateway) skillPrerequisitesSatisfied(workflow *Workflow, tool string, input map[string]any) bool {
	if workflow.SkillID == "" {
		return true
	}
	skill, ok := g.skills[workflow.SkillID]
	if !ok {
		return false
	}
	var step *SkillStep
	for i := range skill.Steps {
		if skill.Steps[i].Tool == tool {
			step = &skill.Steps[i]
			break
		}
	}
	if step == nil {
		// UI commands are globally typed and allowed for every Skill. A Skill may
		// add a step when it needs an explicit ordering constraint; otherwise the
		// command can be acknowledged without inventing a domain prerequisite.
		if strings.HasPrefix(tool, "ui.") {
			return true
		}
		return false
	}
	for _, prerequisite := range step.Requires {
		for _, candidate := range skill.Steps {
			if candidate.ID == prerequisite {
				prerequisite = candidate.Tool
				break
			}
		}
		satisfied := false
		for _, call := range workflow.ToolCalls {
			if call.OperationIndex == workflow.OperationIndex && call.Tool == prerequisite && call.Status == "ok" && resourceMatches(call, tool, input) {
				satisfied = true
				break
			}
		}
		if !satisfied {
			return false
		}
	}
	return true
}

func resourceMatches(prior WorkflowToolCall, nextTool string, next map[string]any) bool {
	if prior.Data == nil {
		return false
	}
	switch prior.Tool {
	case "region.search":
		if nextTool == "ui.fly_to_bounds" {
			for _, match := range dataRows(prior.Data, "matches") {
				if next["bounds"] != nil && sameJSON(next["bounds"], mapValue(match)["bounds"]) {
					return true
				}
			}
			return false
		}
		regionID := stringValue(next["region_id"])
		for _, match := range dataRows(prior.Data, "matches") {
			if regionID != "" && dataString(match, "region_id") == regionID {
				return true
			}
		}
		return false
	case "imagery.scenes":
		if nextTool == "ui.load_phase" || nextTool == "ui.load_phases" {
			phaseIDs := dataStrings(next, "phase_ids")
			if phaseID := stringValue(next["phase_id"]); phaseID != "" {
				phaseIDs = []string{phaseID}
			}
			if !allStringsPresent(phaseIDs, resourceIDs(prior.Data, "phases")) {
				return false
			}
			if ids := dataStrings(next, "scene_ids"); len(ids) > 0 {
				return allStringsPresent(ids, resourceIDs(prior.Data, "scenes"))
			}
			return true
		}
		if stringValue(next["region_id"]) != stringValue(prior.Input["region_id"]) {
			return false
		}
		if ids := dataStrings(next, "scene_ids"); len(ids) > 0 {
			return allStringsPresent(ids, resourceIDs(prior.Data, "scenes"))
		}
		if phaseID := stringValue(next["phase_id"]); phaseID != "" {
			return containsString(resourceIDs(prior.Data, "phases"), phaseID)
		}
		return true
	case "selection.validate":
		// UI actions intentionally carry only the resource they apply. The
		// selection validator already bound the AOI to a region and scene set;
		// requiring those internal fields on set_existing_aoi would reject a
		// valid typed UI command because they are not part of its public schema.
		if nextTool == "ui.set_existing_aoi" {
			if next["aoi"] == nil || prior.Input["aoi"] == nil {
				return false
			}
			if !sameJSON(next["aoi"], prior.Input["aoi"]) {
				return false
			}
			if _, hasBounds := prior.Input["bounds"]; hasBounds {
				return sameJSON(next["bounds"], prior.Input["bounds"])
			}
			return true
		}
		if rid := stringValue(next["region_id"]); rid == "" || rid != dataString(prior.Data, "region_id") {
			return false
		}
		return sameStringSet(next["scene_ids"], prior.Input["scene_ids"]) && sameJSON(next["aoi"], prior.Input["aoi"]) && sameJSON(next["bounds"], prior.Input["bounds"])
	case "detection.plan.create":
		if planID := stringValue(next["plan_id"]); planID != "" {
			return planID == dataString(prior.Data, "plan_id")
		}
		return stringValue(next["region_id"]) == stringValue(prior.Input["region_id"]) && sameStringSet(next["scene_ids"], prior.Input["scene_ids"])
	case "imagery.prepare":
		return stringValue(next["operation_id"]) == dataString(prior.Data, "operation_id")
	case "operation.status":
		return stringValue(next["operation_id"]) != "" && stringValue(next["operation_id"]) == dataString(prior.Data, "operation_id")
	case "detection.workflow.submit":
		id := stringValue(next["task_id"])
		if id == "" {
			return false
		}
		for _, candidate := range dataStrings(prior.Data, "task_ids") {
			if candidate == id {
				return true
			}
		}
		return false
	case "task.status":
		return stringValue(next["task_id"]) == dataString(prior.Data, "task_id")
	case "task.search":
		taskID := stringValue(next["task_id"])
		for _, item := range dataRows(prior.Data, "items") {
			if taskID != "" && dataString(item, "task_id") == taskID {
				return true
			}
		}
		return false
	case "result.summary":
		if nextTool == "ui.show_result" || nextTool == "ui.open_report" {
			if id := stringValue(next["result_id"]); id != "" {
				return id == dataString(prior.Data, "result_id")
			}
		}
		return stringValue(next["task_id"]) == dataString(prior.Data, "task_id")
	case "ui.select_region":
		return stringValue(next["region_id"]) == stringValue(prior.Input["region_id"])
	case "ui.load_phases":
		phaseIDs := resourceIDs(prior.Data, "phases")
		if phaseID := stringValue(next["phase_id"]); phaseID != "" {
			return containsString(phaseIDs, phaseID)
		}
		return allStringsPresent(dataStrings(next, "phase_ids"), phaseIDs)
	case "ui.fly_to_bounds":
		for _, match := range dataRows(prior.Data, "matches") {
			if sameJSON(next["bounds"], mapValue(match)["bounds"]) {
				return true
			}
		}
		return false
	case "ui.show_task":
		return stringValue(next["task_id"]) == dataString(prior.Data, "task_id")
	case "ui.show_result", "ui.open_report":
		resultID := stringValue(next["result_id"])
		if resultID == "" {
			resultID = stringValue(next["task_id"])
		}
		return resultID != "" && (resultID == dataString(prior.Data, "result_id") || resultID == dataString(prior.Data, "task_id"))
	}
	return true
}

func mapValue(value any) map[string]any {
	if object, ok := value.(map[string]any); ok {
		return object
	}
	return map[string]any{}
}

func dataRows(data any, key string) []any {
	rows, _ := mapValue(data)[key].([]any)
	return rows
}

func resourceIDs(data any, field string) []string {
	ids := make([]string, 0)
	for _, row := range dataRows(data, field) {
		id := dataString(row, "phase_id")
		if id == "" {
			id = dataString(row, "id")
		}
		if id == "" {
			id = dataString(row, "scene_id")
		}
		if id != "" {
			ids = append(ids, id)
		}
	}
	return ids
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func allStringsPresent(requested, available []string) bool {
	if len(requested) == 0 {
		return false
	}
	for _, value := range requested {
		if !containsString(available, value) {
			return false
		}
	}
	return true
}

func sameStringSet(left, right any) bool {
	leftValues, rightValues := dataStrings(map[string]any{"items": left}, "items"), dataStrings(map[string]any{"items": right}, "items")
	if len(leftValues) == 0 || len(leftValues) != len(rightValues) {
		return false
	}
	seen := make(map[string]bool, len(leftValues))
	for _, value := range leftValues {
		seen[value] = true
	}
	for _, value := range rightValues {
		if !seen[value] {
			return false
		}
	}
	return true
}
func dataStrings(data any, key string) []string {
	object, _ := data.(map[string]any)
	switch values := object[key].(type) {
	case []any:
		output := make([]string, 0, len(values))
		for _, value := range values {
			if stringValue(value) != "" {
				output = append(output, stringValue(value))
			}
		}
		return output
	case []string:
		return append([]string(nil), values...)
	}
	return nil
}
