package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"time"

	"cdp/backend-go/internal/client"
	"cdp/backend-go/internal/model"
	"cdp/backend-go/internal/progress"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// Maximum explicit compatibility limit; omitted/zero requests every local phase.
const maxWaybackImages = 20

// localFetchReq POST /api/images/local 入参
type localFetchReq struct {
	AOI        json.RawMessage `json:"aoi,omitempty"`
	ProgressID string          `json:"progress_id"`
	ModelID    string          `json:"model_id"`
	Revision   int64           `json:"config_revision"`
	Params     map[string]any  `json:"params"`
	Bounds     [4]float64      `json:"bounds" binding:"required"`
	StartDate  string          `json:"start_date"`
	EndDate    string          `json:"end_date"`
	MaxImages  int             `json:"max_images"`
	Zoom       int             `json:"zoom"`
	Buffer     int             `json:"buffer"`
	RegionID   string          `json:"region_id"`
	SceneIDs   json.RawMessage `json:"scene_ids"`
}

// safeUnderDataRoot 检查 child 是否在 dataDir 之下（防止 ../ 路径逃逸）。
func safeUnderDataRoot(dataDir, child string) error {
	absRoot, err := filepath.Abs(dataDir)
	if err != nil {
		return err
	}
	absChild, err := filepath.Abs(child)
	if err != nil {
		return err
	}
	rel, err := filepath.Rel(absRoot, absChild)
	if err != nil {
		return err
	}
	if strings.HasPrefix(rel, "..") || strings.Contains(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("path escapes data dir: %s not under %s", child, dataDir)
	}
	return nil
}

// parseCaptureDate 解析多种日期格式；失败时返回零值。
func parseCaptureDate(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	formats := []string{
		"2006-01-02",
		time.RFC3339,
		"2006/01/02",
		"20060102",
	}
	for _, f := range formats {
		if t, err := time.Parse(f, s); err == nil {
			return t
		}
	}
	return time.Time{}
}

