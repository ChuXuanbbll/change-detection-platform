package ai

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

var uiActions = []string{"select_region", "load_phase", "load_phases", "set_existing_aoi", "fly_to_bounds", "show_task", "show_result", "open_report", "submit_detection"}

func uiToolSpecs() []ToolSpec {
	result := make([]ToolSpec, 0, len(uiActions))
	for _, action := range uiActions {
		result = append(result, ToolSpec{Name: "ui." + action, Version: "1.0.0", Description: "Request a typed workspace UI action and wait for its acknowledgement.", Effect: "workspace", MaxOutputSize: 16384, InputSchema: uiInputSchemaForAction(action), OutputSchema: uiOutputSchema()})
		if action == "submit_detection" {
			result[len(result)-1].Description = "Submit detection through the same workspace command as the Start detection button. The server binds normalized dates, AOI and parameter overrides; current workspace values are preserved otherwise. Returns a created task, not completed inference. Do not resubmit while running or after an uncertain acknowledgement."
		}
	}
	return result
}

func uiInputSchema() map[string]any {
	return map[string]any{"$schema": "https://json-schema.org/draft/2020-12/schema", "type": "object", "additionalProperties": false, "properties": map[string]any{"region_id": map[string]any{"type": "string", "minLength": 1}, "phase_id": map[string]any{"type": "string", "minLength": 1}, "phase_ids": map[string]any{"type": "array", "items": map[string]any{"type": "string", "minLength": 1}, "minItems": 1, "maxItems": 128}, "scene_ids": map[string]any{"type": "array", "items": map[string]any{"type": "string", "minLength": 1}, "minItems": 1, "maxItems": 128}, "source": map[string]any{"type": "string", "enum": []any{"admin_boundary", "user_drawn", "workspace"}}, "bounds": map[string]any{"type": "array", "minItems": 4, "maxItems": 4, "items": map[string]any{"type": "number"}}, "aoi": uiAOISchema(), "task_id": map[string]any{"type": "string", "minLength": 1}, "result_id": map[string]any{"type": "string", "minLength": 1}, "expected_revision": map[string]any{"type": "string", "minLength": 1}}}
}

func uiInputSchemaForAction(action string) map[string]any {
	base := uiInputSchema()
	properties := base["properties"].(map[string]any)
	properties["admin_code"] = map[string]any{"type": "string", "pattern": "^[0-9]{6}$"}
	allowed := map[string][]string{
		"submit_detection": {"expected_revision"},
		"select_region":    {"region_id", "admin_code", "expected_revision"},
		"load_phase":       {"phase_id", "phase_ids", "scene_ids", "expected_revision"},
		"load_phases":      {"phase_id", "phase_ids", "scene_ids", "expected_revision"},
		"set_existing_aoi": {"aoi", "source", "bounds", "expected_revision"},
		"fly_to_bounds":    {"bounds", "expected_revision"},
		"show_task":        {"task_id", "expected_revision"},
		"show_result":      {"result_id", "expected_revision"},
		"open_report":      {"task_id", "result_id", "expected_revision"},
	}
	filtered := make(map[string]any)
	for _, name := range allowed[action] {
		filtered[name] = properties[name]
	}
	base["properties"] = filtered
	return base
}

func uiOutputSchema() map[string]any {
	return map[string]any{"$schema": "https://json-schema.org/draft/2020-12/schema", "type": "object", "additionalProperties": false, "properties": map[string]any{"status": map[string]any{"type": "string", "enum": []any{"applied", "rejected", "failed", "timeout", "cancelled"}}, "action_id": map[string]any{"type": "string", "minLength": 1}, "context_revision": map[string]any{"type": "string"}, "workspace": map[string]any{"type": "object"}, "task_id": map[string]any{"type": "string"}, "pair_count": map[string]any{"type": "integer", "minimum": 1}, "error_code": map[string]any{"type": "string"}}, "required": []any{"status", "action_id"}}
}

