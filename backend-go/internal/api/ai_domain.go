package api

// The AI domain is deliberately a narrow, typed boundary around existing
// image/task services. It exposes metadata and opaque references only. Paths,
// model files, SQL and upstream traceback text never leave this package.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"

	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"cdp/backend-go/internal/ai"
	"cdp/backend-go/internal/model"
	"cdp/backend-go/internal/store"
	"github.com/google/uuid"
)

const aiSchemaVersion = "tool-result.v1"

type AIDomain struct {
	d           *Deps
	mu          sync.RWMutex
	closed      bool
	idemMu      sync.Mutex
	idempotency map[string]*idempotencyLock
}

type idempotencyLock struct {
	mu   sync.Mutex
	refs int
}

func NewAIDomain(d *Deps) (*AIDomain, error) {
	if d == nil || d.DB == nil {
		return nil, errors.New("ai domain requires database")
	}
	if err := d.DB.MigrateSettings(); err != nil {
		return nil, err
	}
	if err := d.DB.MigrateAIDomain(); err != nil {
		return nil, err
	}
	return &AIDomain{d: d, idempotency: map[string]*idempotencyLock{}}, nil
}
func (d *AIDomain) Close() { d.mu.Lock(); d.closed = true; d.mu.Unlock() }

func (d *AIDomain) Specs() []ai.ToolSpec { return aiSpecs() }

func (d *AIDomain) Execute(ctx context.Context, call ai.Call) ai.Result {
	r := ai.Result{SchemaVersion: aiSchemaVersion, RequestID: call.RequestID, Tool: call.Tool, ToolVersion: call.ToolVersion, Status: "ok"}
	if call.SchemaVersion != "" && call.SchemaVersion != "tool-call.v1" {
		return d.errResult(r, "INVALID_SCHEMA", "unsupported call schema", false, nil)
	}
	if call.RequestID == "" {
		return d.errResult(r, "INVALID_INPUT", "request_id is required", false, nil)
	}
	if call.ToolVersion == "" {
		call.ToolVersion = "1.0.0"
		r.ToolVersion = call.ToolVersion
	}
	d.mu.RLock()
	closed := d.closed
	d.mu.RUnlock()
	if closed {
		return d.errResult(r, "DOMAIN_CLOSED", "AI domain is closed", false, nil)
	}
	if !knownTool(call.Tool) {
		return d.errResult(r, "TOOL_NOT_FOUND", "unknown tool", false, nil)
	}
	// Validate before looking up an idempotent request. Otherwise a malformed
	// retry could bypass the current input and opaque-ID boundary by replaying
	// an older cached result.
	if err := validateInput(call.Tool, call.Input); err != nil {
		return d.errResult(r, errorCode(err), safeError(err), retryableError(err), nil)
	}
	if kind := idempotencyKind(call.Tool); kind != "" {
		unlock := d.lockRequest(kind + ":" + call.RequestID)
		defer unlock()
	}
	callFingerprint := d.callFingerprint(call)
	if kind := idempotencyKind(call.Tool); kind != "" {
		if prior, err := d.d.DB.FindAIRequest(kind, call.RequestID); err == nil {
			if prior.Fingerprint != "" && prior.Fingerprint != callFingerprint {
				return d.errResult(r, "IDEMPOTENCY_CONFLICT", "request_id was already used with different input", false, nil)
			}
			// Replay the complete public result, including an original error. A
			// resource document alone only describes the durable object and must
			// never turn a failed first request into a later success.
			if len(prior.Result) > 0 {
				var replay ai.Result
				if json.Unmarshal(prior.Result, &replay) == nil && replay.Status != "" {
					return replay
				}
			}
			var value any
			if json.Unmarshal(prior.Document, &value) == nil {
				// Documents written before result_json was introduced can still
				// represent a terminal failed request. Preserve that failure when
				// replaying them instead of falling through to status=ok.
				if prior.Status == "partial_failed" || prior.Status == "failed" {
					code, message := "OPERATION_FAILED", "the original request failed"
					if record, ok := value.(map[string]any); ok {
						if candidate := str(record["error_code"]); candidate != "" {
							code = candidate
						}
						if candidate := str(record["error"]); candidate != "" {
							message = candidate
						}
					}
					return d.errResult(r, code, message, true, nil)
				}
				if kind == "plan" {
					if plan, ok := value.(map[string]any); ok {
						value = publicPlan(plan)
					}
				}
				r.Data = normalizePublic(value)
				return r
			}
		}
	}
	data, warns, err := d.execute(ctx, call)
	if err != nil {
		r = d.errResult(r, errorCode(err), safeError(err), retryableError(err), nil)
		d.persistIdempotentResult(call, callFingerprint, r)
		return r
	}
	r.Data = normalizePublic(data)
	r.Warnings = warns
	d.persistIdempotentResult(call, callFingerprint, r)
	return r
}

// callFingerprint applies only deterministic defaults that are local to the
// tool contract. Dynamic defaults (such as the active model or latest
// operation) remain part of the caller input and are resolved by the domain.
func (d *AIDomain) callFingerprint(call ai.Call) string {
	input := cloneMap(call.Input)
	if input == nil {
		input = map[string]any{}
	}
	switch call.Tool {
	case "detection.plan.create":
		if _, ok := input["pair_policy"]; !ok {
			input["pair_policy"] = "first_last"
		}
		if _, ok := input["aoi_source"]; !ok {
			input["aoi_source"] = "user_drawn"
		}
		// createPlan resolves omitted model/config values from the active
		// revision. Include those effective values so an explicit equivalent
		// request reuses the same idempotent plan.
		if revision, err := d.d.DB.ActiveRevision(); err == nil {
			if str(input["model_id"]) == "" {
				input["model_id"] = revision.Settings.DefaultModel
			}
			if int64Val(input["config_revision"]) == 0 {
				input["config_revision"] = revision.ID
			}
		}
	}
	return fingerprint(input)
}

func (d *AIDomain) persistIdempotentResult(call ai.Call, fp string, result ai.Result) {
	kind := idempotencyKind(call.Tool)
	if kind == "" {
		return
	}
	b, err := json.Marshal(result)
	if err != nil {
		return
	}
	doc, err := d.d.DB.FindAIRequest(kind, call.RequestID)
	if err != nil {
		// No resource was created (normally a validation or queue failure), so
		// create a durable request record solely for replaying the result.
		doc = store.AIDocument{ID: "ai-request-" + uuid.NewString(), Kind: kind, Status: result.Status, RequestID: call.RequestID, Fingerprint: fp, Document: json.RawMessage("null")}
	} else {
		// Store the normalized fingerprint used for this request. Resource
		// creators historically persisted the raw input before Execute could
		// apply deterministic defaults.
		doc.Fingerprint = fp
	}
	doc.Result = b
	if doc.Status == "" || doc.Status == "frozen" || doc.Status == "queued" || doc.Status == "submitted" {
		// Keep the resource lifecycle status for successful plans/operations;
		// request replay reads Result.Status independently.
		if result.Status == "error" {
			doc.Status = result.Status
		}
	}
	_ = d.d.DB.SaveAIDocument(doc)
}

func (d *AIDomain) lockRequest(key string) func() {
	d.idemMu.Lock()
	lock := d.idempotency[key]
	if lock == nil {
		lock = &idempotencyLock{}
		d.idempotency[key] = lock
	}
	lock.refs++
	d.idemMu.Unlock()
	lock.mu.Lock()
	return func() {
		lock.mu.Unlock()
		d.idemMu.Lock()
		lock.refs--
		if lock.refs == 0 {
			delete(d.idempotency, key)
		}
		d.idemMu.Unlock()
	}
}

func idempotencyKind(tool string) string {
	switch tool {
	case "detection.plan.create":
		return "plan"
	case "imagery.prepare":
		return "operation"
	case "detection.workflow.submit":
		return "workflow"
	default:
		return ""
	}
}

type aiCodedError struct {
	code, msg string
	retry     bool
}

func (e *aiCodedError) Error() string { return e.msg }
func (e *aiCodedError) Code() string  { return e.code }
func coded(code, msg string, retry bool) error {
	return &aiCodedError{code: code, msg: msg, retry: retry}
}
func errorCode(err error) string {
	var e *aiCodedError
	if errors.As(err, &e) {
		return e.code
	}
	var se *ServiceError
	if errors.As(err, &se) {
		if code, ok := se.Body["code"].(string); ok && code != "" {
			return code
		}
	}
	return "OPERATION_FAILED"
}
func retryableError(err error) bool {
	var e *aiCodedError
	if errors.As(err, &e) {
		return e.retry
	}
	var se *ServiceError
	return errors.As(err, &se) && (se.Status >= 500 || se.Status == 409)
}
func safeError(err error) string {
	if err == nil {
		return ""
	}
	var e *aiCodedError
	if errors.As(err, &e) {
		return e.msg
	}
	var se *ServiceError
	if errors.As(err, &se) {
		// REST preserves upstream diagnostics for operators; AI receives only
		// a stable code. Upstream errors can contain source paths or tracebacks.
		return "domain operation failed (" + errorCode(err) + ")"
	}
	return "operation failed"
}
func (d *AIDomain) errResult(r ai.Result, code, msg string, retry bool, field *string) ai.Result {
	r.Status = "error"
	r.Error = &ai.ToolError{Code: code, Message: msg, Retryable: retry, Field: field}
	return r
}

