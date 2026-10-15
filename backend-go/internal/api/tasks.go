package api

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"cdp/backend-go/internal/client"
	"cdp/backend-go/internal/model"

	"github.com/gin-gonic/gin"
)

// GetTaskHandler GET /api/tasks/:id
func GetTaskHandler(d *Deps) gin.HandlerFunc {
	return func(c *gin.Context) {
		id := c.Param("id")
		t, err := d.DB.GetTask(id)
		if err != nil {
			c.JSON(http.StatusNotFound, gin.H{
				"error":  "task not found",
				"code":   "TASK_NOT_FOUND",
				"id":     id,
				"detail": err.Error(),
			})
			return
		}
		c.JSON(http.StatusOK, t)
	}
}

// GetTaskResultHandler GET /api/tasks/:id/result
func GetTaskResultHandler(d *Deps) gin.HandlerFunc {
	return func(c *gin.Context) {
		id := c.Param("id")
		if _, err := d.DB.GetTask(id); err != nil {
			c.JSON(http.StatusNotFound, gin.H{
				"error":  "task not found",
				"code":   "TASK_NOT_FOUND",
				"id":     id,
				"detail": err.Error(),
			})
			return
		}
		r, err := d.DB.GetResult(id)
		if err != nil {
			c.JSON(http.StatusNotFound, gin.H{
				"error":   "result not ready",
				"code":    "RESULT_NOT_READY",
				"task_id": id,
				"message": "任务尚未完成或尚未生成结果",
			})
			return
		}
		// Keep file-based persistence, but return the patch collection inline so
		// the frontend receives geometry and patch statistics in one package.
		response := struct {
			*model.Result
			GeoJSON      json.RawMessage `json:"geojson,omitempty"`
			GeoJSONError string          `json:"geojson_error,omitempty"`
		}{Result: r}
		if r.GeoJSONPath != "" {
			data, err := os.ReadFile(r.GeoJSONPath)
			var collection struct {
				Type     string            `json:"type"`
				Features []json.RawMessage `json:"features"`
			}
			if err != nil {
				response.GeoJSONError = "GEOJSON_MISSING"
			} else if json.Unmarshal(data, &collection) != nil || collection.Type != "FeatureCollection" || collection.Features == nil {
				response.GeoJSONError = "GEOJSON_INVALID"
			} else {
				response.GeoJSON = data
			}
		}
		c.JSON(http.StatusOK, response)
	}
}