func uiAOISchema() map[string]any {
	return map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{
		"type":        map[string]any{"type": "string", "enum": []any{"Polygon"}},
		"coordinates": map[string]any{"type": "array", "minItems": 1, "maxItems": 1, "items": map[string]any{"type": "array", "minItems": 4, "items": map[string]any{"type": "array", "minItems": 2, "maxItems": 2, "items": map[string]any{"type": "number"}}}},
	}, "required": []any{"type", "coordinates"}}
}

func (g *Gateway) handleUICommand(ctx context.Context, session *Session, workflow *Workflow, w io.Writer, call ProviderToolCall, args map[string]any) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	action := call.Function.Name[len("ui."):]
	allowed := false
	for _, candidate := range uiActions {
		if action == candidate {
			allowed = true
			break
		}
	}
	if !allowed {
		return fmt.Errorf("unsupported UI action")
	}
	if err := validateUIArgs(action, args); err != nil {
		return err
	}
	if action == "select_region" && stringValue(args["admin_code"]) != "" {
		found := false
		for _, prior := range workflow.ToolCalls {
			if prior.OperationIndex != workflow.OperationIndex || prior.Tool != "region.search" || prior.Status != "ok" {
				continue
			}
			for _, row := range dataRows(prior.Data, "matches") {
				m := mapValue(row)
				if stringValue(m["admin_code"]) == stringValue(args["admin_code"]) && stringValue(m["region_id"]) == stringValue(args["region_id"]) {
					found = true
				}
			}
		}
		if !found {
			return fmt.Errorf("county must be resolved by region.search before selection")
		}
	}
	if action == "submit_detection" {
		if workflow.Intent == nil || workflow.OperationIndex >= len(workflow.Intent.Operations) {
			return fmt.Errorf("detection requires a normalized operation")
		}
		op := workflow.Intent.Operations[workflow.OperationIndex]
		if op.SkillID != "change.detect.v1" || stringValue(op.Slots["operation"]) != "detect" {
			return fmt.Errorf("detection is not requested")
		}
		var county map[string]any
		if len(dataStrings(op.Slots, "admin_path")) > 0 {
			resolved := false
			for _, prior := range workflow.ToolCalls {
				if prior.OperationIndex != workflow.OperationIndex || prior.Tool != "region.search" || prior.Status != "ok" || !sameJSON(normalizeJSONValue(prior.Input["admin_path"]), normalizeJSONValue(op.Slots["admin_path"])) || len(dataRows(prior.Data, "matches")) != 1 {
					continue
				}
				for _, row := range dataRows(prior.Data, "matches") {
					if stringValue(mapValue(row)["region_id"]) == stringValue(session.Workspace["region_id"]) {
						resolved = true
						if stringValue(mapValue(row)["admin_code"]) != "" && stringValue(mapValue(row)["level"]) == "county" && stringValue(mapValue(row)["boundary_version"]) != "" {
							county = mapValue(row)
						}
					}
				}
			}
			if !resolved {
				return fmt.Errorf("resolve the requested administrative path and select its region before detection")
			}
		}
		args = cloneMap(args)
		for _, key := range []string{"region_id", "phase_dates", "phase_ids", "scene_ids", "date_range", "pair_policy", "aoi", "model_id", "detection_settings"} {
			if value, ok := op.Slots[key]; ok {
				args[key] = value
			}
		}
		useBoundary, _ := op.Slots["use_admin_boundary"].(bool)
		if useBoundary && county == nil {
			return fmt.Errorf("resolve one county boundary before detection")
		}
		if useBoundary && county != nil {
			args["admin_code"] = county["admin_code"]
			args["admin_version"] = county["boundary_version"]
			args["region_id"] = county["region_id"]
			// Resolve geometry in the shared service; never reconstruct a county.
			delete(args, "aoi")
		}
	}
	actionID := uuid.NewString()
	expected := workspaceRevision(session.Workspace)
	if value, ok := args["expected_revision"].(string); ok {
		expected = value
	}
	command := UICommand{Action: action, ActionID: actionID, Args: args, ExpectedRevision: expected}
	encodedArgs, _ := json.Marshal(map[string]any{"action": action, "args": args, "expected_revision": expected})
	fingerprint := fmt.Sprintf("%x", sha256.Sum256(encodedArgs))
	for _, existing := range session.Actions {
		// Submission is irreversible once accepted. An uncertain or successful
		// acknowledgement in this operation must never create another task.
		if action == "submit_detection" && existing.WorkflowID == workflow.ID && existing.OperationIndex == workflow.OperationIndex && existing.Command.Action == action && existing.Status != "rejected" {
			if existing.Status == "applied" {
				return nil
			}
			return fmt.Errorf("detection submission already pending or unconfirmed; inspect workspace task")
		}
		if existing.WorkflowID == workflow.ID && existing.OperationIndex == workflow.OperationIndex && existing.Fingerprint == fingerprint {
			if existing.Status == "applied" {
				return nil
			}
			if existing.Status == "pending" {
				return fmt.Errorf("UI action is already pending")
			}
		}
	}
	pending := PendingAction{OperationIndex: workflow.OperationIndex, ID: actionID, WorkflowID: workflow.ID, Command: command, Status: "pending", Fingerprint: fingerprint, CreatedAt: time.Now().UTC()}
	g.requestMu.Lock()
	if current, ok := g.repo.getSession(session.ID); !ok || current.ClosedAt != nil || ctx.Err() != nil {
		g.requestMu.Unlock()
		return context.Canceled
	}
	if session.Actions == nil {
		session.Actions = make(map[string]PendingAction)
	}
	session.Actions[actionID] = pending
	session.UpdatedAt = time.Now().UTC()
	if err := g.repo.upsertSession(*session); err != nil {
		g.requestMu.Unlock()
		return err
	}
	ackCh := make(chan ActionAck, 1)
	g.ackMu.Lock()
	g.acks[actionID] = ackCh
	g.ackMu.Unlock()
	defer func() { g.ackMu.Lock(); delete(g.acks, actionID); g.ackMu.Unlock() }()
	g.emitEvent(workflow, w, "ui_command", command)
	g.requestMu.Unlock()
	wait := 30 * time.Second
	if action == "submit_detection" {
		wait = operationPollLimit
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case ack := <-ackCh:
		if err := ctx.Err(); err != nil {
			return err
		}
		pending.Status = ack.Status
		pending.Ack = &ack
		now := time.Now().UTC()
		pending.AcknowledgedAt = &now
		session.Actions[actionID] = pending
		if ack.Workspace != nil {
			session.Workspace = ack.Workspace
		}
		_ = g.repo.upsertSession(*session)
		if ack.Status != "applied" {
			return fmt.Errorf("UI action was not applied")
		}
		return nil
	case <-timer.C:
		pending.Status = "timeout"
		session.Actions[actionID] = pending
		_ = g.repo.upsertSession(*session)
		return fmt.Errorf("UI action acknowledgement timed out")
	case <-ctx.Done():
		pending.Status = "cancelled"
		session.Actions[actionID] = pending
		_ = g.repo.upsertSession(*session)
		return ctx.Err()
	}
}