func (d *AIDomain) execute(ctx context.Context, c ai.Call) (any, []string, error) {
	if err := validateInput(c.Tool, c.Input); err != nil {
		return nil, nil, err
	}
	switch c.Tool {
	case "workspace.context":
		workspace := c.Context.Workspace
		if workspace == nil {
			workspace = map[string]any{}
		}
		return workspaceDTO(workspace), nil, nil
	case "region.search":
		return d.regionSearch(ctx, c.Input)
	case "imagery.catalog":
		value, warnings, err := d.pyJSON(ctx, "GET", "/py/local/catalog?refresh=false", nil)
		return catalogDTO(value), warnings, err
	case "imagery.scenes":
		regionID := urlPart(str(c.Input["region_id"]))
		if regionID == "" {
			return nil, nil, coded("INVALID_INPUT", "region_id must be an opaque resource identifier", false)
		}
		data, warnings, err := d.pyJSON(ctx, "GET", "/py/local/regions/"+regionID+"/scenes?refresh=false", nil)
		if err != nil {
			return nil, warnings, err
		}
		return scenesDTO(filterScenes(data, c.Input)), warnings, nil
	case "imagery.phase":
		return d.phase(ctx, c.Input)
	case "model.list":
		return d.models()
	case "image.list":
		return d.images(c.Input)
	case "task.search":
		return d.tasks(c.Input)
	case "task.status":
		return d.task(c.Input)
	case "result.summary":
		return d.resultSummary(c.Input)
	case "result.patches.query":
		return d.resultPatches(c.Input)
	case "selection.validate":
		return d.selectionValidate(ctx, c.Input)
	case "detection.plan.create":
		return d.createPlan(ctx, c)
	case "imagery.prepare":
		return d.prepare(ctx, c)
	case "detection.workflow.submit":
		return d.submitWorkflow(ctx, c)
	case "operation.status":
		return d.operationStatus(c.Input)
	case "result.export.prepare":
		return d.exportPrepare(c.Input)
	}
	return nil, nil, coded("TOOL_NOT_FOUND", "unknown tool", false)
}

func knownTool(n string) bool {
	for _, s := range aiSpecs() {
		if s.Name == n {
			return true
		}
	}
	return false
}
func urlPart(s string) string {
	s = strings.TrimSpace(s)
	if !validOpaqueID(s) {
		return ""
	}
	return s
}

func validOpaqueID(value string) bool {
	if value == "" || len(value) > 256 {
		return false
	}
	for _, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' || r == '-' || r == ':' {
			continue
		}
		return false
	}
	return true
}

func validateOpaqueInputs(in map[string]any) error {
	for _, field := range []string{"region_id", "phase_id", "task_id", "plan_id", "operation_id", "model_id", "before_id", "after_id", "config_revision"} {
		if raw, exists := in[field]; exists && str(raw) != "" && !validOpaqueID(str(raw)) {
			return coded("INVALID_INPUT", field+" must be an opaque resource identifier", false)
		}
	}
	if raw, exists := in["scene_ids"]; exists {
		for _, id := range stringSlice(raw) {
			if !validOpaqueID(id) {
				return coded("INVALID_INPUT", "scene_ids must contain opaque resource identifiers", false)
			}
		}
	}
	return nil
}

func validateInput(tool string, in map[string]any) error {
	allowed := map[string][]string{
		"workspace.context": nil, "region.search": {"query", "admin_path", "region_id"}, "imagery.catalog": {"refresh"}, "imagery.scenes": {"region_id", "start_date", "end_date"}, "imagery.phase": {"region_id", "phase_id"}, "model.list": nil, "image.list": {"source", "phase", "limit"}, "task.search": {"status", "algorithm", "region_id", "created_after", "created_before", "offset", "limit"}, "task.status": {"task_id"}, "result.summary": {"task_id"}, "result.patches.query": {"task_id", "area_min", "area_max", "confidence_min", "limit", "sort"}, "selection.validate": {"region_id", "scene_ids", "bounds", "aoi"}, "detection.plan.create": {"region_id", "aoi", "aoi_source", "bounds", "scene_ids", "before_id", "after_id", "pair_policy", "model_id", "config_revision", "start_date", "end_date"}, "imagery.prepare": {"plan_id"}, "detection.workflow.submit": {"plan_id", "operation_id"}, "operation.status": {"operation_id"}, "result.export.prepare": {"task_id", "format"},
	}
	known := map[string]bool{}
	for _, name := range allowed[tool] {
		known[name] = true
	}
	for k := range in {
		if !known[k] {
			return coded("INVALID_INPUT", "unknown input field: "+k, false)
		}
	}
	if err := validateOpaqueInputs(in); err != nil {
		return err
	}
	return nil
}

func (d *AIDomain) pyJSON(ctx context.Context, method, path string, payload any) (any, []string, error) {
	if d.d.Py == nil {
		return nil, nil, coded("UPSTREAM_UNAVAILABLE", "imagery service unavailable", true)
	}
	var out any
	if err := d.d.Py.JSON(ctx, method, path, payload, &out); err != nil {
		return nil, nil, coded("UPSTREAM_UNAVAILABLE", "imagery service unavailable", true)
	}
	return sanitize(out), nil, nil
}

func filterScenes(raw any, input map[string]any) any {
	m, ok := raw.(map[string]any)
	if !ok {
		return raw
	}
	start := dateBoundary(input["start_date"], false)
	end := dateBoundary(input["end_date"], true)
	if start.IsZero() && end.IsZero() {
		return raw
	}
	filtered := map[string]any{}
	for key, value := range m {
		filtered[key] = value
	}
	if scenes, ok := m["scenes"].([]any); ok {
		out := make([]any, 0, len(scenes))
		for _, value := range scenes {
			row, ok := value.(map[string]any)
			if !ok {
				continue
			}
			sceneStart, sceneEnd := sceneDateBounds(row)
			if sceneStart.IsZero() || (!start.IsZero() && sceneEnd.Before(start)) || (!end.IsZero() && sceneStart.After(end)) {
				continue
			}
			out = append(out, row)
		}
		filtered["scenes"] = out
	}
	if phases, ok := m["phases"].([]any); ok {
		out := make([]any, 0, len(phases))
		for _, value := range phases {
			phase, ok := value.(map[string]any)
			if !ok {
				continue
			}
			month := str(phase["month"])
			when, err := time.Parse("2006-01", month)
			if err != nil || (!start.IsZero() && when.AddDate(0, 1, 0).Add(-time.Nanosecond).Before(start)) || (!end.IsZero() && when.After(end)) {
				continue
			}
			out = append(out, phase)
		}
		filtered["phases"] = out
	}
	return filtered
}

func sceneDateSlot(row map[string]any) map[string]any {
	value := str(row["date"])
	precision := strDefault(str(row["date_precision"]), "day")
	switch precision {
	case "year":
		if len(value) >= 4 {
			value = value[:4]
		}
	case "month":
		if len(value) >= 7 {
			value = value[:7]
		}
	}
	return normalizeDateSlot(map[string]any{"value": value, "precision": precision})
}

func sceneDateBounds(row map[string]any) (time.Time, time.Time) {
	slot := sceneDateSlot(row)
	if slot == nil {
		return time.Time{}, time.Time{}
	}
	return dateBoundary(slot, false), dateBoundary(slot, true)
}

func catalogDTO(raw any) any {
	m, ok := raw.(map[string]any)
	if !ok {
		return map[string]any{"images": []any{}, "bounds": []any{}, "boundary_mode": "unknown", "warning": "catalog response invalid"}
	}
	out := map[string]any{"images": m["images"], "bounds": m["bounds"], "boundary": m["boundary"], "boundary_mode": m["boundary_mode"], "warning": m["warning"]}
	if out["images"] == nil {
		out["images"] = []any{}
	}
	if out["bounds"] == nil {
		out["bounds"] = []any{}
	}
	return out
}

func scenesDTO(raw any) any {
	m, ok := raw.(map[string]any)
	if !ok {
		return map[string]any{"region_id": "", "scenes": []any{}, "phases": []any{}}
	}
	out := map[string]any{"region_id": m["region_id"], "name": m["name"], "scenes": m["scenes"], "phases": m["phases"]}
	if out["scenes"] == nil {
		out["scenes"] = []any{}
	}
	if out["phases"] == nil {
		out["phases"] = []any{}
	}
	return out
}

func phaseDTO(raw map[string]any) any {
	// Phase records are catalog-owned metadata. Keep opaque identifiers and
	// coverage fields while dropping any accidental upstream file references.
	out := map[string]any{}
	for _, key := range []string{"id", "region_id", "month", "version", "available", "bounds", "scenes"} {
		if value, ok := raw[key]; ok {
			out[key] = value
		}
	}
	return out
}

func dateBoundary(value any, end bool) time.Time {
	if value == nil {
		return time.Time{}
	}
	slot := normalizeDateSlot(value)
	if slot == nil {
		return time.Time{}
	}
	dateValue, precision := str(slot["value"]), str(slot["precision"])
	if precision == "year" {
		if end {
			dateValue += "-12-31"
		} else {
			dateValue += "-01-01"
		}
	} else if precision == "month" {
		if end {
			parsed, err := time.Parse("2006-01", dateValue)
			if err != nil {
				return time.Time{}
			}
			return parsed.AddDate(0, 1, 0).Add(-time.Nanosecond)
		}
		dateValue += "-01"
	}
	parsed, err := time.Parse("2006-01-02", dateValue)
	if err != nil {
		return time.Time{}
	}
	return parsed
}

func dateSlotBoundary(slot map[string]any, end bool, fallback string) string {
	value, precision := str(slot["value"]), str(slot["precision"])
	if value == "" {
		return fallback
	}
	switch precision {
	case "year":
		if end {
			return value + "-12-31"
		}
		return value + "-01-01"
	case "month":
		parsed, err := time.Parse("2006-01", value)
		if err != nil {
			return fallback
		}
		if end {
			return parsed.AddDate(0, 1, 0).Add(-24 * time.Hour).Format("2006-01-02")
		}
		return value + "-01"
	case "day":
		return value
	default:
		return fallback
	}
}
func sanitize(v any) any {
	switch x := v.(type) {
	case map[string]any:
		if x == nil {
			return nil
		}
		o := map[string]any{}
		for k, v := range x {
			lk := strings.ToLower(k)
			if forbiddenKey(lk) {
				continue
			}
			o[k] = sanitize(v)
		}
		return o
	case []any:
		if x == nil {
			return nil
		}
		o := make([]any, len(x))
		for i, v := range x {
			o[i] = sanitize(v)
		}
		return o
	default:
		return v
	}
}

func workspaceDTO(raw map[string]any) map[string]any {
	out := map[string]any{}
	for _, key := range []string{"revision", "page", "region_id", "region_name", "phase_ids", "phases", "available_phases", "catalog_status", "selection_error", "active_phase", "bounds", "aoi", "task_id", "result_id", "task", "detection"} {
		if value, ok := raw[key]; ok {
			out[key] = value
		}
	}
	return out
}

