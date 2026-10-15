package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"cdp/backend-go/internal/ai"
	"cdp/backend-go/internal/client"
	"cdp/backend-go/internal/model"
	"cdp/backend-go/internal/queue"
	"cdp/backend-go/internal/store"
)

func newDomainFixture(t *testing.T) (*AIDomain, *ai.Registry, *map[string]any) {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "domain.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err := db.Migrate(); err != nil {
		t.Fatal(err)
	}
	catalog := map[string]any{"region_id": "130600", "name": "保定市", "scenes": []any{
		map[string]any{"id": "s1", "date": "2025-01-01", "date_precision": "month", "available": true, "version": "v1", "bounds": []any{114.0, 38.0, 116.0, 40.0}},
		map[string]any{"id": "s2", "date": "2025-07-01", "date_precision": "month", "available": true, "version": "v2", "bounds": []any{114.0, 38.0, 116.0, 40.0}},
	}, "phases": []any{map[string]any{"id": "130600:2025-01", "region_id": "130600", "month": "2025-01", "version": "p1"}}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/py/local/regions":
			json.NewEncoder(w).Encode(map[string]any{"type": "FeatureCollection", "features": []any{map[string]any{"properties": map[string]any{"region_id": "130600", "name": "保定市", "kind": "prefecture", "admin_path": []any{"河北省", "保定市"}}, "geometry": domainAOI()}}})
		case strings.HasSuffix(r.URL.Path, "/scenes"):
			json.NewEncoder(w).Encode(catalog)
		case r.URL.Path == "/py/local/selection-pixels":
			json.NewEncoder(w).Encode(map[string]any{"width": 10, "height": 10, "pixels": 100})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)
	domain, err := NewAIDomain(&Deps{DB: db, Py: client.New(server.URL, time.Second, 1)})
	if err != nil {
		t.Fatal(err)
	}
	if err = db.SaveModel(model.ManagedModel{ID: "m", Name: "模型", Status: "ready", Enabled: true, Hash: "hash-v1", Mapping: map[string]string{"before": "input1"}}); err != nil {
		t.Fatal(err)
	}
	revision, err := db.NewRevision(model.Settings{DefaultModel: "m"})
	if err != nil {
		t.Fatal(err)
	}
	if err = db.ActivateRevision(revision); err != nil {
		t.Fatal(err)
	}
	registry, err := ai.NewRegistry(domain.Specs(), domain)
	if err != nil {
		t.Fatal(err)
	}
	return domain, registry, &catalog
}
func domainAOI() map[string]any {
	return map[string]any{"type": "Polygon", "coordinates": []any{[]any{[]any{114.5, 38.5}, []any{115.0, 38.5}, []any{115.0, 39.0}, []any{114.5, 39.0}, []any{114.5, 38.5}}}}
}
func domainCall(tool, key string, input map[string]any) ai.Call {
	return ai.Call{SchemaVersion: "tool-call.v1", RequestID: key, Tool: tool, ToolVersion: "1.0.0", Input: input}
}
func domainPlanInput() map[string]any {
	return map[string]any{"region_id": "130600", "scene_ids": []any{"s1", "s2"}, "aoi": domainAOI(), "aoi_source": "user_drawn", "pair_policy": "first_last", "start_date": map[string]any{"value": "2025-01", "precision": "month"}, "end_date": map[string]any{"value": "2025-07", "precision": "month"}}
}
func requireDomainCode(t *testing.T, r ai.Result, code string) {
	t.Helper()
	if r.Status != "error" || r.Error == nil || r.Error.Code != code {
		t.Fatalf("expected %s, got %#v", code, r)
	}
}