// DownloadResultHandler GET /api/tasks/:id/result/:file
// :file 必须是 mask.tif / change.geojson / stats.csv / change.zip
func DownloadResultHandler(d *Deps) gin.HandlerFunc {
	return func(c *gin.Context) {
		taskID := c.Param("id")
		name := c.Param("file")
		if !isAllowedResultFile(name) {
			c.JSON(http.StatusBadRequest, gin.H{
				"error":    "file 仅允许 mask.tif / change.geojson / stats.csv / change.zip",
				"code":     "INVALID_FILE",
				"received": name,
			})
			return
		}
		r, err := d.DB.GetResult(taskID)
		if err != nil || r == nil {
			c.JSON(http.StatusNotFound, gin.H{
				"error":   "result not ready",
				"code":    "RESULT_NOT_READY",
				"task_id": taskID,
			})
			return
		}
		switch name {
		case "change.zip":
			if r.GeoJSONPath == "" || !fileExists(r.GeoJSONPath) {
				c.JSON(http.StatusNotFound, gin.H{"error": "矢量文件不存在", "code": "GEOJSON_MISSING"})
				return
			}
			collection, err := os.ReadFile(r.GeoJSONPath)
			if err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"error": "矢量文件读取失败", "code": "GEOJSON_READ_FAILED"})
				return
			}
			if !json.Valid(collection) {
				c.JSON(http.StatusBadRequest, gin.H{"error": "GeoJSON 内容无效", "code": "GEOJSON_INVALID"})
				return
			}
			if d.Py == nil {
				c.JSON(http.StatusBadGateway, gin.H{"error": "SHP 转换服务不可用", "code": "SHP_EXPORT_FAILED"})
				return
			}
			archive, err := d.Py.ExportShapefile(c.Request.Context(), json.RawMessage(collection))
			if err != nil {
				var upstream *client.UpstreamError
				if errors.As(err, &upstream) && (upstream.StatusCode == http.StatusBadRequest || upstream.StatusCode == http.StatusUnprocessableEntity) {
					message := "GeoJSON 内容无效，无法导出 SHP"
					var body struct {
						Detail string `json:"detail"`
					}
					if json.Unmarshal([]byte(upstream.Body), &body) == nil && body.Detail != "" {
						message = body.Detail
					}
					c.JSON(http.StatusBadRequest, gin.H{"error": message, "code": "GEOJSON_INVALID"})
				} else {
					c.JSON(http.StatusBadGateway, gin.H{"error": "SHP 转换失败，请检查 Python 服务后重试", "code": "SHP_EXPORT_FAILED"})
				}
				return
			}
			c.Header("Content-Disposition", fmt.Sprintf("attachment; filename=\"result_%s_shp.zip\"", taskID))
			c.Data(http.StatusOK, "application/zip", archive)
			return
		case "mask.tif":
			if r.MaskPath == "" || !fileExists(r.MaskPath) {
				c.JSON(http.StatusNotFound, gin.H{
					"error": "掩膜文件不存在",
					"code":  "MASK_MISSING",
					"path":  r.MaskPath,
				})
				return
			}
			c.Header("Content-Disposition", fmt.Sprintf("attachment; filename=\"%s_mask.tif\"", taskID))
			c.Header("Content-Type", "image/tiff")
			c.File(r.MaskPath)
			return
		case "change.geojson":
			if r.GeoJSONPath == "" || !fileExists(r.GeoJSONPath) {
				c.JSON(http.StatusNotFound, gin.H{
					"error": "矢量文件不存在",
					"code":  "GEOJSON_MISSING",
					"path":  r.GeoJSONPath,
				})
				return
			}
			c.Header("Content-Disposition", fmt.Sprintf("attachment; filename=\"%s_change.geojson\"", taskID))
			c.Header("Content-Type", "application/geo+json")
			c.File(r.GeoJSONPath)
			return
		case "stats.csv":
			// 真实从 StatsJSON 渲染 CSV：按 key 排序，输出 key,value 两列。
			if r.StatsJSON == "" {
				c.JSON(http.StatusNotFound, gin.H{
					"error":   "stats_json 为空，无法渲染 CSV",
					"code":    "STATS_JSON_EMPTY",
					"task_id": taskID,
				})
				return
			}
			var stats map[string]any
			if err := json.Unmarshal([]byte(r.StatsJSON), &stats); err != nil {
				c.JSON(http.StatusBadRequest, gin.H{
					"error":  "stats_json 解析失败",
					"code":   "STATS_JSON_INVALID",
					"detail": err.Error(),
				})
				return
			}
			keys := make([]string, 0, len(stats))
			for k := range stats {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			var buf bytes.Buffer
			w := csv.NewWriter(&buf)
			_ = w.Write([]string{"metric", "value"})
			for _, k := range keys {
				row := []string{k, fmt.Sprintf("%v", stats[k])}
				_ = w.Write(row)
			}
			w.Flush()
			c.Header("Content-Disposition", fmt.Sprintf("attachment; filename=\"%s_stats.csv\"", taskID))
			c.Header("Content-Type", "text/csv")
			c.String(http.StatusOK, buf.String())
			return
		}
	}
}

func isAllowedResultFile(name string) bool {
	switch name {
	case "mask.tif", "change.geojson", "stats.csv", "change.zip":
		return true
	}
	return false
}

func contentTypeFor(name string) string {
	switch {
	case strings.HasSuffix(name, ".zip"):
		return "application/zip"
	case strings.HasSuffix(name, ".tif"):
		return "image/tiff"
	case strings.HasSuffix(name, ".geojson"):
		return "application/geo+json"
	case strings.HasSuffix(name, ".csv"):
		return "text/csv"
	}
	return "application/octet-stream"
}

func fileExists(p string) bool {
	if p == "" {
		return false
	}
	if _, err := os.Stat(filepath.Clean(p)); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false
		}
		return false
	}
	return true
}