func latestOperationAction(session *Session, workflow *Workflow, action string) PendingAction {
	latest := PendingAction{}
	for _, candidate := range session.Actions {
		if candidate.WorkflowID == workflow.ID && candidate.OperationIndex == workflow.OperationIndex && candidate.Command.Action == action && (latest.ID == "" || candidate.CreatedAt.After(latest.CreatedAt)) {
			latest = candidate
		}
	}
	return latest
}

func validateUIArgs(action string, args map[string]any) error {
	allowed := map[string]map[string]bool{
		"submit_detection": {"expected_revision": true},
		"select_region":    {"region_id": true, "admin_code": true, "expected_revision": true},
		"load_phase":       {"phase_id": true, "phase_ids": true, "scene_ids": true, "expected_revision": true},
		"load_phases":      {"phase_id": true, "phase_ids": true, "scene_ids": true, "expected_revision": true},
		"set_existing_aoi": {"aoi": true, "source": true, "bounds": true, "expected_revision": true},
		"fly_to_bounds":    {"bounds": true, "expected_revision": true},
		"show_task":        {"task_id": true, "expected_revision": true},
		"show_result":      {"result_id": true, "expected_revision": true},
		"open_report":      {"task_id": true, "result_id": true, "expected_revision": true},
	}
	for key := range args {
		if !allowed[action][key] {
			return fmt.Errorf("UI action %s does not accept %s", action, key)
		}
	}
	requireString := func(key string) error {
		value, ok := args[key].(string)
		if !ok || value == "" {
			return fmt.Errorf("UI action %s requires %s", action, key)
		}
		return nil
	}
	switch action {
	case "submit_detection":
		return nil
	case "select_region":
		return requireString("region_id")
	case "load_phase", "load_phases":
		if value, ok := args["phase_id"].(string); ok && value != "" {
			return nil
		}
		values, ok := args["phase_ids"].([]any)
		if !ok || len(values) == 0 {
			return fmt.Errorf("UI action %s requires phase_id or phase_ids", action)
		}
		return nil
	case "set_existing_aoi":
		if _, ok := args["aoi"].(map[string]any); !ok {
			return fmt.Errorf("UI action set_existing_aoi requires aoi")
		}
		return nil
	case "fly_to_bounds":
		values, ok := args["bounds"].([]any)
		if !ok || len(values) != 4 {
			return fmt.Errorf("UI action fly_to_bounds requires bounds")
		}
		return nil
	case "show_task":
		return requireString("task_id")
	case "show_result":
		return requireString("result_id")
	case "open_report":
		if _, ok := args["result_id"].(string); ok {
			return nil
		}
		return requireString("task_id")
	default:
		return fmt.Errorf("unsupported UI action")
	}
}

