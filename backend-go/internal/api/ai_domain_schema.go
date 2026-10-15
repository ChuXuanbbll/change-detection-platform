package api

// Every domain tool owns an explicit DTO schema. Only legacy geographic and
// result metadata remain extensible; resource references and workflow records
// are closed objects so contract drift is detected before reaching the LLM.
func aiOutputSchema(tool string) map[string]any {
	s := map[string]any{"type": "string"}
	n := map[string]any{"type": "number"}
	i := map[string]any{"type": "integer"}
	b := map[string]any{"type": "boolean"}
	arr := func(item map[string]any) map[string]any { return map[string]any{"type": "array", "items": item} }
	obj := func(fields map[string]any, required ...string) map[string]any {
		value := map[string]any{"type": "object", "additionalProperties": false, "properties": fields}
		if len(required) > 0 {
			value["required"] = stringAny(required)
		}
		return value
	}
	nullable := func(value map[string]any) map[string]any {
		return map[string]any{"anyOf": []any{value, map[string]any{"type": "null"}}}
	}
	metadata := map[string]any{"type": "object", "additionalProperties": true}
	bounds := map[string]any{"type": "array", "minItems": 4, "maxItems": 4, "items": n}
	geometry := obj(map[string]any{"type": s, "coordinates": map[string]any{"type": "array"}}, "type", "coordinates")
	date := obj(map[string]any{"value": s, "precision": map[string]any{"type": "string", "enum": []any{"day", "month", "year"}}}, "value", "precision")
	scene := obj(map[string]any{"id": s, "region_id": s, "region": s, "date": s, "date_precision": s, "available": b, "error": s, "version": s, "bounds": nullable(bounds), "width": i, "height": i, "crs": s, "resolution": arr(n), "resolution_m": n, "source_priority": i}, "id", "date")
	scene["additionalProperties"] = true
	phase := obj(map[string]any{"id": s, "region_id": s, "month": s, "available": b, "version": s, "bounds": nullable(bounds), "scenes": arr(scene)}, "id", "region_id", "month")
	task := obj(map[string]any{"task_id": s, "before_image_id": s, "after_image_id": s, "algorithm": s, "status": s, "stage": s, "progress": i, "error": s, "created_at": s}, "task_id", "status", "progress")
	operationResult := obj(map[string]any{"operation_id": s, "status": s, "image_count": i, "image_ids": arr(s), "image_scene_ids": arr(arr(s)), "source_scene_ids": nullable(arr(s)), "batch_id": s}, "operation_id", "status", "image_ids", "batch_id")
	operation := obj(map[string]any{"operation_id": s, "kind": s, "plan_id": s, "status": s, "created_at": s, "error": s, "error_code": s, "result": operationResult}, "operation_id", "plan_id", "status")
	var result map[string]any
	switch tool {
	case "workspace.context":
		result = obj(map[string]any{"revision": s, "page": s, "region_id": s, "region_name": s, "phase_ids": arr(s), "phases": arr(obj(map[string]any{"id": s, "month": s, "scene_ids": arr(s)}, "id", "month", "scene_ids")), "active_phase": s, "bounds": nullable(bounds), "aoi": nullable(geometry), "task_id": s, "result_id": s, "task": obj(map[string]any{"status": s, "progress": i, "message": s})})
	case "region.search":
		region := obj(map[string]any{"region_id": s, "name": s, "level": s, "admin_path": arr(s), "aoi_supported": b, "bounds": nullable(bounds), "geometry": nullable(geometry)}, "region_id", "name", "admin_path", "aoi_supported")
		result = obj(map[string]any{"matches": arr(region), "count": i}, "matches", "count")
	case "imagery.catalog":
		result = obj(map[string]any{"images": arr(scene), "bounds": bounds, "boundary": nullable(geometry), "boundary_mode": s, "warning": nullable(s)}, "images", "bounds")
	case "imagery.scenes":
		result = obj(map[string]any{"region_id": s, "name": nullable(s), "scenes": arr(scene), "phases": arr(phase)}, "region_id", "scenes", "phases")
	case "imagery.phase":
		result = phase
	case "model.list":
		model := obj(map[string]any{"model_id": s, "name": s, "description": s, "status": s, "enabled": b, "sha256_prefix": s, "mapping": nullable(metadata)}, "model_id", "status", "enabled")
		result = obj(map[string]any{"items": arr(model), "count": i}, "items", "count")
	case "image.list":
		image := obj(map[string]any{"image_id": s, "source": s, "phase": s, "capture_date": s, "date_precision": s, "bounds": bounds, "width": i, "height": i, "crs": s, "metadata": nullable(metadata)}, "image_id", "bounds", "capture_date", "date_precision")
		result = obj(map[string]any{"items": arr(image), "count": i}, "items", "count")
	case "task.search":
		result = obj(map[string]any{"items": arr(task), "count": i}, "items", "count")
	case "task.status":
		result = task
	case "result.summary":
		result = obj(map[string]any{"result_id": s, "task_id": s, "stats": metadata, "created_at": s, "available_formats": arr(s)}, "result_id", "task_id", "stats", "available_formats")
	case "result.patches.query":
		patch := obj(map[string]any{"id": nullable(map[string]any{"anyOf": []any{s, n}}), "geometry": nullable(geometry), "properties": metadata}, "geometry", "properties")
		result = obj(map[string]any{"task_id": s, "items": arr(patch), "count": i, "limit": i}, "task_id", "items", "count", "limit")
	case "selection.validate":
		result = obj(map[string]any{"region_id": s, "phases": metadata, "phase_count": i, "intersection_bounds": bounds}, "region_id", "phases", "phase_count", "intersection_bounds")
	case "detection.plan.create":
		result = obj(map[string]any{"plan_id": s, "region_id": s, "scene_ids": arr(s), "before_id": s, "after_id": s, "pair_policy": s, "aoi": nullable(geometry), "aoi_source": s, "bounds": nullable(bounds), "model_id": s, "config_revision": i, "start_date": nullable(date), "end_date": nullable(date), "catalog_version": s, "phase_versions": metadata, "missing_months": arr(s), "actual_dates": arr(date), "status": s, "created_at": s}, "plan_id", "region_id", "scene_ids", "pair_policy", "model_id", "config_revision", "status")
	case "imagery.prepare", "operation.status":
		result = operation
	case "detection.workflow.submit":
		result = obj(map[string]any{"workflow_id": s, "plan_id": s, "status": s, "task_ids": arr(s), "created_at": s, "error_code": s, "error": s, "tasks": arr(task), "task_summary": metadata}, "workflow_id", "plan_id", "status", "task_ids")
	case "result.export.prepare":
		result = obj(map[string]any{"result_id": s, "format": s, "download_ref": s}, "result_id", "format", "download_ref")
	default:
		result = obj(map[string]any{})
	}
	result["$schema"] = "https://json-schema.org/draft/2020-12/schema"
	return result
}
