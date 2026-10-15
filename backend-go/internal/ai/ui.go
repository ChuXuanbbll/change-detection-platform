package ai

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

var uiActions = []string{"select_region", "load_phase", "load_phases", "set_existing_aoi", "fly_to_bounds", "show_task", "show_result", "open_report"}

func uiToolSpecs() []ToolSpec {
	result := make([]ToolSpec, 0, len(uiActions))
	for _, action := range uiActions {
		result = append(result, ToolSpec{Name: "ui." + action, Version: "1.0.0", Description: "Request a typed workspace UI action and wait for its acknowledgement.", Effect: "workspace", MaxOutputSize: 16384, InputSchema: uiInputSchemaForAction(action), OutputSchema: uiOutputSchema()})
	}
	return result
}

func uiInputSchema() map[string]any {
	return map[string]any{"$schema": "https://json-schema.org/draft/2020-12/schema", "type": "object", "additionalProperties": false, "properties": map[string]any{"region_id": map[string]any{"type": "string", "minLength": 1}, "phase_id": map[string]any{"type": "string", "minLength": 1}, "phase_ids": map[string]any{"type": "array", "items": map[string]any{"type": "string", "minLength": 1}, "minItems": 1, "maxItems": 128}, "scene_ids": map[string]any{"type": "array", "items": map[string]any{"type": "string", "minLength": 1}, "minItems": 1, "maxItems": 128}, "source": map[string]any{"type": "string", "enum": []any{"admin_boundary", "user_drawn", "workspace"}}, "bounds": map[string]any{"type": "array", "minItems": 4, "maxItems": 4, "items": map[string]any{"type": "number"}}, "aoi": uiAOISchema(), "task_id": map[string]any{"type": "string", "minLength": 1}, "result_id": map[string]any{"type": "string", "minLength": 1}, "expected_revision": map[string]any{"type": "string", "minLength": 1}}}
}

func uiInputSchemaForAction(action string) map[string]any {
	base := uiInputSchema()
	properties := base["properties"].(map[string]any)
	allowed := map[string][]string{
		"select_region":    {"region_id", "expected_revision"},
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
	return map[string]any{"$schema": "https://json-schema.org/draft/2020-12/schema", "type": "object", "additionalProperties": false, "properties": map[string]any{"status": map[string]any{"type": "string", "enum": []any{"applied", "rejected", "failed", "timeout", "cancelled"}}, "action_id": map[string]any{"type": "string", "minLength": 1}, "context_revision": map[string]any{"type": "string"}, "error_code": map[string]any{"type": "string"}}, "required": []any{"status", "action_id"}}
}

func uiAOISchema() map[string]any {
	return map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{
		"type":        map[string]any{"type": "string", "enum": []any{"Polygon"}},
		"coordinates": map[string]any{"type": "array", "minItems": 1, "maxItems": 1, "items": map[string]any{"type": "array", "minItems": 4, "items": map[string]any{"type": "array", "minItems": 2, "maxItems": 2, "items": map[string]any{"type": "number"}}}},
	}, "required": []any{"type", "coordinates"}}
}

func (g *Gateway) handleUICommand(ctx context.Context, session *Session, workflow *Workflow, w io.Writer, call ProviderToolCall, args map[string]any) error {
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
	actionID := uuid.NewString()
	expected := workspaceRevision(session.Workspace)
	if value, ok := args["expected_revision"].(string); ok {
		expected = value
	}
	command := UICommand{Action: action, ActionID: actionID, Args: args, ExpectedRevision: expected}
	encodedArgs, _ := json.Marshal(map[string]any{"action": action, "args": args, "expected_revision": expected})
	fingerprint := fmt.Sprintf("%x", sha256.Sum256(encodedArgs))
	for _, existing := range session.Actions {
		if existing.Fingerprint == fingerprint {
			if existing.Status == "applied" {
				return nil
			}
			if existing.Status == "pending" {
				return fmt.Errorf("UI action is already pending")
			}
		}
	}
	pending := PendingAction{ID: actionID, Command: command, Status: "pending", Fingerprint: fingerprint, CreatedAt: time.Now().UTC()}
	if session.Actions == nil {
		session.Actions = make(map[string]PendingAction)
	}
	session.Actions[actionID] = pending
	session.UpdatedAt = time.Now().UTC()
	if err := g.repo.upsertSession(*session); err != nil {
		return err
	}
	ackCh := make(chan ActionAck, 1)
	g.ackMu.Lock()
	g.acks[actionID] = ackCh
	g.ackMu.Unlock()
	defer func() { g.ackMu.Lock(); delete(g.acks, actionID); g.ackMu.Unlock() }()
	g.emitEvent(workflow, w, "ui_command", command)
	timer := time.NewTimer(30 * time.Second)
	defer timer.Stop()
	select {
	case ack := <-ackCh:
		pending.Status = ack.Status
		pending.Ack = &ack
		now := time.Now().UTC()
		pending.AcknowledgedAt = &now
		session.Actions[actionID] = pending
		session.Workspace = ack.Workspace
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

func latestAppliedActionID(session *Session) string {
	latest := ""
	var latestAt time.Time
	for id, action := range session.Actions {
		if action.Status == "applied" && (latest == "" || action.CreatedAt.After(latestAt)) {
			latest, latestAt = id, action.CreatedAt
		}
	}
	return latest
}

func validateUIArgs(action string, args map[string]any) error {
	allowed := map[string]map[string]bool{
		"select_region":    {"region_id": true, "expected_revision": true},
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
	pending, ok := session.Actions[id]
	if !ok {
		c.JSON(404, gin.H{"error": "action not found"})
		return
	}
	if pending.Status != "pending" {
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