func TestAIDomainRejectsUnsafeOpaqueIDs(t *testing.T) {
	_, registry, _ := newDomainFixture(t)
	for _, input := range []map[string]any{
		{"region_id": "130600/../other"},
		{"region_id": "130600?refresh=true"},
		{"region_id": "130600#fragment"},
		{"region_id": "130600%2fother"},
		{"region_id": "130600", "scene_ids": []any{"scene.tif"}},
	} {
		result := registry.Invoke(context.Background(), domainCall("imagery.scenes", "unsafe-"+fingerprint(input), input))
		requireDomainCode(t, result, "INVALID_INPUT")
	}
	valid := registry.Invoke(context.Background(), domainCall("imagery.scenes", "valid-id", map[string]any{"region_id": "130600"}))
	if valid.Status != "ok" {
		t.Fatalf("valid opaque ID rejected: %#v", valid)
	}
}

func TestAIDomainPlanIdempotencyAndStale(t *testing.T) {
	domain, registry, catalog := newDomainFixture(t)
	call := domainCall("detection.plan.create", "plan-request", domainPlanInput())
	first := registry.Invoke(context.Background(), call)
	if first.Status != "ok" {
		t.Fatalf("plan: %#v", first)
	}
	second := registry.Invoke(context.Background(), call)
	if second.Status != "ok" || fingerprint(first.Data) != fingerprint(second.Data) {
		t.Fatalf("idempotent response differs: %#v %#v error=%v", first, second, second.Error)
	}
	serialized, _ := json.Marshal(second.Data)
	if strings.Contains(string(serialized), "model_hash") || strings.Contains(string(serialized), "model_mapping") {
		t.Fatal("internal plan snapshot exposed")
	}
	changed := domainPlanInput()
	changed["pair_policy"] = "adjacent"
	requireDomainCode(t, registry.Invoke(context.Background(), domainCall(call.Tool, call.RequestID, changed)), "IDEMPOTENCY_CONFLICT")
	planID := first.Data.(map[string]any)["plan_id"].(string)
	(*catalog)["scenes"].([]any)[0].(map[string]any)["version"] = "changed"
	requireDomainCode(t, registry.Invoke(context.Background(), domainCall("imagery.prepare", "prepare", map[string]any{"plan_id": planID})), "PLAN_STALE")
	domain.Close()
}

func TestAIDomainPlanIdempotencyNormalizesModelConfigDefaults(t *testing.T) {
	_, registry, _ := newDomainFixture(t)
	input := domainPlanInput()
	first := registry.Invoke(context.Background(), domainCall("detection.plan.create", "normalized-plan", input))
	if first.Status != "ok" {
		t.Fatalf("plan with omitted model/config defaults: status=%s error=%+v", first.Status, first.Error)
	}
	explicit := domainPlanInput()
	explicit["model_id"] = "m"
	explicit["config_revision"] = 1
	second := registry.Invoke(context.Background(), domainCall("detection.plan.create", "normalized-plan", explicit))
	if second.Status != "ok" || fingerprint(first.Data) != fingerprint(second.Data) {
		t.Fatalf("equivalent defaulted plan was not deduplicated: first=%#v second=%#v", first, second)
	}
}

func TestAIDomainPlanDetectsCatalogMetadataChanges(t *testing.T) {
	_, registry, catalog := newDomainFixture(t)
	plan := registry.Invoke(context.Background(), domainCall("detection.plan.create", "metadata-plan", domainPlanInput()))
	if plan.Status != "ok" {
		t.Fatalf("plan: %#v", plan)
	}
	(*catalog)["scenes"].([]any)[0].(map[string]any)["bounds"] = []any{113.0, 37.0, 116.0, 40.0}
	planID := plan.Data.(map[string]any)["plan_id"].(string)
	requireDomainCode(t, registry.Invoke(context.Background(), domainCall("imagery.prepare", "metadata-prepare", map[string]any{"plan_id": planID})), "PLAN_STALE")
}

