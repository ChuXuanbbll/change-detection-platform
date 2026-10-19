package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"time"

	"cdp/backend-go/internal/client"
	"cdp/backend-go/internal/model"
	"cdp/backend-go/internal/progress"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// ServiceError preserves the established REST error envelope. AI callers map
// this to a separate stable code/message; raw upstream errors stay local.
type ServiceError struct {
	Status int
	Body   map[string]any
}

func (e *ServiceError) Error() string { return fmt.Sprint(e.Body["error"]) }
func serviceErr(status int, code, msg string) error {
	return &ServiceError{Status: status, Body: map[string]any{"code": code, "error": msg}}
}

type LocalFetchResult struct {
	Items   []model.Image `json:"items"`
	Count   int           `json:"count"`
	BatchID string        `json:"batch_id"`
	Message string        `json:"message,omitempty"`
}

func LocalFetchHandler(d *Deps) gin.HandlerFunc {
	return func(c *gin.Context) {
		var req localFetchReq
		if e := c.ShouldBindJSON(&req); e != nil {
			c.JSON(400, gin.H{"error": "入参 JSON 解析失败", "code": "INVALID_JSON", "detail": e.Error()})
			return
		}
		out, e := LocalFetchService(c.Request.Context(), d, req)
		if e != nil {
			var se *ServiceError
			if errors.As(e, &se) {
				c.JSON(se.Status, se.Body)
			} else {
				c.JSON(500, gin.H{"error": "影像准备失败", "code": "OPERATION_FAILED"})
			}
			return
		}
		status := http.StatusCreated
		if out.Count == 0 {
			status = 200
		}
		c.JSON(status, out)
	}
}