func normalizePublic(v any) any {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	var decoded any
	if json.Unmarshal(raw, &decoded) != nil {
		return nil
	}
	return sanitize(decoded)
}
func forbiddenKey(k string) bool {
	// Administrative paths contain display names, never filesystem paths.
	if k == "admin_path" {
		return false
	}
	for _, x := range []string{"path", "file", "filename", "source_file", "source_files", "output_dir", "source_root", "token", "control.token", "password", "secret", "sql", "shell", "traceback", "model_path", "params", "parameters", "provider"} {
		if k == x || strings.HasSuffix(k, "_path") || strings.HasSuffix(k, "_file") {
			return true
		}
	}
	return false
}

func (d *AIDomain) regionSearch(ctx context.Context, in map[string]any) (any, []string, error) {
	data, _, err := d.pyJSON(ctx, "GET", "/py/local/regions", nil)
	if err != nil {
		return nil, nil, err
	}
	query := strings.ToLower(strings.TrimSpace(str(in["query"])))
	rid := strings.TrimSpace(str(in["region_id"]))
	requestedPath := stringSlice(in["admin_path"])
	matches := []any{}
	if m, ok := data.(map[string]any); ok {
		if fs, ok := m["features"].([]any); ok {
			for _, f := range fs {
				if fm, ok := f.(map[string]any); ok {
					p, _ := fm["properties"].(map[string]any)
					// GeoJSON regions currently carry their stable opaque identifier in
					// properties.  Never manufacture an identifier from the display name.
					id := str(fm["id"])
					if id == "" {
						id = str(p["region_id"])
					}
					if id == "" {
						continue
					}
					name := str(p["name"])
					if rid != "" && id != rid {
						continue
					}
					adminPath := p["admin_path"]
					// Boundary manifests predating hierarchy metadata are still
					// queryable, but we must not invent a parent or widen a county
					// request to its containing city.
					if adminPath == nil {
						// Older boundary manifests do not carry hierarchy metadata.
						// Keep the display name as the only known path component rather
						// than inventing a parent province or city.
						adminPath = []any{name}
					}
					if len(requestedPath) > 0 && !sameAdminPath(requestedPath, adminPath) {
						continue
					}
					pathText := strings.ToLower(strings.Join(adminPathParts(adminPath), ""))
					if query != "" && !strings.Contains(pathText, query) && !strings.Contains(strings.ToLower(id), query) {
						continue
					}
					geometry := sanitize(fm["geometry"])
					matches = append(matches, map[string]any{"region_id": id, "name": name, "level": strDefault(str(p["level"]), str(p["kind"])), "admin_path": adminPath, "aoi_supported": validateAIPolygon(geometry) == nil, "bounds": geometryBounds(geometry), "geometry": geometry})
				}
			}
		}
	}
	return map[string]any{"matches": matches, "count": len(matches)}, nil, nil
}

func adminPathParts(value any) []string {
	switch path := value.(type) {
	case []any:
		parts := make([]string, 0, len(path))
		for _, item := range path {
			parts = append(parts, str(item))
		}
		return parts
	case []string:
		return path
	case string:
		return []string{path}
	default:
		return nil
	}
}

func sameAdminPath(requested []string, actual any) bool {
	parts := adminPathParts(actual)
	if len(requested) != len(parts) {
		return false
	}
	for i := range requested {
		if strings.TrimSpace(requested[i]) != parts[i] {
			return false
		}
	}
	return true
}

func (d *AIDomain) phase(ctx context.Context, in map[string]any) (any, []string, error) {
	rid := urlPart(str(in["region_id"]))
	pid := str(in["phase_id"])
	if rid == "" || pid == "" {
		return nil, nil, coded("INVALID_INPUT", "region_id and phase_id are required", false)
	}
	if !validOpaqueID(pid) {
		return nil, nil, coded("INVALID_INPUT", "phase_id must be an opaque resource identifier", false)
	}
	data, warnings, err := d.pyJSON(ctx, "GET", "/py/local/regions/"+rid+"/scenes?refresh=false", nil)
	if err != nil {
		return nil, warnings, err
	}
	if m, ok := data.(map[string]any); ok {
		if phases, ok := m["phases"].([]any); ok {
			for _, raw := range phases {
				if p, ok := raw.(map[string]any); ok && str(p["id"]) == pid {
					return phaseDTO(p), warnings, nil
				}
			}
		}
	}
	return nil, nil, coded("RESOURCE_NOT_FOUND", "imagery phase not found", false)
}
func (d *AIDomain) models() (any, []string, error) {
	ms, err := d.d.DB.Models()
	if err != nil {
		return nil, nil, coded("MODEL_LIST_FAILED", "model catalog unavailable", true)
	}
	out := make([]any, 0, len(ms))
	for _, m := range ms {
		if !m.Enabled || m.Status != "ready" {
			continue
		}
		hash := m.Hash
		if len(hash) > 12 {
			hash = hash[:12]
		}
		out = append(out, map[string]any{"model_id": m.ID, "name": m.Name, "description": m.Description, "status": m.Status, "enabled": m.Enabled, "sha256_prefix": hash, "mapping": m.Mapping})
	}
	return map[string]any{"items": out, "count": len(out)}, nil, nil
}

func (d *AIDomain) images(in map[string]any) (any, []string, error) {
	limit := intVal(in["limit"], 100)
	if limit < 1 || limit > 500 {
		return nil, nil, coded("INVALID_INPUT", "limit must be 1..500", false)
	}
	imgs, err := d.d.DB.ListImages(limit, str(in["source"]), str(in["phase"]))
	if err != nil {
		return nil, nil, coded("IMAGE_LIST_FAILED", "image catalog unavailable", true)
	}
	out := make([]any, 0, len(imgs))
	for _, im := range imgs {
		out = append(out, imageDTO(im))
	}
	return map[string]any{"items": out, "count": len(out)}, nil, nil
}
func imageDTO(im model.Image) map[string]any {
	return map[string]any{"image_id": im.ID, "source": im.Source, "phase": im.Phase, "capture_date": im.CaptureDate.Format(time.RFC3339), "date_precision": datePrecision(im.MetaJSON), "bounds": []float64{im.BoundsWest, im.BoundsSouth, im.BoundsEast, im.BoundsNorth}, "width": im.Width, "height": im.Height, "crs": im.CRS, "metadata": sanitizeMeta(im.MetaJSON)}
}
func sanitizeMeta(raw string) any {
	var x any
	if json.Unmarshal([]byte(raw), &x) != nil {
		return nil
	}
	return sanitize(x)
}
func datePrecision(raw string) string {
	var m map[string]any
	if json.Unmarshal([]byte(raw), &m) == nil {
		if p := str(m["date_precision"]); p != "" {
			return p
		}
	}
	return "day"
}

func (d *AIDomain) tasks(in map[string]any) (any, []string, error) {
	ts, err := d.d.DB.ListTasksForAIFilter(store.AITaskFilter{Status: str(in["status"]), Algorithm: str(in["algorithm"]), RegionID: str(in["region_id"]), CreatedAfter: str(in["created_after"]), CreatedBefore: str(in["created_before"]), Offset: intVal(in["offset"], 0), Limit: intVal(in["limit"], 100)})
	if err != nil {
		return nil, nil, coded("TASK_LIST_FAILED", "task catalog unavailable", true)
	}
	out := make([]any, 0, len(ts))
	for _, t := range ts {
		out = append(out, taskDTO(t))
	}
	return map[string]any{"items": out, "count": len(out)}, nil, nil
}
func taskDTO(t store.AITask) map[string]any {
	errText := ""
	if t.ErrorMsg != "" {
		errText = "task failed"
	}
	return map[string]any{"task_id": t.ID, "before_image_id": t.BeforeID, "after_image_id": t.AfterID, "algorithm": t.Algorithm, "status": t.Status, "stage": t.Stage, "progress": t.Progress, "error": errText, "created_at": t.CreatedAt.Format(time.RFC3339)}
}

func (d *AIDomain) task(in map[string]any) (any, []string, error) {
	id := str(in["task_id"])
	if id == "" {
		return nil, nil, coded("INVALID_INPUT", "task_id is required", false)
	}
	t, err := d.d.DB.GetTask(id)
	if err != nil {
		return nil, nil, coded("TASK_NOT_FOUND", "task not found", false)
	}
	return taskDTO(store.AITask{ID: t.ID, BeforeID: t.BeforeID, AfterID: t.AfterID, Algorithm: t.Algorithm, Status: t.Status, Stage: t.Stage, Progress: t.Progress, ErrorMsg: t.ErrorMsg, CreatedAt: t.CreatedAt}), nil, nil
}
func (d *AIDomain) resultSummary(in map[string]any) (any, []string, error) {
	id := str(in["task_id"])
	if id == "" {
		return nil, nil, coded("INVALID_INPUT", "task_id is required", false)
	}
	r, err := d.d.DB.GetResult(id)
	if err != nil {
		return nil, nil, coded("RESULT_NOT_READY", "result is not ready", false)
	}
	var stats any
	if json.Unmarshal([]byte(r.StatsJSON), &stats) != nil {
		stats = map[string]any{}
	}
	return map[string]any{"result_id": id, "task_id": id, "stats": sanitize(stats), "created_at": r.CreatedAt.Format(time.RFC3339), "available_formats": []string{"geojson", "csv", "shp", "tiff"}}, nil, nil
}