// LocalFetchHandler POST /api/images/local
// 转发到 Python /py/local/fetch，并把返回的影像写入 SQLite。
func LocalFetchHandler(d *Deps) gin.HandlerFunc {
	return func(c *gin.Context) {
		var req localFetchReq
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{
				"error":  "入参 JSON 解析失败",
				"code":   "INVALID_JSON",
				"detail": err.Error(),
			})
			return
		}
		success := false
		if req.ProgressID != "" {
			if _, err := uuid.Parse(req.ProgressID); err != nil {
				c.JSON(400, gin.H{"error": "progress_id 必须为 UUID"})
				return
			}
			if !d.preparations.start(req.ProgressID) {
				c.JSON(409, gin.H{"error": "progress_id 已使用", "code": "PROGRESS_ID_CONFLICT"})
				return
			}
			defer func() { d.preparations.finish(req.ProgressID, success) }()
		}
		// bounds 入参校验
		var selectedIDs []string
		if req.SceneIDs != nil {
			if err := json.Unmarshal(req.SceneIDs, &selectedIDs); err != nil || len(selectedIDs) == 0 || req.RegionID == "" {
				c.JSON(http.StatusBadRequest, gin.H{"error": "请选择行政区及至少一张影像", "code": "EMPTY_SCENE_SELECTION"})
				return
			}
		}
		if err := validateBounds(req.Bounds); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{
				"error":    err.Error(),
				"code":     "BOUNDS_INVALID",
				"field":    "bounds",
				"received": req.Bounds,
			})
			return
		}
		// 默认值
		if req.MaxImages < 0 || req.MaxImages > maxWaybackImages {
			c.JSON(http.StatusBadRequest, gin.H{"error": "max_images 必须为 0~20，省略或 0 表示全部时相"})
			return
		}
		if req.StartDate == "" {
			req.StartDate = "0001-01-01"
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

		batchID := uuid.NewString()
		if d.Management != nil {
			d.Management.mu.RLock()
			defer d.Management.mu.RUnlock()
			if _, err := d.Management.freeze(c.Request.Context(), batchID, req.ModelID, req.Revision, req.Params); err != nil {
				c.JSON(409, gin.H{"error": err.Error()})
				return
			}
		}
		outputDir := filepath.Join(d.Cfg.DataDir, "shared", batchID)
		if err := safeUnderDataRoot(d.Cfg.DataDir, outputDir); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{
				"error": "computed path escapes data dir",
				"code":  "PATH_ESCAPE",
				"path":  outputDir,
			})
			return
		}
		if err := os.MkdirAll(outputDir, 0o755); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{
				"error": "mkdir shared failed: " + err.Error(),
				"code":  "MKDIR_FAILED",
			})
			return
		}
		defer func() {
			if !success {
				if err := os.RemoveAll(outputDir); err != nil {
					log.Printf("[images.local] cleanup failed batch: %v", err)
				}
			}
		}()

		stopProgress := func() {}
		if req.ProgressID != "" {
			stopProgress = progress.Watch(outputDir, func(s progress.Snapshot) { d.preparations.update(req.ProgressID, s) })
			defer stopProgress()
		}
		pyReq := &model.LocalFetchReq{
			AOI:       req.AOI,
			Bounds:    req.Bounds,
			StartDate: req.StartDate,
			EndDate:   req.EndDate,
			MaxImages: req.MaxImages,
			Zoom:      req.Zoom,
			Buffer:    req.Buffer,
			OutputDir: outputDir,
			RegionID:  req.RegionID,
			SceneIDs:  selectedIDs,
		}
		resp, err := d.Py.LocalFetch(c.Request.Context(), pyReq)
		if err != nil {
			log.Printf("[images.local] py fetch failed: %v", err)
			status := http.StatusBadGateway
			var upstream *client.UpstreamError
			if errors.As(err, &upstream) && (upstream.StatusCode == 400 || upstream.StatusCode == 422) {
				status = upstream.StatusCode
			}
			c.JSON(status, gin.H{
				"error":  "本地影像裁切失败: " + err.Error(),
				"code":   "LOCAL_CROP_FAILED",
				"detail": err.Error(),
			})
			return
		}
		stopProgress()
		if len(req.AOI) > 0 && string(req.AOI) != "null" {
			var requested, confirmed any
			if resp == nil || json.Unmarshal(req.AOI, &requested) != nil || json.Unmarshal(resp.AOI, &confirmed) != nil || !reflect.DeepEqual(requested, confirmed) {
				c.JSON(http.StatusBadGateway, gin.H{"error": "裁切服务未确认多边形范围，请重启 Go 和 Python 服务后重新检测", "code": "AOI_NOT_CONFIRMED"})
				return
			}
		}
		if resp == nil || len(resp.Images) == 0 {
			success = true
			c.JSON(http.StatusOK, gin.H{
				"items":    []model.Image{},
				"count":    0,
				"batch_id": batchID,
				"message":  "未拉取到任何历史影像（可能日期范围无数据）",
			})
			return
		}

		if req.ProgressID != "" {
			d.preparations.update(req.ProgressID, progress.Snapshot{Stage: "saving_images", Progress: 99})
		}
		items := make([]model.Image, 0, len(resp.Images))
		now := time.Now().UTC()
		for _, it := range resp.Images {
			if err := safeUnderDataRoot(outputDir, it.Path); err != nil {
				log.Printf("[images.local] reject path not under output_dir: %s", it.Path)
				continue
			}
			id := uuid.NewString()
			thumb := filepath.Join(outputDir, id+".thumb.png")

			bounds := it.Bounds
			var bs [4]float64
			if len(bounds) >= 4 {
				bs = [4]float64{bounds[0], bounds[1], bounds[2], bounds[3]}
			}
			width := 0
			height := 0
			if it.Width > 0 {
				width = it.Width
			}
			if it.Height > 0 {
				height = it.Height
			}
			precision := it.DatePrecision
			if precision != "year" && precision != "day" {
				precision = "month"
			}
			meta := map[string]any{"batch_id": batchID, "source_file": it.SourceFile, "date_precision": precision}
			meta["region_id"] = req.RegionID
			meta["bounds"] = bs
			if len(req.AOI) > 0 && string(req.AOI) != "null" {
				meta["aoi"] = req.AOI
			}
			if len(it.SourceFiles) > 0 {
				meta["source_files"] = it.SourceFiles
			}
			if len(it.SceneIDs) > 0 {
				meta["scene_ids"] = it.SceneIDs
			}
			metadata, _ := json.Marshal(meta)
			items = append(items, model.Image{
				ID:          id,
				Source:      "local",
				Phase:       "",
				CaptureDate: parseCaptureDate(it.Date),
				FilePath:    it.Path,
				ThumbPath:   thumb,
				CRS:         "EPSG:4326",
				BoundsWest:  bs[0],
				BoundsSouth: bs[1],
				BoundsEast:  bs[2],
				BoundsNorth: bs[3],
				Width:       width,
				Height:      height,
				Bands:       3,
				Dtype:       "uint8",
				MetaJSON:    string(metadata),
				CreatedAt:   now,
			})
		}
		if len(items) == 0 {
			c.JSON(http.StatusOK, gin.H{
				"items":    []model.Image{},
				"count":    0,
				"batch_id": batchID,
				"message":  "拉取的影像路径全部被安全校验拒绝",
			})
			return
		}
		if err := d.DB.InsertImages(items); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "影像元数据保存失败", "code": "DB_INSERT_FAILED"})
			return
		}

		success = true
		c.JSON(http.StatusCreated, gin.H{
			"items":    items,
			"count":    len(items),
			"batch_id": batchID,
		})
	}
}