// LocalFetchService is shared by the REST handler and AI workflow. It freezes
// the existing management snapshot and writes only its own derived batch.
func LocalFetchService(ctx context.Context, d *Deps, req localFetchReq) (out LocalFetchResult, err error) {
	success := false
	if req.ProgressID != "" {
		if _, e := uuid.Parse(req.ProgressID); e != nil {
			return out, serviceErr(400, "INVALID_INPUT", "progress_id 必须为 UUID")
		}
		if !d.preparations.start(req.ProgressID) {
			return out, serviceErr(409, "PROGRESS_ID_CONFLICT", "progress_id 已使用")
		}
		defer func() { d.preparations.finish(req.ProgressID, success) }()
	}
	var selected []string
	if req.SceneIDs != nil {
		if json.Unmarshal(req.SceneIDs, &selected) != nil || len(selected) == 0 || req.RegionID == "" {
			return out, serviceErr(400, "EMPTY_SCENE_SELECTION", "请选择行政区及至少一张影像")
		}
	}
	if e := validateBounds(req.Bounds); e != nil {
		return out, serviceErr(400, "BOUNDS_INVALID", e.Error())
	}
	if req.AdminCode != "" {
		if d.DB == nil || len(selected) == 0 {
			return out, serviceErr(400, "ADMIN_BOUNDARY_INVALID", "县界检测需要当前来源及已选影像")
		}
		feature, e := d.DB.AdministrativeRegion(req.AdminCode, req.AdminVersion)
		if e != nil || feature.Properties.Level != "county" || feature.Properties.SourceID != req.RegionID {
			return out, serviceErr(400, "ADMIN_BOUNDARY_INVALID", "县级行政区或边界版本与当前影像来源不匹配")
		}
		if len(req.AOI) > 0 && string(req.AOI) != "null" {
			var supplied, known any
			if json.Unmarshal(req.AOI, &supplied) != nil || json.Unmarshal(feature.Geometry, &known) != nil || !reflect.DeepEqual(supplied, known) {
				return out, serviceErr(400, "ADMIN_BOUNDARY_MISMATCH", "检测轮廓与入库县级边界不一致")
			}
		}
		req.AOI = feature.Geometry
		req.AdminVersion = feature.Properties.Version
		req.Bounds = feature.Properties.Bounds
	}
	if req.MaxImages < 0 || req.MaxImages > maxWaybackImages {
		return out, serviceErr(400, "INVALID_LIMIT", "max_images 必须为 0~20，省略或 0 表示全部时相")
	}
	if req.StartDate == "" {
		req.StartDate = "0001-01-01"
	}
	if d.Py == nil {
		return out, serviceErr(503, "UPSTREAM_UNAVAILABLE", "影像服务不可用")
	}
	if req.EndDate == "" {
		req.EndDate = "9999-12-31"
	}
	if req.Zoom <= 0 {
		req.Zoom = 18
	}
	if req.Buffer <= 0 {
		req.Buffer = 500
	}
	out = LocalFetchResult{BatchID: uuid.NewString(), Items: []model.Image{}}
	if d.Management != nil {
		d.Management.mu.RLock()
		defer d.Management.mu.RUnlock()
		if _, e := d.Management.freeze(ctx, out.BatchID, req.ModelID, req.Revision, req.Params); e != nil {
			return out, serviceErr(409, "CONFIG_STALE", e.Error())
		}
	}
	outputDir := filepath.Join(d.Cfg.DataDir, "shared", out.BatchID)
	if e := safeUnderDataRoot(d.Cfg.DataDir, outputDir); e != nil {
		return out, serviceErr(400, "PATH_ESCAPE", "computed path escapes data dir")
	}
	if e := os.MkdirAll(outputDir, 0755); e != nil {
		return out, serviceErr(500, "MKDIR_FAILED", "mkdir shared failed")
	}
	defer func() {
		if !success {
			if e := os.RemoveAll(outputDir); e != nil {
				log.Printf("[images.local] cleanup batch failed: %v", e)
			}
		}
	}()
	stop := func() {}
	if req.ProgressID != "" {
		stop = progress.Watch(outputDir, func(s progress.Snapshot) { d.preparations.update(req.ProgressID, s) })
		defer stop()
	}
	response, e := d.Py.LocalFetch(ctx, &model.LocalFetchReq{AdminCode: req.AdminCode, AdminVersion: req.AdminVersion, AOI: req.AOI, Bounds: req.Bounds, StartDate: req.StartDate, EndDate: req.EndDate, MaxImages: req.MaxImages, Zoom: req.Zoom, Buffer: req.Buffer, OutputDir: outputDir, RegionID: req.RegionID, SceneIDs: selected})
	if e != nil {
		status := 502
		var up *client.UpstreamError
		if errors.As(e, &up) && (up.StatusCode == 400 || up.StatusCode == 422) {
			status = up.StatusCode
		}
		return out, serviceErr(status, "LOCAL_CROP_FAILED", "本地影像裁切失败: "+e.Error())
	}
	stop()
	if len(req.AOI) > 0 && string(req.AOI) != "null" {
		var requested, confirmed any
		if response == nil || json.Unmarshal(req.AOI, &requested) != nil || json.Unmarshal(response.AOI, &confirmed) != nil || !reflect.DeepEqual(requested, confirmed) {
			return out, serviceErr(502, "AOI_NOT_CONFIRMED", "裁切服务未确认多边形范围，请重启 Go 和 Python 服务后重新检测")
		}
	}
	if response == nil || len(response.Images) == 0 {
		success = true
		out.Message = "未拉取到任何历史影像（可能日期范围无数据）"
		return out, nil
	}
	if req.ProgressID != "" {
		d.preparations.update(req.ProgressID, progress.Snapshot{Stage: "saving_images", Progress: 99})
	}
	now := time.Now().UTC()
	for _, it := range response.Images {
		if safeUnderDataRoot(outputDir, it.Path) != nil {
			continue
		}
		id := uuid.NewString()
		precision := it.DatePrecision
		if precision != "day" && precision != "year" {
			precision = "month"
		}
		meta := map[string]any{"batch_id": out.BatchID, "source_file": it.SourceFile, "date_precision": precision, "region_id": req.RegionID, "bounds": it.Bounds}
		if req.AdminCode != "" {
			meta["admin_code"] = req.AdminCode
			meta["admin_version"] = req.AdminVersion
		}
		if len(req.AOI) > 0 && string(req.AOI) != "null" {
			meta["aoi"] = req.AOI
		}
		if len(it.SourceFiles) > 0 {
			meta["source_files"] = it.SourceFiles
		}
		if len(it.SceneIDs) > 0 {
			meta["scene_ids"] = it.SceneIDs
		}
		raw, _ := json.Marshal(meta)
		bs := it.Bounds
		out.Items = append(out.Items, model.Image{ID: id, Source: "local", CaptureDate: parseCaptureDate(it.Date), FilePath: it.Path, ThumbPath: filepath.Join(outputDir, id+".thumb.png"), CRS: "EPSG:4326", BoundsWest: bs[0], BoundsSouth: bs[1], BoundsEast: bs[2], BoundsNorth: bs[3], Width: it.Width, Height: it.Height, Bands: 3, Dtype: "uint8", MetaJSON: string(raw), CreatedAt: now})
	}
	if len(out.Items) == 0 {
		out.Message = "拉取的影像路径全部被安全校验拒绝"
		return out, nil
	}
	if e := d.DB.InsertImages(out.Items); e != nil {
		return out, serviceErr(500, "DB_INSERT_FAILED", "影像元数据保存失败")
	}
	out.Count = len(out.Items)
	success = true
	return out, nil
}