func TestAIDomainInvalidPlansAndMissingMonths(t *testing.T) {
	_, registry, _ := newDomainFixture(t)
	single := domainPlanInput()
	single["scene_ids"] = []any{"s1"}
	requireDomainCode(t, registry.Invoke(context.Background(), domainCall("detection.plan.create", "single", single)), "SINGLE_PHASE")
	adjacent := domainPlanInput()
	adjacent["pair_policy"] = "adjacent"
	requireDomainCode(t, registry.Invoke(context.Background(), domainCall("detection.plan.create", "missing", adjacent)), "MISSING_PHASES")
	yearAdjacent := domainPlanInput()
	yearAdjacent["pair_policy"] = "adjacent"
	yearAdjacent["start_date"] = map[string]any{"value": "2025", "precision": "year"}
	yearAdjacent["end_date"] = map[string]any{"value": "2025", "precision": "year"}
	requireDomainCode(t, registry.Invoke(context.Background(), domainCall("detection.plan.create", "missing-year", yearAdjacent)), "MISSING_PHASES")
	invalid := domainPlanInput()
	invalid["aoi"] = map[string]any{"type": "Polygon", "coordinates": []any{[]any{[]any{114.0, 38.0}, []any{115.0, 39.0}, []any{114.0, 39.0}, []any{115.0, 38.0}, []any{114.0, 38.0}}}}
	requireDomainCode(t, registry.Invoke(context.Background(), domainCall("detection.plan.create", "invalid", invalid)), "AOI_INVALID")
	arbitrary := domainPlanInput()
	arbitrary["params"] = map[string]any{"shell": "bad"}
	requireDomainCode(t, registry.Invoke(context.Background(), domainCall("detection.plan.create", "params", arbitrary)), "INVALID_INPUT")
}

func TestAIDomainRejectsDuplicateSceneReferences(t *testing.T) {
	_, registry, _ := newDomainFixture(t)
	input := domainPlanInput()
	input["scene_ids"] = []any{"s1", "s1"}
	requireDomainCode(t, registry.Invoke(context.Background(), domainCall("detection.plan.create", "duplicate-scenes", input)), "INVALID_INPUT")
}

func TestAIDomainExplicitPairWithoutCompleteSceneList(t *testing.T) {
	_, registry, _ := newDomainFixture(t)
	for _, scenes := range []any{nil, []any{"s1"}} {
		input := domainPlanInput()
		delete(input, "scene_ids")
		if scenes != nil {
			input["scene_ids"] = scenes
		}
		input["before_id"], input["after_id"] = "s1", "s2"
		result := registry.Invoke(context.Background(), domainCall("detection.plan.create", "explicit-"+fingerprint(scenes), input))
		if result.Status != "ok" {
			t.Fatalf("explicit pair with scenes=%v: %#v", scenes, result)
		}
		plan := result.Data.(map[string]any)
		if plan["before_id"] != "s1" || plan["after_id"] != "s2" {
			t.Fatalf("explicit pair was rewritten: %#v", plan)
		}
	}
}
func TestAIDomainRegionPathsDoNotWidenCounty(t *testing.T) {
	_, registry, _ := newDomainFixture(t)
	for _, input := range []map[string]any{{}, {"query": "河北省保定市"}, {"admin_path": []any{"河北省", "保定市"}}} {
		r := registry.Invoke(context.Background(), domainCall("region.search", "region", input))
		if r.Status != "ok" || r.Data.(map[string]any)["count"] != float64(1) {
			t.Fatalf("region: %#v error=%v", r, r.Error)
		}
	}
	r := registry.Invoke(context.Background(), domainCall("region.search", "county", map[string]any{"query": "河北省保定市雄县"}))
	if r.Status != "ok" || r.Data.(map[string]any)["count"] != float64(0) {
		t.Fatalf("county widened: %#v", r)
	}
}
func TestAIDomainSelectionMosaicsAndNoIntersection(t *testing.T) {
	_, registry, catalog := newDomainFixture(t)
	scenes := (*catalog)["scenes"].([]any)
	scenes[0].(map[string]any)["bounds"] = []any{114.0, 38.0, 115.0, 39.0}
	scenes = append(scenes, map[string]any{"id": "s1b", "date": "2025-01-01", "bounds": []any{115.0, 38.0, 116.0, 39.0}})
	(*catalog)["scenes"] = scenes
	in := map[string]any{"region_id": "130600", "scene_ids": []any{"s1", "s1b", "s2"}, "bounds": []any{114.0, 38.0, 116.0, 39.0}}
	r := registry.Invoke(context.Background(), domainCall("selection.validate", "mosaic", in))
	if r.Status != "ok" {
		t.Fatalf("mosaic failed: %#v", r)
	}
	scenes[1].(map[string]any)["bounds"] = []any{117.0, 38.0, 118.0, 39.0}
	requireDomainCode(t, registry.Invoke(context.Background(), domainCall("selection.validate", "no-intersection", in)), "NO_SPATIAL_INTERSECTION")
}