func (d *AIDomain) resultPatches(in map[string]any) (any, []string, error) {
	id := str(in["task_id"])
	if id == "" {
		return nil, nil, coded("INVALID_INPUT", "task_id is required", false)
	}
	r, err := d.d.DB.GetResult(id)
	if err != nil {
		return nil, nil, coded("RESULT_NOT_READY", "result is not ready", false)
	}
	raw, err := os.ReadFile(r.GeoJSONPath)
	if err != nil {
		return nil, nil, coded("RESULT_NOT_READY", "result geometry is not ready", false)
	}
	var fc struct {
		Type     string           `json:"type"`
		Features []map[string]any `json:"features"`
	}
	if json.Unmarshal(raw, &fc) != nil || fc.Type != "FeatureCollection" {
		return nil, nil, coded("RESULT_INVALID", "result geometry is invalid", false)
	}
	min, max := floatVal(in["area_min"], -1), floatVal(in["area_max"], -1)
	conf := floatVal(in["confidence_min"], -1)
	filtered := []map[string]any{}
	for _, f := range fc.Features {
		p, _ := f["properties"].(map[string]any)
		area := floatVal(p["area_m2"], -1)
		c := floatVal(p["confidence_mean"], -1)
		if min >= 0 && area < min || max >= 0 && area > max || conf >= 0 && c < conf {
			continue
		}
		filtered = append(filtered, map[string]any{"id": f["id"], "geometry": f["geometry"], "properties": sanitize(p)})
	}
	sortMode := strDefault(str(in["sort"]), "area_desc")
	if sortMode != "area_desc" && sortMode != "area_asc" && sortMode != "confidence_desc" && sortMode != "confidence_asc" {
		return nil, nil, coded("INVALID_INPUT", "sort must be area_desc, area_asc, confidence_desc or confidence_asc", false)
	}
	sort.SliceStable(filtered, func(i, j int) bool {
		left := filtered[i]["properties"].(map[string]any)
		right := filtered[j]["properties"].(map[string]any)
		leftValue, rightValue := floatVal(left["area_m2"], -1), floatVal(right["area_m2"], -1)
		if strings.HasPrefix(sortMode, "confidence") {
			leftValue, rightValue = floatVal(left["confidence_mean"], -1), floatVal(right["confidence_mean"], -1)
		}
		if strings.HasSuffix(sortMode, "_asc") {
			return leftValue < rightValue
		}
		return leftValue > rightValue
	})
	limit := intVal(in["limit"], 100)
	if limit > len(filtered) {
		limit = len(filtered)
	}
	return map[string]any{"task_id": id, "items": filtered[:limit], "count": len(filtered), "limit": limit}, nil, nil
}

func (d *AIDomain) selectionValidate(ctx context.Context, in map[string]any) (any, []string, error) {
	rid := str(in["region_id"])
	ids := stringSlice(in["scene_ids"])
	if rid == "" || len(ids) == 0 {
		return nil, nil, coded("INVALID_INPUT", "region_id and scene_ids are required", false)
	}
	regionID := urlPart(rid)
	if regionID == "" {
		return nil, nil, coded("INVALID_INPUT", "region_id must be an opaque resource identifier", false)
	}
	catalog, warnings, err := d.pyJSON(ctx, "GET", "/py/local/regions/"+regionID+"/scenes?refresh=false", nil)
	if err != nil {
		return nil, warnings, err
	}
	groups, boundsByMonth, err := groupSceneIDsByMonth(catalog, ids)
	if err != nil {
		return nil, warnings, err
	}
	selectionBounds := in["bounds"]
	if selectionBounds == nil {
		selectionBounds = geometryBounds(in["aoi"])
	}
	if selectionBounds == nil {
		return nil, warnings, coded("BOUNDS_INVALID", "bounds or a valid AOI is required", false)
	}
	if err := validateAIBounds(selectionBounds); err != nil {
		return nil, warnings, err
	}
	var west, south, east, north float64
	first := true
	results := map[string]any{}
	for month, group := range groups {
		payload := map[string]any{"region_id": rid, "scene_ids": group, "bounds": selectionBounds, "aoi": in["aoi"]}
		value, phaseWarnings, callErr := d.pyJSON(ctx, "POST", "/py/local/selection-pixels", payload)
		warnings = append(warnings, phaseWarnings...)
		if callErr != nil {
			return nil, warnings, callErr
		}
		results[month] = value
		b := boundsByMonth[month]
		if first {
			west, south, east, north, first = b[0], b[1], b[2], b[3], false
		} else {
			if b[0] > west {
				west = b[0]
			}
			if b[1] > south {
				south = b[1]
			}
			if b[2] < east {
				east = b[2]
			}
			if b[3] < north {
				north = b[3]
			}
		}
	}
	if !first && (west >= east || south >= north) {
		return nil, warnings, coded("NO_SPATIAL_INTERSECTION", "selected phases have no valid spatial intersection", false)
	}
	return map[string]any{"region_id": rid, "phases": results, "phase_count": len(results), "intersection_bounds": []float64{west, south, east, north}}, warnings, nil
}

func groupSceneIDsByMonth(raw any, ids []string) (map[string][]string, map[string][4]float64, error) {
	m, ok := raw.(map[string]any)
	if !ok {
		return nil, nil, coded("CATALOG_INVALID", "imagery catalog response is invalid", true)
	}
	byID := map[string]map[string]any{}
	if scenes, ok := m["scenes"].([]any); ok {
		for _, value := range scenes {
			if row, ok := value.(map[string]any); ok {
				byID[str(row["id"])] = row
			}
		}
	}
	groups := map[string][]string{}
	bounds := map[string][4]float64{}
	for _, id := range ids {
		row, ok := byID[id]
		if !ok || !boolVal(row["available"], true) {
			return nil, nil, coded("SCENE_NOT_FOUND", "selected scene is unavailable for this region", false)
		}
		dateValue := str(row["date"])
		if len(dateValue) < 7 {
			return nil, nil, coded("DATE_INVALID", "selected scene has no usable date", false)
		}
		month := dateValue[:7]
		groups[month] = append(groups[month], id)
		b := arrayBounds(row["bounds"])
		if current, exists := bounds[month]; !exists {
			bounds[month] = b
		} else {
			// Scenes in the same month are mosaic members: union their
			// envelopes before checking coverage across different phases.
			if b[0] < current[0] {
				current[0] = b[0]
			}
			if b[1] < current[1] {
				current[1] = b[1]
			}
			if b[2] > current[2] {
				current[2] = b[2]
			}
			if b[3] > current[3] {
				current[3] = b[3]
			}
			bounds[month] = current
		}
	}
	return groups, bounds, nil
}

func (d *AIDomain) createPlan(ctx context.Context, c ai.Call) (any, []string, error) {
	in := c.Input
	rid := str(in["region_id"])
	if rid == "" {
		return nil, nil, coded("INVALID_INPUT", "region_id is required", false)
	}
	ids := stringSlice(in["scene_ids"])
	if duplicate := duplicateAIString(ids); duplicate != "" {
		return nil, nil, coded("INVALID_INPUT", "scene_ids must not contain duplicate scene references", false)
	}
	before, after := str(in["before_id"]), str(in["after_id"])
	policy := strDefault(str(in["pair_policy"]), "first_last")
	if policy == "specified" && (before == "" || after == "") {
		return nil, nil, coded("PAIR_INVALID", "specified pairing requires before_id and after_id", false)
	}
	if len(ids) < 2 && (before == "" || after == "") {
		return nil, nil, coded("SINGLE_PHASE", "at least two scene/image references are required", false)
	}
	if before != "" && before == after {
		return nil, nil, coded("NO_OVERLAP", "before and after must differ", false)
	}
	if policy != "first_last" && policy != "adjacent" && policy != "specified" {
		return nil, nil, coded("INVALID_INPUT", "pair_policy must be first_last, adjacent or specified", false)
	}
	if policy == "specified" && (before == "" || after == "") {
		return nil, nil, coded("PAIR_INVALID", "specified pairing requires before_id and after_id", false)
	}
	if policy == "adjacent" && len(ids) < 2 {
		return nil, nil, coded("PAIR_INVALID", "adjacent pairing requires at least two scene_ids", false)
	}
	// Explicit pair references must be part of the frozen set even when the
	// caller also supplied unrelated scenes.
	if before != "" && after != "" {
		for _, pairID := range []string{before, after} {
			if !containsAIString(ids, pairID) {
				ids = append(ids, pairID)
			}
		}
	}
	if err := validateAIPolygon(in["aoi"]); err != nil {
		return nil, nil, err
	}
	planBounds := in["bounds"]
	if planBounds == nil {
		planBounds = geometryBounds(in["aoi"])
	}
	if bounds, ok := in["bounds"]; ok {
		if err := validateAIBounds(bounds); err != nil {
			return nil, nil, err
		}
	}
	planID := uuid.NewString()
	rev := int64Val(in["config_revision"])
	modelID := str(in["model_id"])
	if modelID == "" {
		if r, e := d.d.DB.ActiveRevision(); e == nil {
			rev = r.ID
			modelID = r.Settings.DefaultModel
		}
	}
	if modelID == "" {
		return nil, nil, coded("MODEL_NOT_READY", "no default model is configured", false)
	}
	selectedModel, modelErr := d.d.DB.Model(modelID)
	if modelErr != nil || !selectedModel.Enabled || selectedModel.Status != "ready" {
		return nil, nil, coded("MODEL_NOT_READY", "selected model is not ready", false)
	}
	if r, e := d.d.DB.ActiveRevision(); e == nil && rev != 0 && r.ID != rev {
		return nil, nil, coded("CONFIG_STALE", "configuration revision changed; recreate the plan", false)
	}
	if rev == 0 {
		if r, e := d.d.DB.ActiveRevision(); e == nil {
			rev = r.ID
		}
	}
	dateStart, dateEnd := normalizeDateSlot(in["start_date"]), normalizeDateSlot(in["end_date"])
	if err := validateDateRange(dateStart, dateEnd); err != nil {
		return nil, nil, err
	}
	var sceneCatalog any
	if len(ids) > 0 || before != "" || after != "" {
		var catalogErr error
		var warnings []string
		sceneCatalog, warnings, catalogErr = d.pyJSON(ctx, "GET", "/py/local/regions/"+urlPart(rid)+"/scenes?refresh=false", nil)
		_ = warnings
		if catalogErr != nil {
			return nil, nil, catalogErr
		} else if err := validatePlanScenes(sceneCatalog, rid, ids, before, after); err != nil {
			return nil, nil, err
		}
	}
	orderedIDs, actualDates, missingMonths, phaseErr := planPhaseMetadata(sceneCatalog, ids, before, after, dateStart, dateEnd)
	if phaseErr != nil {
		return nil, nil, phaseErr
	}
	ids = orderedIDs
	if len(ids) < 2 {
		return nil, nil, coded("SINGLE_PHASE", "at least two distinct scene/image references are required", false)
	}
	phaseKeys := selectedScenePhaseKeys(sceneCatalog, ids)
	if len(phaseKeys) < 2 {
		return nil, nil, coded("SINGLE_PHASE", "selected imagery contains fewer than two distinct dates", false)
	}
	if policy == "specified" && (!containsAIString(ids, before) || !containsAIString(ids, after)) {
		return nil, nil, coded("NO_COVERAGE", "specified pair does not intersect the requested date range", false)
	}
	if policy == "specified" && scenePhaseKeyForID(sceneCatalog, before) == scenePhaseKeyForID(sceneCatalog, after) {
		return nil, nil, coded("NO_OVERLAP", "specified pair must use two distinct dates", false)
	}
	if policy == "first_last" {
		before, after = ids[0], ids[len(ids)-1]
	}
	if policy == "adjacent" && len(missingMonths) > 0 {
		return nil, nil, coded("MISSING_PHASES", "monthly detection has missing months: "+strings.Join(missingMonths, ", "), false)
	}
	catalogVersion, phaseVersions := catalogSnapshot(sceneCatalog)
	if actualDates == nil {
		actualDates = []any{}
	}
	if missingMonths == nil {
		missingMonths = []string{}
	}
	doc := map[string]any{"plan_id": planID, "region_id": rid, "scene_ids": ids, "before_id": before, "after_id": after, "pair_policy": policy, "aoi": sanitize(in["aoi"]), "aoi_source": strDefault(str(in["aoi_source"]), "user_drawn"), "bounds": planBounds, "model_id": modelID, "model_hash": selectedModel.Hash, "model_mapping": selectedModel.Mapping, "config_revision": rev, "start_date": dateStart, "end_date": dateEnd, "catalog_version": catalogVersion, "phase_versions": phaseVersions, "status": "frozen", "created_at": time.Now().UTC().Format(time.RFC3339)}
	doc["actual_dates"], doc["missing_months"] = actualDates, missingMonths
	b, _ := json.Marshal(doc)
	if err := d.d.DB.SaveAIDocument(store.AIDocument{ID: planID, Kind: "plan", Status: "frozen", RequestID: c.RequestID, Fingerprint: fingerprint(c.Input), Document: b}); err != nil {
		return nil, nil, coded("PLAN_SAVE_FAILED", "could not persist detection plan", true)
	}
	return publicPlan(doc), nil, nil
}

