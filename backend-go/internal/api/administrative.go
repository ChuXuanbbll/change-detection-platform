package api

import (
	"github.com/gin-gonic/gin"
	"os"
	"path/filepath"
)

// LoadAdministrativeRegions publishes a versioned local snapshot. No remote
// data requests occur during startup, and previous versions remain queryable.
func LoadAdministrativeRegions(d *Deps) error {
	path := os.Getenv("ADMIN_BOUNDARY_FILE")
	if path == "" {
		assets := os.Getenv("ASSET_DIR")
		if assets == "" {
			assets = filepath.Join(filepath.Dir(d.Cfg.DataDir), "..", "data")
		}
		path = filepath.Join(assets, "boundaries", "hebei-administrative.geojson")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return d.DB.ImportAdministrativeRegions(raw)
}

func registerAdministrativeRoutes(r *gin.RouterGroup, d *Deps) {
	r.GET("/administrative-regions", func(c *gin.Context) {
		level := c.DefaultQuery("level", "county")
		if level != "county" && level != "prefecture" && level != "province" {
			c.JSON(400, gin.H{"error": "行政级别无效"})
			return
		}
		features, err := d.DB.AdministrativeRegions(c.Query("source_id"), level)
		if err != nil {
			c.JSON(503, gin.H{"error": "行政区边界尚未就绪"})
			return
		}
		c.JSON(200, gin.H{"type": "FeatureCollection", "features": features})
	})
	r.GET("/administrative-regions/:code", func(c *gin.Context) {
		f, err := d.DB.AdministrativeRegion(c.Param("code"), c.Query("version"))
		if err != nil {
			c.JSON(404, gin.H{"error": "行政区或边界版本不存在"})
			return
		}
		c.JSON(200, f)
	})
}