func TestAIDomainSelectionDerivesBoundsFromAOI(t *testing.T) {
	_, registry, _ := newDomainFixture(t)
	input := map[string]any{"region_id": "130600", "scene_ids": []any{"s1", "s2"}, "aoi": domainAOI()}
	result := registry.Invoke(context.Background(), domainCall("selection.validate", "aoi-only", input))
	if result.Status != "ok" {
		t.Fatalf("AOI-only selection failed: %#v error=%+v", result, result.Error)
	}
}

func TestAIDomainPlanFiltersScenesToDateRange(t *testing.T) {
	_, registry, catalog := newDomainFixture(t)
	scenes := (*catalog)["scenes"].([]any)
	scenes = append(scenes, map[string]any{"id": "s3", "date": "2025-12-01", "date_precision": "month", "available": true, "version": "v3", "bounds": []any{114.0, 38.0, 116.0, 40.0}})
	(*catalog)["scenes"] = scenes
	input := domainPlanInput()
	input["scene_ids"] = []any{"s1", "s2", "s3"}
	result := registry.Invoke(context.Background(), domainCall("detection.plan.create", "date-filter", input))
	if result.Status != "ok" {
		t.Fatalf("date-filtered plan failed: %#v", result)
	}
	plan := result.Data.(map[string]any)
	if got := stringSlice(plan["scene_ids"]); len(got) != 2 || got[0] != "s1" || got[1] != "s2" {
		t.Fatalf("plan retained scenes outside the date range: %#v", got)
	}
	if plan["before_id"] != "s1" || plan["after_id"] != "s2" {
		t.Fatalf("pair was not formed from the date-filtered scenes: %#v", plan)
	}

	input["pair_policy"] = "specified"
	input["before_id"], input["after_id"] = "s1", "s3"
	requireDomainCode(t, registry.Invoke(context.Background(), domainCall("detection.plan.create", "date-filter-pair", input)), "NO_COVERAGE")
}