func publicPlan(doc map[string]any) map[string]any {
	out := map[string]any{}
	for key, value := range doc {
		if key == "params" || key == "model_hash" || key == "model_mapping" {
			continue
		}
		out[key] = sanitize(value)
	}
	return out
}

func planPhaseMetadata(raw any, ids []string, before, after string, start, end map[string]any) ([]string, []any, []string, error) {
	m, ok := raw.(map[string]any)
	if !ok {
		selected := append([]string{}, ids...)
		if len(selected) == 0 {
			selected = append(selected, before, after)
		}
		return selected, nil, nil, nil
	}
	rows, _ := m["scenes"].([]any)
	byID := map[string]map[string]any{}
	for _, item := range rows {
		row, ok := item.(map[string]any)
		if !ok {
			continue
		}
		id := str(row["id"])
		if id != "" {
			byID[id] = row
		}
	}
	selected := append([]string{}, ids...)
	if len(selected) == 0 {
		selected = append(selected, before, after)
	}
	requestedStart, requestedEnd := dateBoundary(start, false), dateBoundary(end, true)
	withinRange := selected[:0]
	for _, id := range selected {
		row := byID[id]
		if row == nil {
			continue
		}
		dateValue := str(row["date"])
		if len(dateValue) < 7 {
			continue
		}
		sceneStart, sceneEnd := sceneDateBounds(row)
		if sceneStart.IsZero() || sceneEnd.IsZero() {
			continue
		}
		if !requestedStart.IsZero() && sceneEnd.Before(requestedStart) || !requestedEnd.IsZero() && sceneStart.After(requestedEnd) {
			continue
		}
		withinRange = append(withinRange, id)
	}
	selected = withinRange
	if len(selected) == 0 {
		return nil, nil, nil, coded("NO_COVERAGE", "no selected imagery intersects the requested date range", false)
	}
	sort.SliceStable(selected, func(i, j int) bool {
		left, _ := sceneDateBounds(byID[selected[i]])
		right, _ := sceneDateBounds(byID[selected[j]])
		if left.Equal(right) {
			return selected[i] < selected[j]
		}
		return left.Before(right)
	})
	actual := make([]any, 0, len(selected))
	seenActual := map[string]bool{}
	for _, id := range selected {
		if row := byID[id]; row != nil {
			if slot := sceneDateSlot(row); slot != nil {
				key := scenePhaseKey(row)
				if !seenActual[key] {
					actual = append(actual, slot)
					seenActual[key] = true
				}
			}
		}
	}
	availableMonths := map[string]bool{}
	for _, row := range byID {
		if row == nil || strDefault(str(row["date_precision"]), "day") == "year" {
			continue
		}
		from, to := sceneDateBounds(row)
		if from.IsZero() || to.IsZero() {
			continue
		}
		for cursor := time.Date(from.Year(), from.Month(), 1, 0, 0, 0, 0, time.UTC); !cursor.After(to); cursor = cursor.AddDate(0, 1, 0) {
			availableMonths[cursor.Format("2006-01")] = true
		}
	}
	missing := []string{}
	if start != nil && end != nil {
		from, to := dateBoundary(start, false), dateBoundary(end, true)
		if !from.IsZero() && !to.IsZero() {
			for cursor := time.Date(from.Year(), from.Month(), 1, 0, 0, 0, 0, time.UTC); !cursor.After(to); cursor = cursor.AddDate(0, 1, 0) {
				month := cursor.Format("2006-01")
				if !availableMonths[month] {
					missing = append(missing, month)
				}
			}
		}
	}
	return selected, actual, missing, nil
}

func selectedScenePhaseKeys(raw any, ids []string) map[string]bool {
	keys := map[string]bool{}
	m, ok := raw.(map[string]any)
	if !ok {
		return keys
	}
	rows, _ := m["scenes"].([]any)
	byID := map[string]map[string]any{}
	for _, item := range rows {
		if row, ok := item.(map[string]any); ok {
			byID[str(row["id"])] = row
		}
	}
	for _, id := range ids {
		if key := scenePhaseKey(byID[id]); key != "" {
			keys[key] = true
		}
	}
	return keys
}

func scenePhaseKey(row map[string]any) string {
	if row == nil {
		return ""
	}
	slot := sceneDateSlot(row)
	if slot == nil {
		return ""
	}
	start, _ := sceneDateBounds(row)
	if start.IsZero() {
		return ""
	}
	return start.UTC().Format("2006-01-02")
}

func scenePhaseKeyForID(raw any, id string) string {
	return scenePhaseKey(sceneByID(raw, id))
}

func sceneByID(raw any, id string) map[string]any {
	m, ok := raw.(map[string]any)
	if !ok {
		return nil
	}
	rows, _ := m["scenes"].([]any)
	for _, item := range rows {
		if row, ok := item.(map[string]any); ok && str(row["id"]) == id {
			return row
		}
	}
	return nil
}

func (d *AIDomain) prepare(ctx context.Context, c ai.Call) (any, []string, error) {
	in := c.Input
	planID := str(in["plan_id"])
	if planID == "" {
		return nil, nil, coded("INVALID_INPUT", "plan_id is required", false)
	}
	doc, err := d.d.DB.GetAIDocument(planID)
	if err != nil || doc.Kind != "plan" {
		return nil, nil, coded("PLAN_NOT_FOUND", "detection plan not found", false)
	}
	if stale, reason := d.planStale(doc); stale {
		return nil, nil, coded("PLAN_STALE", reason, false)
	}
	if c.Context.WorkflowID != "" && c.Context.WorkflowID != planID {
		_ = c.Context.WorkflowID
	}
	opID := uuid.NewString()
	op := map[string]any{"operation_id": opID, "kind": "imagery.prepare", "plan_id": planID, "status": "queued", "created_at": time.Now().UTC().Format(time.RFC3339)}
	b, _ := json.Marshal(op)
	if err = d.d.DB.SaveAIDocument(store.AIDocument{ID: opID, Kind: "operation", Status: "queued", RequestID: c.RequestID, Fingerprint: fingerprint(c.Input), Document: b}); err != nil {
		return nil, nil, coded("OPERATION_SAVE_FAILED", "could not persist operation", true)
	}
	go d.runPrepare(ctx, opID, doc)
	return op, nil, nil
}
func (d *AIDomain) runPrepare(parent context.Context, opID string, plan store.AIDocument) {
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Hour)
	defer cancel()
	var p map[string]any
	_ = json.Unmarshal(plan.Document, &p)
	startDate, endDate := "0001-01-01", "9999-12-31"
	if slot, ok := p["start_date"].(map[string]any); ok {
		startDate = dateSlotBoundary(slot, false, startDate)
	} else {
		startDate = strDefault(str(p["start_date"]), startDate)
	}
	if slot, ok := p["end_date"].(map[string]any); ok {
		endDate = dateSlotBoundary(slot, true, endDate)
	} else {
		endDate = strDefault(str(p["end_date"]), endDate)
	}
	req := localFetchReq{AOI: mustJSON(p["aoi"]), Bounds: arrayBounds(p["bounds"]), RegionID: str(p["region_id"]), SceneIDs: mustJSON(stringSlice(p["scene_ids"])), StartDate: startDate, EndDate: endDate, ModelID: str(p["model_id"]), Revision: int64Val(p["config_revision"]), Params: mapVal(p["params"]), MaxImages: 0, Zoom: 18, Buffer: 500}
	res, err := LocalFetchService(ctx, d.d, req)
	if err != nil {
		d.updateOp(opID, "failed", nil, "imagery preparation failed")
		return
	}
	ids := make([]string, 0, len(res.Items))
	imageSceneIDs := make([]any, 0, len(res.Items))
	for _, im := range res.Items {
		ids = append(ids, im.ID)
		var metadata map[string]any
		if json.Unmarshal([]byte(im.MetaJSON), &metadata) == nil {
			imageSceneIDs = append(imageSceneIDs, stringSlice(metadata["scene_ids"]))
		} else {
			imageSceneIDs = append(imageSceneIDs, []string{})
		}
	}
	legacySourceIDs := stringSlice(p["scene_ids"])
	if len(legacySourceIDs) != len(ids) {
		legacySourceIDs = nil
	}
	// LocalFetchService preserves the selected catalog order.  Keep the opaque
	// source IDs alongside prepared image IDs so specified/adjacent pairing can
	// never silently fall back to the first and last generated images.
	d.updateOp(opID, "succeeded", map[string]any{"operation_id": opID, "status": "succeeded", "image_count": len(ids), "image_ids": ids, "image_scene_ids": imageSceneIDs, "source_scene_ids": legacySourceIDs, "batch_id": res.BatchID}, "")
}

