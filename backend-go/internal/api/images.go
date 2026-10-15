package api

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"cdp/backend-go/internal/model"

	"github.com/gin-gonic/gin"
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

// GetImageHandler resolves an existing derived image for task restoration.
// This endpoint is used by the ordinary workspace adapter, never advertised to
// the LLM; AI tools use the separate path-free image DTO.
func GetImageHandler(d *Deps) gin.HandlerFunc {
	return func(c *gin.Context) {
		image, err := d.DB.GetImage(c.Param("id"))
		if err != nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "image not found", "code": "IMAGE_NOT_FOUND"})
			return
		}
		c.JSON(http.StatusOK, image)
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