func TestAIDomainModelListOnlyReturnsReadyEnabledModels(t *testing.T) {
	domain, registry, _ := newDomainFixture(t)
	if err := domain.d.DB.SaveModel(model.ManagedModel{ID: "disabled", Name: "不可用", Status: "ready", Enabled: false}); err != nil {
		t.Fatal(err)
	}
	if err := domain.d.DB.SaveModel(model.ManagedModel{ID: "validating", Name: "校验中", Status: "validating", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	result := registry.Invoke(context.Background(), domainCall("model.list", "ready-models", map[string]any{}))
	if result.Status != "ok" {
		t.Fatalf("model list failed: %#v", result)
	}
	data := result.Data.(map[string]any)
	items := data["items"].([]any)
	if len(items) != 1 || items[0].(map[string]any)["model_id"] != "m" {
		t.Fatalf("model list included unavailable models: %#v", data)
	}
}

func TestAIDomainWorkflowIdempotencyBindingAndTaskState(t *testing.T) {
	domain, registry, _ := newDomainFixture(t)
	plan := registry.Invoke(context.Background(), domainCall("detection.plan.create", "plan", domainPlanInput()))
	if plan.Status != "ok" {
		t.Fatal(plan)
	}
	planID := str(plan.Data.(map[string]any)["plan_id"])
	for _, id := range []string{"im1", "im2"} {
		if err := domain.d.DB.InsertImage(&model.Image{ID: id, Source: "local", Phase: id, FilePath: "derived.tif", CreatedAt: time.Now().UTC()}); err != nil {
			t.Fatal(err)
		}
	}
	saveOp := func(id, pid string) {
		raw, _ := json.Marshal(map[string]any{"operation_id": id, "plan_id": pid, "status": "succeeded", "result": map[string]any{"batch_id": "batch", "image_ids": []string{"im1", "im2"}, "image_scene_ids": [][]string{{"s1"}, {"s2"}}}})
		if err := domain.d.DB.SaveAIDocument(store.AIDocument{ID: id, Kind: "operation", Status: "succeeded", Document: raw}); err != nil {
			t.Fatal(err)
		}
	}
	saveOp("wrong", "another-plan")
	requireDomainCode(t, registry.Invoke(context.Background(), domainCall("detection.workflow.submit", "wrong-submit", map[string]any{"plan_id": planID, "operation_id": "wrong"})), "OPERATION_PLAN_MISMATCH")
	saveOp("op", planID)
	domain.d.Q = queue.New(1)
	t.Cleanup(func() { domain.d.Q.Stop(context.Background()) })
	call := domainCall("detection.workflow.submit", "submit", map[string]any{"plan_id": planID, "operation_id": "op"})
	first := registry.Invoke(context.Background(), call)
	if first.Status != "ok" {
		t.Fatalf("submit: %#v", first)
	}
	second := registry.Invoke(context.Background(), call)
	if second.Status != "ok" || fingerprint(first.Data) != fingerprint(second.Data) {
		t.Fatalf("workflow duplicate: %#v", second)
	}
	wf := first.Data.(map[string]any)
	wfID := str(wf["workflow_id"])
	taskID := stringSlice(wf["task_ids"])[0]
	if code := errorCode(domain.CancelWorkflow(context.Background(), wfID)); code != "WORKFLOW_NOT_CANCELLABLE" {
		t.Fatalf("cancel code: %s", code)
	}
	task, err := domain.d.DB.GetTask(taskID)
	if err != nil || task.Status != model.TaskPending {
		t.Fatalf("cancel changed task: %#v %v", task, err)
	}
	if err := domain.d.DB.UpdateTaskStatus(taskID, model.TaskSucceeded, 100, nil); err != nil {
		t.Fatal(err)
	}
	snapshot, err := domain.GetWorkflow(context.Background(), wfID)
	if err != nil || str(snapshot.(map[string]any)["status"]) != "succeeded" {
		t.Fatalf("workflow status: %#v %v", snapshot, err)
	}
	failedCall := domainCall("detection.workflow.submit", "queue-full", call.Input)
	failed := registry.Invoke(context.Background(), failedCall)
	requireDomainCode(t, failed, "WORKFLOW_PARTIAL")
	replayed := registry.Invoke(context.Background(), failedCall)
	if replayed.Status != failed.Status || replayed.Error == nil || failed.Error == nil || replayed.Error.Code != failed.Error.Code || replayed.Error.Message != failed.Error.Message || replayed.Error.Retryable != failed.Error.Retryable {
		t.Fatalf("failed workflow duplicate was not replayed faithfully: first=%#v replay=%#v", failed, replayed)
	}
}

func TestAIDomainFailedIdempotencyPersistsResultWithoutResource(t *testing.T) {
	_, registry, _ := newDomainFixture(t)
	// This fails before a plan is persisted, so the request record itself must
	// retain the public error for a process-safe duplicate replay.
	input := domainPlanInput()
	input["scene_ids"] = []any{"s1"}
	call := domainCall("detection.plan.create", "failed-plan", input)
	first := registry.Invoke(context.Background(), call)
	requireDomainCode(t, first, "SINGLE_PHASE")
	second := registry.Invoke(context.Background(), call)
	if second.Status != first.Status || second.Error == nil || first.Error == nil || second.Error.Code != first.Error.Code || second.Error.Message != first.Error.Message {
		t.Fatalf("failed plan duplicate was not replayed faithfully: first=%#v replay=%#v", first, second)
	}
}

func TestAIDomainAdjacentWorkflowSubmitsEveryPair(t *testing.T) {
	domain, registry, catalog := newDomainFixture(t)
	// Adjacent pairing uses the ordered prepared batch. Omit a bounded date
	// range here so the fixture does not need a complete month-by-month catalog.
	scenes := (*catalog)["scenes"].([]any)
	scenes = append(scenes, map[string]any{"id": "s3", "date": "2025-12-01", "date_precision": "month", "available": true, "version": "v3", "bounds": []any{114.0, 38.0, 116.0, 40.0}})
	(*catalog)["scenes"] = scenes
	input := domainPlanInput()
	delete(input, "start_date")
	delete(input, "end_date")
	input["scene_ids"] = []any{"s1", "s2", "s3"}
	input["pair_policy"] = "adjacent"
	plan := registry.Invoke(context.Background(), domainCall("detection.plan.create", "adjacent-plan", input))
	if plan.Status != "ok" {
		t.Fatalf("adjacent plan: %#v error=%+v", plan, plan.Error)
	}
	planID := str(plan.Data.(map[string]any)["plan_id"])
	for _, id := range []string{"im1", "im2", "im3"} {
		if err := domain.d.DB.InsertImage(&model.Image{ID: id, Source: "local", Phase: id, FilePath: "derived.tif", CreatedAt: time.Now().UTC()}); err != nil {
			t.Fatal(err)
		}
	}
	raw, _ := json.Marshal(map[string]any{
		"operation_id": "adjacent-op",
		"plan_id":      planID,
		"status":       "succeeded",
		"result": map[string]any{
			"batch_id":        "batch-adjacent",
			"image_ids":       []string{"im1", "im2", "im3"},
			"image_scene_ids": [][]string{{"s1"}, {"s2"}, {"s3"}},
		},
	})
	if err := domain.d.DB.SaveAIDocument(store.AIDocument{ID: "adjacent-op", Kind: "operation", Status: "succeeded", Document: raw}); err != nil {
		t.Fatal(err)
	}
	domain.d.Q = queue.New(4)
	t.Cleanup(func() { domain.d.Q.Stop(context.Background()) })
	result := registry.Invoke(context.Background(), domainCall("detection.workflow.submit", "adjacent-submit", map[string]any{"plan_id": planID, "operation_id": "adjacent-op"}))
	if result.Status != "ok" {
		t.Fatalf("adjacent submit: %#v", result)
	}
	taskIDs := stringSlice(result.Data.(map[string]any)["task_ids"])
	if len(taskIDs) != 2 {
		t.Fatalf("adjacent task count = %d, want 2: %#v", len(taskIDs), taskIDs)
	}
	want := map[string]bool{"im1|im2": true, "im2|im3": true}
	for _, taskID := range taskIDs {
		task, err := domain.d.DB.GetTask(taskID)
		if err != nil {
			t.Fatalf("task %s: %v", taskID, err)
		}
		delete(want, task.BeforeID+"|"+task.AfterID)
	}
	if len(want) != 0 {
		t.Fatalf("missing adjacent pairs: %#v", want)
	}
}

func TestAIDomainDatePrecisionExpansion(t *testing.T) {
	for _, row := range []struct {
		value, precision, want string
		end                    bool
	}{{"2025-07", "month", "2025-07-31", true}, {"2024-02", "month", "2024-02-29", true}, {"2025", "year", "2025-01-01", false}} {
		if got := dateSlotBoundary(map[string]any{"value": row.value, "precision": row.precision}, row.end, ""); got != row.want {
			t.Fatalf("%s != %s", got, row.want)
		}
	}
}