func (d *AIDomain) submitWorkflow(ctx context.Context, c ai.Call) (any, []string, error) {
	id := str(c.Input["plan_id"])
	doc, err := d.d.DB.GetAIDocument(id)
	if err != nil || doc.Kind != "plan" {
		return nil, nil, coded("PLAN_NOT_FOUND", "detection plan not found", false)
	}
	if stale, reason := d.planStale(doc); stale {
		return nil, nil, coded("PLAN_STALE", reason, false)
	}
	var p map[string]any
	_ = json.Unmarshal(doc.Document, &p)
	operationID := str(c.Input["operation_id"])
	if operationID == "" {
		ops, listErr := d.d.DB.ListAIDocuments("operation")
		if listErr != nil {
			return nil, nil, coded("OPERATION_NOT_FOUND", "imagery operation not found", false)
		}
		for _, candidate := range ops {
			var value map[string]any
			if json.Unmarshal(candidate.Document, &value) == nil && str(value["plan_id"]) == id && str(value["status"]) == "succeeded" {
				operationID = candidate.ID
				break
			}
		}
		if operationID == "" {
			return nil, nil, coded("OPERATION_NOT_READY", "imagery preparation is not complete", true)
		}
	}
	opDoc, opErr := d.d.DB.GetAIDocument(operationID)
	if opErr != nil || opDoc.Kind != "operation" {
		return nil, nil, coded("OPERATION_NOT_FOUND", "imagery operation not found", false)
	}
	var op map[string]any
	_ = json.Unmarshal(opDoc.Document, &op)
	if planRef := str(op["plan_id"]); planRef != "" && planRef != id {
		return nil, nil, coded("OPERATION_PLAN_MISMATCH", "imagery operation belongs to another detection plan", false)
	}
	if str(op["status"]) != "succeeded" {
		return nil, nil, coded("OPERATION_NOT_READY", "imagery preparation is not complete", true)
	}
	resultMap, ok := op["result"].(map[string]any)
	if !ok {
		return nil, nil, coded("OPERATION_NOT_READY", "imagery preparation has no result", true)
	}
	batchID := str(resultMap["batch_id"])
	imageIDs := stringSlice(resultMap["image_ids"])
	imageSceneIDs := nestedStringSlice(resultMap["image_scene_ids"])
	if len(imageSceneIDs) == 0 {
		legacy := stringSlice(resultMap["source_scene_ids"])
		for _, source := range legacy {
			imageSceneIDs = append(imageSceneIDs, []string{source})
		}
	}
	if len(imageIDs) < 2 {
		return nil, nil, coded("SINGLE_PHASE", "imagery preparation produced fewer than two images", false)
	}
	before, after := str(p["before_id"]), str(p["after_id"])
	if str(p["pair_policy"]) != "adjacent" {
		imageForSource := func(source string) string {
			for i, candidates := range imageSceneIDs {
				for _, candidate := range candidates {
					if candidate == source && i < len(imageIDs) {
						return imageIDs[i]
					}
				}
			}
			return ""
		}
		if mapped := imageForSource(before); mapped != "" {
			before = mapped
		} else if _, err = d.d.DB.GetImage(before); err != nil {
			return nil, nil, coded("PAIR_NOT_IN_BATCH", "before scene is not present in the prepared batch", false)
		}
		if mapped := imageForSource(after); mapped != "" {
			after = mapped
		} else if _, err = d.d.DB.GetImage(after); err != nil {
			return nil, nil, coded("PAIR_NOT_IN_BATCH", "after scene is not present in the prepared batch", false)
		}
		if before == after {
			return nil, nil, coded("SINGLE_PHASE", "workflow requires two distinct images", false)
		}
	}
	wfID := uuid.NewString()
	w := map[string]any{"workflow_id": wfID, "plan_id": id, "status": "queued", "task_ids": []string{}, "created_at": time.Now().UTC().Format(time.RFC3339)}
	b, _ := json.Marshal(w)
	if err = d.d.DB.SaveAIDocument(store.AIDocument{ID: wfID, Kind: "workflow", Status: "queued", RequestID: c.RequestID, Fingerprint: fingerprint(c.Input), Document: b}); err != nil {
		return nil, nil, coded("WORKFLOW_SAVE_FAILED", "could not persist workflow", true)
	}
	ids := [][2]string{{before, after}}
	if str(p["pair_policy"]) == "adjacent" {
		ids = make([][2]string, 0, len(imageIDs)-1)
		for i := 0; i < len(imageIDs)-1; i++ {
			ids = append(ids, [2]string{imageIDs[i], imageIDs[i+1]})
		}
	}
	// model_id identifies the frozen managed model; the task API's algorithm
	// remains the registered detector name and is not an arbitrary model ID.
	algo := "hebei_onnx"
	params := map[string]any{}
	if p["params"] != nil {
		if pm, ok := p["params"].(map[string]any); ok {
			params = pm
		}
	}
	if d.d.Q == nil {
		return nil, nil, coded("QUEUE_UNAVAILABLE", "task queue unavailable", true)
	}
	taskIDs := []string{}
	for _, pair := range ids {
		t, e := CreateTaskService(ctx, d.d, model.CreateTaskReq{BatchID: batchID, BeforeID: pair[0], AfterID: pair[1], Algorithm: algo, Params: params})
		if e != nil {
			w["task_ids"] = taskIDs
			w["status"] = "partial_failed"
			w["error_code"] = errorCode(e)
			w["error"] = safeError(e)
			if partial, marshalErr := json.Marshal(w); marshalErr == nil {
				_ = d.d.DB.SaveAIDocument(store.AIDocument{ID: wfID, Kind: "workflow", Status: "partial_failed", RequestID: c.RequestID, Fingerprint: fingerprint(c.Input), Document: partial})
			}
			return nil, nil, coded("WORKFLOW_PARTIAL", "detection workflow could not submit all pairs", retryableError(e))
		}
		taskIDs = append(taskIDs, t.ID)
	}
	w["task_ids"] = taskIDs
	w["status"] = "submitted"
	b, _ = json.Marshal(w)
	_ = d.d.DB.SaveAIDocument(store.AIDocument{ID: wfID, Kind: "workflow", Status: "submitted", RequestID: c.RequestID, Fingerprint: fingerprint(c.Input), Document: b})
	return w, nil, nil
}

func (d *AIDomain) planStale(doc store.AIDocument) (bool, string) {
	var p map[string]any
	if json.Unmarshal(doc.Document, &p) != nil {
		return true, "detection plan is invalid"
	}
	rev := int64Val(p["config_revision"])
	if rev != 0 {
		r, err := d.d.DB.ActiveRevision()
		if err != nil || r.ID != rev {
			return true, "configuration revision changed; recreate the plan"
		}
	}
	modelID := str(p["model_id"])
	if modelID != "" {
		m, err := d.d.DB.Model(modelID)
		if err != nil || !m.Enabled || m.Status != "ready" {
			return true, "selected model is no longer ready"
		}
		if expectedHash := str(p["model_hash"]); expectedHash != "" && m.Hash != expectedHash {
			return true, "selected model changed; recreate the plan"
		}
		if expectedMapping, ok := p["model_mapping"].(map[string]any); ok {
			current, _ := json.Marshal(m.Mapping)
			expected, _ := json.Marshal(expectedMapping)
			if string(current) != string(expected) {
				return true, "selected model mapping changed; recreate the plan"
			}
		}
	}
	if expected := str(p["catalog_version"]); expected != "" && d.d.Py != nil {
		regionID := urlPart(str(p["region_id"]))
		if raw, _, e := d.pyJSON(context.Background(), "GET", "/py/local/regions/"+regionID+"/scenes?refresh=false", nil); e != nil {
			return true, "imagery catalog is unavailable; refresh the plan"
		} else if current, _ := catalogSnapshot(raw); current != expected {
			return true, "imagery catalog changed; recreate the plan"
		}
	}
	return false, ""
}

// validatePlanScenes checks opaque scene references against the authoritative
// region response before a plan is frozen.  This prevents a later crop from
// silently selecting a different region or silently ignoring a typo.
func validatePlanScenes(raw any, regionID string, sceneIDs []string, before, after string) error {
	m, ok := raw.(map[string]any)
	if !ok {
		return coded("CATALOG_INVALID", "imagery catalog response is invalid", true)
	}
	if got := str(m["region_id"]); got != "" && got != regionID {
		return coded("REGION_MISMATCH", "imagery catalog belongs to another region", false)
	}
	known := map[string]bool{}
	if scenes, ok := m["scenes"].([]any); ok {
		for _, item := range scenes {
			if row, ok := item.(map[string]any); ok {
				if id := str(row["id"]); id != "" && boolVal(row["available"], true) {
					known[id] = true
				}
			}
		}
	}
	if len(known) == 0 {
		return coded("NO_IMAGERY", "no usable imagery is available for this region", false)
	}
	for _, id := range append(append([]string{}, sceneIDs...), before, after) {
		if id != "" && !known[id] {
			return coded("SCENE_NOT_FOUND", "selected scene is unavailable for this region", false)
		}
	}
	return nil
}