// ListImagesHandler GET /api/images
func ListImagesHandler(d *Deps) gin.HandlerFunc {
	return func(c *gin.Context) {
		source := c.Query("source")
		phase := c.Query("phase")
		limit := 100
		if v := c.Query("limit"); v != "" {
			var n int
			_, perr := fmt.Sscanf(v, "%d", &n)
			if perr == nil && n > 0 && n <= 500 {
				limit = n
			} else if perr != nil || n <= 0 || n > 500 {
				c.JSON(http.StatusBadRequest, gin.H{
					"error":    "limit 必须是 1~500 的整数",
					"code":     "INVALID_LIMIT",
					"received": v,
				})
				return
			}
		}
		imgs, err := d.DB.ListImages(limit, source, phase)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{
				"error": "list images failed: " + err.Error(),
				"code":  "DB_LIST_FAILED",
			})
			return
		}
		if imgs == nil {
			imgs = []model.Image{}
		}
		c.JSON(http.StatusOK, gin.H{"items": imgs, "count": len(imgs)})
	}
}

var thumbnailMutex sync.Mutex

// ImagePreviewHandler GET /api/images/:id/preview
// 严格真实：仅返回已生成的有效缩略图；不存在则返 404。
func ImagePreviewHandler(d *Deps) gin.HandlerFunc {
	return func(c *gin.Context) {
		id := c.Param("id")
		img, err := d.DB.GetImage(id)
		if err != nil {
			c.JSON(http.StatusNotFound, gin.H{
				"error": "image not found",
				"code":  "IMAGE_NOT_FOUND",
				"id":    id,
			})
			return
		}
		c.Header("X-Image-Id", id)

		thumb := img.ThumbPath
		if thumb != "" && fileIsNonEmpty(thumb) {
			c.Header("Content-Type", "image/png")
			c.File(thumb)
			return
		}

		if img.FilePath != "" && fileExists(img.FilePath) {
			if thumb == "" {
				thumb = filepath.Join(filepath.Dir(img.FilePath), "thumb.png")
			}
			thumbnailMutex.Lock()
			if !fileIsNonEmpty(thumb) {
				err = d.Py.GenerateThumbnail(c.Request.Context(), img.FilePath, thumb, 256)
			}
			thumbnailMutex.Unlock()
			if err == nil && fileIsNonEmpty(thumb) {
				c.Header("Content-Type", "image/png")
				c.File(thumb)
				return
			}

		}
		c.JSON(http.StatusNotFound, gin.H{
			"error":    "缩略图尚未生成，请稍后重试",
			"code":     "THUMBNAIL_NOT_READY",
			"image_id": id,
		})
	}
}

// DeleteImageHandler DELETE /api/images/:id
func DeleteImageHandler(d *Deps) gin.HandlerFunc {
	return func(c *gin.Context) {
		id := strings.TrimSpace(c.Param("id"))
		if id == "" {
			c.JSON(http.StatusBadRequest, gin.H{
				"error": "missing id",
				"code":  "MISSING_ID",
			})
			return
		}

		img, err := d.DB.GetImage(id)
		if err != nil || img == nil {
			c.JSON(http.StatusNotFound, gin.H{
				"error": "image not found",
				"code":  "IMAGE_NOT_FOUND",
				"id":    id,
			})
			return
		}

		if img.FilePath != "" {
			if rmErr := os.Remove(img.FilePath); rmErr != nil && !os.IsNotExist(rmErr) {
				log.Printf("[images.delete] remove file %s: %v", img.FilePath, rmErr)
			}
		}
		if img.ThumbPath != "" {
			if rmErr := os.Remove(img.ThumbPath); rmErr != nil && !os.IsNotExist(rmErr) {
				log.Printf("[images.delete] remove thumb %s: %v", img.ThumbPath, rmErr)
			}
		}

		n, err := d.DB.DeleteImage(id)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{
				"error": "db delete failed: " + err.Error(),
				"code":  "DB_DELETE_FAILED",
			})
			return
		}
		if n == 0 {
			// 记录已被并发删除或不存在
			c.JSON(http.StatusNotFound, gin.H{
				"error": "image not found",
				"code":  "IMAGE_NOT_FOUND",
				"id":    id,
			})
			return
		}
		c.JSON(http.StatusOK, gin.H{"ok": true, "id": id})
	}
}

// validateBounds 校验 [w, s, e, n] 经纬度范围，避免 Go 端 panic 与上游 400。
func validateBounds(b [4]float64) error {
	if b[0] < -180 || b[0] > 180 || b[2] < -180 || b[2] > 180 {
		return fmt.Errorf("bounds 经度必须在 [-180, 180]，收到 west=%v east=%v", b[0], b[2])
	}
	if b[1] < -90 || b[1] > 90 || b[3] < -90 || b[3] > 90 {
		return fmt.Errorf("bounds 纬度必须在 [-90, 90]，收到 south=%v north=%v", b[1], b[3])
	}
	if b[0] >= b[2] {
		return fmt.Errorf("bounds west (%v) 必须小于 east (%v)", b[0], b[2])
	}
	if b[1] >= b[3] {
		return fmt.Errorf("bounds south (%v) 必须小于 north (%v)", b[1], b[3])
	}
	return nil
}

// fileIsNonEmpty 文件存在且字节数 > 0
func fileIsNonEmpty(p string) bool {
	if p == "" {
		return false
	}
	fi, err := os.Stat(p)
	if err != nil {
		return false
	}
	return fi.Size() > 0
}