func (g *Gateway) actionAck(c *gin.Context) {
	g.requestMu.Lock()
	defer g.requestMu.Unlock()
	var ack ActionAck
	decoder := json.NewDecoder(io.LimitReader(c.Request.Body, 64<<10))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&ack); err != nil {
		c.JSON(400, gin.H{"error": "invalid action acknowledgement"})
		return
	}
	id := c.Param("actionID")
	session, ok := g.repo.getSession(c.Param("id"))
	if !ok {
		c.JSON(404, gin.H{"error": "session not found"})
		return
	}
	if session.ClosedAt != nil {
		c.JSON(http.StatusConflict, gin.H{"code": "AI_SESSION_CLOSED", "error": "session is closed"})
		return
	}
	pending, ok := session.Actions[id]
	if !ok {
		c.JSON(404, gin.H{"error": "action not found"})
		return
	}
	if workflow, exists := g.repo.getWorkflow(pending.WorkflowID); exists && workflow.Status == "cancelled" {
		c.JSON(http.StatusConflict, gin.H{"error": "workflow is cancelled"})
		return
	}
	if pending.Status != "pending" {
		if pending.Ack != nil && sameJSON(normalizeJSONValue(*pending.Ack), normalizeJSONValue(ack)) {
			c.JSON(http.StatusOK, pending.Ack)
			return
		}
		c.JSON(409, gin.H{"error": "action already acknowledged"})
		return
	}
	if ack.Status == "" {
		c.JSON(400, gin.H{"error": "status is required"})
		return
	}
	if ack.Status != "applied" && ack.Status != "rejected" && ack.Status != "failed" {
		c.JSON(400, gin.H{"error": "status must be applied, rejected, or failed"})
		return
	}
	if pending.Command.Action == "submit_detection" && ack.Status == "applied" && (ack.Result == nil || ack.Result.TaskID == "" || ack.Result.PairCount < 1 || stringValue(ack.Workspace["task_id"]) != ack.Result.TaskID) {
		c.JSON(400, gin.H{"error": "detection acknowledgement requires the created workspace task"})
		return
	}
	pending.Status = ack.Status
	pending.Ack = &ack
	now := time.Now().UTC()
	pending.AcknowledgedAt = &now
	session.Actions[id] = pending
	if ack.Workspace != nil {
		session.Workspace = ack.Workspace
	}
	session.UpdatedAt = now
	if err := g.repo.upsertSession(session); err != nil {
		c.JSON(500, gin.H{"error": "session persistence failed"})
		return
	}
	g.ackMu.Lock()
	ch := g.acks[id]
	g.ackMu.Unlock()
	if ch != nil {
		select {
		case ch <- ack:
		default:
		}
	}
	c.JSON(200, ack)
}