func catalogSnapshot(raw any) (string, map[string]string) {
	m, ok := raw.(map[string]any)
	if !ok {
		return "", nil
	}
	// Hash stable catalog metadata that affects pairing, coverage, and
	// availability. Display labels and upstream paths must never become part of
	// a public plan reference.
	ids := []string{}
	if scenes, ok := m["scenes"].([]any); ok {
		for _, item := range scenes {
			if row, ok := item.(map[string]any); ok {
				snapshot := map[string]any{
					"id":             str(row["id"]),
					"version":        str(row["version"]),
					"date":           str(row["date"]),
					"date_precision": strDefault(str(row["date_precision"]), "day"),
					"available":      boolVal(row["available"], true),
					"bounds":         arrayBounds(row["bounds"]),
				}
				encoded, _ := json.Marshal(snapshot)
				ids = append(ids, string(encoded))
			}
		}
	}
	sort.Strings(ids)
	phases := map[string]string{}
	if values, ok := m["phases"].([]any); ok {
		for _, item := range values {
			if phase, ok := item.(map[string]any); ok {
				if id := str(phase["id"]); id != "" {
					phases[id] = str(phase["version"])
				}
			}
		}
	}
	phaseSnapshots := map[string]any{}
	if values, ok := m["phases"].([]any); ok {
		for _, item := range values {
			if phase, ok := item.(map[string]any); ok {
				if id := str(phase["id"]); id != "" {
					phaseSnapshots[id] = map[string]any{
						"version":   str(phase["version"]),
						"month":     str(phase["month"]),
						"available": boolVal(phase["available"], true),
						"bounds":    arrayBounds(phase["bounds"]),
					}
				}
			}
		}
	}
	b, _ := json.Marshal(map[string]any{"scenes": ids, "phases": phaseSnapshots})
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:]), phases
}

func validateAIPolygon(v any) error {
	aoi, ok := v.(map[string]any)
	if !ok {
		return coded("AOI_INVALID", "AOI must be a GeoJSON Polygon", false)
	}
	if str(aoi["type"]) != "Polygon" {
		return coded("AOI_INVALID", "a single Polygon is required; MultiPolygon is not accepted", false)
	}
	rings, ok := aoi["coordinates"].([]any)
	if !ok || len(rings) != 1 {
		return coded("AOI_INVALID", "AOI must be a single Polygon without holes", false)
	}
	pointsRaw, ok := rings[0].([]any)
	if !ok || len(pointsRaw) < 4 {
		return coded("AOI_INVALID", "AOI polygon ring must contain at least four points", false)
	}
	points := make([][2]float64, len(pointsRaw))
	for i, raw := range pointsRaw {
		point, ok := raw.([]any)
		if !ok || len(point) != 2 {
			return coded("AOI_INVALID", "AOI polygon coordinates must be [longitude, latitude]", false)
		}
		points[i] = [2]float64{floatVal(point[0], math.NaN()), floatVal(point[1], math.NaN())}
		if math.IsNaN(points[i][0]) || math.IsNaN(points[i][1]) || points[i][0] < -180 || points[i][0] > 180 || points[i][1] < -90 || points[i][1] > 90 {
			return coded("AOI_INVALID", "AOI polygon coordinates must be WGS84", false)
		}
	}
	if points[0] != points[len(points)-1] {
		return coded("AOI_INVALID", "AOI polygon ring must be closed", false)
	}
	for i := 0; i < len(points)-1; i++ {
		if points[i] == points[i+1] && i+1 < len(points)-1 {
			return coded("AOI_INVALID", "AOI polygon has a zero-length edge", false)
		}
		for j := i + 1; j < len(points)-1; j++ {
			if j == i+1 || (i == 0 && j == len(points)-2) {
				continue
			}
			if segmentsIntersect(points[i], points[i+1], points[j], points[j+1]) {
				return coded("AOI_INVALID", "AOI polygon must not self-intersect", false)
			}
		}
	}
	return nil
}

func segmentsIntersect(a, b, c, d [2]float64) bool {
	orient := func(p, q, r [2]float64) float64 { return (q[0]-p[0])*(r[1]-p[1]) - (q[1]-p[1])*(r[0]-p[0]) }
	on := func(p, q, r [2]float64) bool {
		return math.Min(p[0], r[0]) <= q[0] && q[0] <= math.Max(p[0], r[0]) && math.Min(p[1], r[1]) <= q[1] && q[1] <= math.Max(p[1], r[1])
	}
	eps := 1e-12
	o1, o2, o3, o4 := orient(a, b, c), orient(a, b, d), orient(c, d, a), orient(c, d, b)
	if math.Abs(o1) < eps && on(a, c, b) || math.Abs(o2) < eps && on(a, d, b) || math.Abs(o3) < eps && on(c, a, d) || math.Abs(o4) < eps && on(c, b, d) {
		return true
	}
	return (o1 > 0) != (o2 > 0) && (o3 > 0) != (o4 > 0)
}

func (d *AIDomain) operationStatus(in map[string]any) (any, []string, error) {
	id := str(in["operation_id"])
	doc, err := d.d.DB.GetAIDocument(id)
	if err != nil || doc.Kind != "operation" {
		return nil, nil, coded("OPERATION_NOT_FOUND", "operation not found", false)
	}
	var out any
	_ = json.Unmarshal(doc.Document, &out)
	return sanitize(out), nil, nil
}
func (d *AIDomain) updateOp(id, status string, result any, msg string) {
	doc, err := d.d.DB.GetAIDocument(id)
	if err != nil {
		return
	}
	var out map[string]any
	_ = json.Unmarshal(doc.Document, &out)
	out["status"] = status
	if msg != "" {
		out["error"] = msg
	}
	if result != nil {
		out["result"] = sanitize(result)
	}
	b, _ := json.Marshal(out)
	doc.Status = status
	doc.Document = b
	doc.UpdatedAt = time.Now().UTC()
	_ = d.d.DB.SaveAIDocument(doc)
}
func (d *AIDomain) exportPrepare(in map[string]any) (any, []string, error) {
	id := str(in["task_id"])
	format := strings.ToLower(strDefault(str(in["format"]), "geojson"))
	if _, err := d.d.DB.GetResult(id); err != nil {
		return nil, nil, coded("RESULT_NOT_READY", "result is not ready", false)
	}
	allowed := map[string]string{"geojson": "change.geojson", "csv": "stats.csv", "shp": "change.zip", "tiff": "mask.tif"}
	file, ok := allowed[format]
	if !ok {
		return nil, nil, coded("INVALID_INPUT", "format must be geojson, csv, shp or tiff", false)
	}
	return map[string]any{"result_id": id, "format": format, "download_ref": "/api/tasks/" + urlPart(id) + "/result/" + file}, nil, nil
}

func (d *AIDomain) GetWorkflow(ctx context.Context, id string) (any, error) {
	doc, e := d.d.DB.GetAIDocument(id)
	if e != nil || doc.Kind != "workflow" {
		return nil, e
	}
	var out map[string]any
	if err := json.Unmarshal(doc.Document, &out); err != nil {
		return nil, coded("WORKFLOW_INVALID", "workflow document is invalid", false)
	}
	taskIDs := stringSlice(out["task_ids"])
	tasks := make([]any, 0, len(taskIDs))
	counts := map[string]int{}
	for _, taskID := range taskIDs {
		task, err := d.d.DB.GetTask(taskID)
		if err != nil {
			counts["missing"]++
			continue
		}
		dto := taskDTO(store.AITask{ID: task.ID, BeforeID: task.BeforeID, AfterID: task.AfterID, Algorithm: task.Algorithm, Status: task.Status, Stage: task.Stage, Progress: task.Progress, ErrorMsg: task.ErrorMsg, CreatedAt: task.CreatedAt})
		tasks = append(tasks, dto)
		counts[task.Status]++
	}
	if len(taskIDs) > 0 {
		out["tasks"] = tasks
		out["task_summary"] = counts
		if counts[model.TaskFailed] > 0 {
			out["status"] = "failed"
		} else if counts[model.TaskPending]+counts[model.TaskRunning] > 0 {
			out["status"] = "running"
		} else if counts[model.TaskSucceeded]+counts[model.TaskCompleted] == len(taskIDs) {
			out["status"] = "succeeded"
		}
	}
	return sanitize(out), nil
}
func (d *AIDomain) GetEvents(ctx context.Context, id string, after int64) ([]map[string]any, error) {
	v, e := d.GetWorkflow(ctx, id)
	if e != nil {
		return nil, e
	}
	return []map[string]any{{"id": after + 1, "type": "workflow", "data": v}}, nil
}
func (d *AIDomain) CancelWorkflow(ctx context.Context, id string) error {
	doc, e := d.d.DB.GetAIDocument(id)
	if e != nil {
		return coded("WORKFLOW_NOT_FOUND", "workflow not found", false)
	}
	if doc.Kind != "workflow" {
		return coded("WORKFLOW_NOT_FOUND", "workflow not found", false)
	}
	var out map[string]any
	if err := json.Unmarshal(doc.Document, &out); err != nil {
		return coded("WORKFLOW_INVALID", "workflow document is invalid", false)
	}
	if taskIDs := stringSlice(out["task_ids"]); len(taskIDs) > 0 {
		return coded("WORKFLOW_NOT_CANCELLABLE", "submitted detection tasks cannot be cancelled through this endpoint", false)
	}
	if status := str(out["status"]); status == "succeeded" || status == "failed" || status == "cancelled" {
		return coded("WORKFLOW_FINISHED", "workflow already finished", false)
	}
	out["status"] = "cancelled"
	b, _ := json.Marshal(out)
	doc.Status = "cancelled"
	doc.Document = b
	return d.d.DB.SaveAIDocument(doc)
}