func CreateTaskHandler(d *Deps) gin.HandlerFunc {
	return func(c *gin.Context) {
		var req model.CreateTaskReq
		if e := c.ShouldBindJSON(&req); e != nil {
			c.JSON(400, gin.H{"error": "入参 JSON 解析失败", "code": "INVALID_JSON", "detail": e.Error()})
			return
		}
		t, e := CreateTaskService(c.Request.Context(), d, req)
		if e != nil {
			var se *ServiceError
			if errors.As(e, &se) {
				c.JSON(se.Status, se.Body)
			} else {
				c.JSON(500, gin.H{"error": "任务提交失败", "code": "OPERATION_FAILED"})
			}
			return
		}
		c.JSON(http.StatusAccepted, t)
	}
}

func CreateTaskService(ctx context.Context, d *Deps, req model.CreateTaskReq) (*model.Task, error) {
	if d.Management != nil {
		d.Management.mu.RLock()
		defer d.Management.mu.RUnlock()
	}
	if strings.TrimSpace(req.BeforeID) == "" {
		return nil, serviceErr(400, "MISSING_BEFORE_ID", "before_id 必填")
	}
	if strings.TrimSpace(req.AfterID) == "" {
		return nil, serviceErr(400, "MISSING_AFTER_ID", "after_id 必填")
	}
	if strings.TrimSpace(req.Algorithm) == "" {
		req.Algorithm = "hebei_onnx"
	}
	if req.Algorithm == "hebei_onnx" {
		if p, ok := req.Params["provider"]; ok && p != "cuda" {
			return nil, serviceErr(400, "GPU_REQUIRED", "hebei_onnx 模型检测必须使用 GPU（provider=cuda）")
		}
		if req.Params == nil {
			req.Params = map[string]any{}
		}
		req.Params["provider"] = "cuda"
	}
	if _, e := d.DB.GetImage(req.BeforeID); e != nil {
		return nil, serviceErr(400, "BEFORE_NOT_FOUND", "before_id 对应的影像不存在")
	}
	if _, e := d.DB.GetImage(req.AfterID); e != nil {
		return nil, serviceErr(400, "AFTER_NOT_FOUND", "after_id 对应的影像不存在")
	}
	if d.Management != nil {
		b, e := d.DB.Batch(req.BatchID)
		if e != nil {
			return nil, serviceErr(400, "BATCH_NOT_FOUND", "批次快照不存在")
		}
		for _, id := range []string{req.BeforeID, req.AfterID} {
			im, e := d.DB.GetImage(id)
			if e != nil {
				return nil, serviceErr(400, "IMAGE_NOT_FOUND", "影像不存在")
			}
			var meta map[string]any
			if json.Unmarshal([]byte(im.MetaJSON), &meta) != nil || meta["batch_id"] != req.BatchID {
				return nil, serviceErr(400, "BATCH_MISMATCH", "影像不属于该批次")
			}
		}
		req.Algorithm = "hebei_onnx"
		req.Params = b.Params
	}
	raw := ""
	if req.Params != nil {
		b, _ := json.Marshal(req.Params)
		raw = string(b)
	}
	t := &model.Task{ID: uuid.NewString(), BeforeID: req.BeforeID, AfterID: req.AfterID, Algorithm: req.Algorithm, ParamsJSON: raw, Status: model.TaskPending, Stage: "queued", CreatedAt: time.Now().UTC()}
	if d.Q == nil {
		return nil, serviceErr(503, "QUEUE_FULL", "检测队列不可用")
	}
	ok, e := d.Q.Submit(t.ID, func() error { return d.DB.InsertTask(t) })
	if e != nil {
		return nil, serviceErr(500, "DB_INSERT_FAILED", "db insert task failed")
	}
	if !ok {
		return nil, serviceErr(503, "QUEUE_FULL", "检测队列已满或正在关闭")
	}
	return t, nil
}