func fingerprint(v any) string {
	b, _ := json.Marshal(v)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func cloneMap(in map[string]any) map[string]any {
	if in == nil {
		return nil
	}
	out := make(map[string]any, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}
func str(v any) string {
	if s, ok := v.(string); ok {
		return strings.TrimSpace(s)
	}
	return ""
}
func strDefault(v, def string) string {
	if strings.TrimSpace(v) == "" {
		return def
	}
	return v
}
func intVal(v any, def int) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	case json.Number:
		i, _ := n.Int64()
		return int(i)
	}
	return def
}
func int64Val(v any) int64 { return int64(intVal(v, 0)) }
func boolVal(v any, def bool) bool {
	if b, ok := v.(bool); ok {
		return b
	}
	return def
}
func geometryBounds(v any) any {
	geometry, ok := v.(map[string]any)
	if !ok {
		return nil
	}
	coordinates := geometry["coordinates"]
	var west, south, east, north float64
	first := true
	var visit func(any)
	visit = func(value any) {
		switch item := value.(type) {
		case []any:
			if len(item) == 2 {
				if lon, ok := item[0].(float64); ok {
					if lat, ok := item[1].(float64); ok {
						if first {
							west, east, south, north, first = lon, lon, lat, lat, false
						} else {
							if lon < west {
								west = lon
							}
							if lon > east {
								east = lon
							}
							if lat < south {
								south = lat
							}
							if lat > north {
								north = lat
							}
						}
						return
					}
				}
			}
			for _, child := range item {
				visit(child)
			}
		}
	}
	visit(coordinates)
	if first {
		return nil
	}
	return []float64{west, south, east, north}
}
func floatVal(v any, def float64) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case float32:
		return float64(n)
	case int:
		return float64(n)
	}
	return def
}
func stringSlice(v any) []string {
	a, ok := v.([]any)
	if !ok {
		if s, ok := v.([]string); ok {
			return s
		}
		return nil
	}
	out := make([]string, 0, len(a))
	for _, x := range a {
		if s := str(x); s != "" {
			out = append(out, s)
		}
	}
	return out
}
func containsAIString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}
func duplicateAIString(values []string) string {
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		if _, exists := seen[value]; exists {
			return value
		}
		seen[value] = struct{}{}
	}
	return ""
}
func nestedStringSlice(v any) [][]string {
	values, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([][]string, 0, len(values))
	for _, value := range values {
		out = append(out, stringSlice(value))
	}
	return out
}
func mapVal(v any) map[string]any {
	if m, ok := v.(map[string]any); ok {
		return m
	}
	return nil
}
func mustJSON(v any) json.RawMessage { b, _ := json.Marshal(v); return b }
func arrayBounds(v any) [4]float64 {
	var b [4]float64
	if a, ok := v.([]float64); ok {
		for i := 0; i < 4 && i < len(a); i++ {
			b[i] = a[i]
		}
		return b
	}
	if a, ok := v.([]any); ok {
		for i := 0; i < 4 && i < len(a); i++ {
			b[i] = floatVal(a[i], 0)
		}
	}
	if a, ok := v.([4]float64); ok {
		return a
	}
	return b
}

func validateAIBounds(v any) error {
	b := arrayBounds(v)
	if b[0] < -180 || b[2] > 180 || b[1] < -90 || b[3] > 90 || b[0] >= b[2] || b[1] >= b[3] {
		return coded("BOUNDS_INVALID", "bounds must be [west,south,east,north] in WGS84", false)
	}
	return nil
}

func normalizeDateSlot(v any) map[string]any {
	if m, ok := v.(map[string]any); ok {
		value, precision := str(m["value"]), str(m["precision"])
		if value != "" && (precision == "day" || precision == "month" || precision == "year") {
			return map[string]any{"value": value, "precision": precision}
		}
	}
	if s := str(v); s != "" {
		return map[string]any{"value": s, "precision": "day"}
	}
	return nil
}

func validateDateRange(start, end map[string]any) error {
	for _, slot := range []map[string]any{start, end} {
		if slot == nil {
			continue
		}
		value, precision := str(slot["value"]), str(slot["precision"])
		if value == "" || (precision != "day" && precision != "month" && precision != "year") {
			return coded("DATE_INVALID", "date must include value and precision day, month, or year", false)
		}
		layout := "2006-01-02"
		if precision == "month" {
			layout = "2006-01"
		} else if precision == "year" {
			layout = "2006"
		}
		if _, err := time.Parse(layout, value); err != nil {
			return coded("DATE_INVALID", "date value does not match its declared precision", false)
		}
	}
	if start == nil || end == nil {
		return nil
	}
	from := dateBoundary(start, false)
	to := dateBoundary(end, true)
	if from.IsZero() || to.IsZero() {
		return coded("DATE_INVALID", "date range contains an invalid boundary", false)
	}
	if from.After(to) {
		return coded("DATE_INVALID", "start date must not be after end date", false)
	}
	return nil
}

func aiSpecs() []ai.ToolSpec {
	schema := func(fields map[string]any) map[string]any {
		return map[string]any{"$schema": "https://json-schema.org/draft/2020-12/schema", "type": "object", "additionalProperties": false, "properties": fields}
	}
	names := []struct {
		n, d, e  string
		fields   []string
		required []string
	}{
		{n: "workspace.context", d: "Read the current workspace context", e: "query"},
		{n: "region.search", d: "Find known administrative regions", e: "query", fields: []string{"query", "admin_path", "region_id"}},
		{n: "imagery.catalog", d: "Read the local imagery catalog", e: "query", fields: []string{"refresh"}},
		{n: "imagery.scenes", d: "Read scenes and phases for a region", e: "query", fields: []string{"region_id", "start_date", "end_date"}, required: []string{"region_id"}},
		{n: "imagery.phase", d: "Read one phase", e: "query", fields: []string{"region_id", "phase_id"}, required: []string{"region_id", "phase_id"}},
		{n: "model.list", d: "List ready models without paths", e: "query"},
		{n: "image.list", d: "List derived images", e: "query", fields: []string{"source", "phase", "limit"}},
		{n: "task.search", d: "Search detection tasks", e: "query", fields: []string{"status", "algorithm", "region_id", "created_after", "created_before", "offset", "limit"}},
		{n: "task.status", d: "Read one task", e: "query", fields: []string{"task_id"}, required: []string{"task_id"}},
		{n: "result.summary", d: "Read result statistics", e: "query", fields: []string{"task_id"}, required: []string{"task_id"}},
		{n: "result.patches.query", d: "Filter result patches server-side", e: "query", fields: []string{"task_id", "area_min", "area_max", "confidence_min", "limit", "sort"}, required: []string{"task_id"}},
		{n: "selection.validate", d: "Validate coverage and selection", e: "compute", fields: []string{"region_id", "scene_ids", "bounds", "aoi"}, required: []string{"region_id", "scene_ids"}},
		{n: "detection.plan.create", d: "Freeze a detection plan", e: "prepare", fields: []string{"region_id", "aoi", "aoi_source", "bounds", "scene_ids", "before_id", "after_id", "pair_policy", "model_id", "config_revision", "start_date", "end_date"}, required: []string{"region_id", "aoi", "aoi_source", "pair_policy"}},
		{n: "imagery.prepare", d: "Create derived imagery", e: "prepare", fields: []string{"plan_id"}, required: []string{"plan_id"}},
		{n: "detection.workflow.submit", d: "Submit a detection workflow", e: "compute", fields: []string{"plan_id", "operation_id"}, required: []string{"plan_id"}},
		{n: "operation.status", d: "Read a long operation", e: "query", fields: []string{"operation_id"}, required: []string{"operation_id"}},
		{n: "result.export.prepare", d: "Prepare a controlled result download", e: "artifact", fields: []string{"task_id", "format"}, required: []string{"task_id", "format"}},
	}
	out := make([]ai.ToolSpec, 0, len(names))
	for _, x := range names {
		fields := map[string]any{}
		for _, name := range x.fields {
			fields[name] = schemaField(name)
		}
		inputSchema := schema(fields)
		if len(x.required) > 0 {
			inputSchema["required"] = stringAny(x.required)
		}
		out = append(out, ai.ToolSpec{Name: x.n, Version: "1.0.0", Description: x.d, InputSchema: inputSchema, OutputSchema: aiOutputSchema(x.n), Effect: x.e, TimeoutPolicy: map[string]any{"kind": "bounded"}, RetryPolicy: map[string]any{"safe": x.e == "query"}, MaxOutputSize: 256 * 1024})
	}
	return out
}

func stringAny(values []string) []any {
	out := make([]any, len(values))
	for i, value := range values {
		out[i] = value
	}
	return out
}

func schemaField(name string) map[string]any {
	s := map[string]any{"type": "string"}
	switch name {
	case "admin_path":
		s = map[string]any{"type": "array", "items": map[string]any{"type": "string", "minLength": 1}, "minItems": 1}
	case "limit":
		s = map[string]any{"type": "integer", "minimum": 1, "maximum": 500}
	case "offset":
		s = map[string]any{"type": "integer", "minimum": 0}
	case "config_revision":
		s = map[string]any{"type": "integer", "minimum": 0}
	case "area_min", "area_max", "confidence_min":
		s = map[string]any{"type": "number", "minimum": 0}
	case "refresh":
		s = map[string]any{"type": "boolean"}
	case "scene_ids":
		s = map[string]any{"type": "array", "items": map[string]any{"type": "string", "minLength": 1}, "minItems": 1}
	case "bounds":
		s = map[string]any{"type": "array", "items": map[string]any{"type": "number"}, "minItems": 4, "maxItems": 4}
	case "aoi":
		s = map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{
			"type":        map[string]any{"type": "string", "enum": []any{"Polygon"}},
			"coordinates": map[string]any{"type": "array", "minItems": 1, "maxItems": 1, "items": map[string]any{"type": "array", "minItems": 4, "items": map[string]any{"type": "array", "minItems": 2, "maxItems": 2, "items": map[string]any{"type": "number"}}}},
		}, "required": []any{"type", "coordinates"}}
	case "pair_policy":
		s = map[string]any{"type": "string", "enum": []any{"first_last", "adjacent", "specified"}}
	case "aoi_source":
		s = map[string]any{"type": "string", "enum": []any{"admin_boundary", "user_drawn", "workspace"}}
	case "start_date", "end_date":
		s = map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{"value": map[string]any{"type": "string", "minLength": 4}, "precision": map[string]any{"type": "string", "enum": []any{"day", "month", "year"}}}, "required": []any{"value", "precision"}}
	case "format":
		s = map[string]any{"type": "string", "enum": []any{"geojson", "csv", "shp", "tiff"}}
	}
	return s
}
