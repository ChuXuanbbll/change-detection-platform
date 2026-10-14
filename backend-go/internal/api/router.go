package api

import (
	"net/http"
	"time"

	"cdp/backend-go/internal/client"
	"cdp/backend-go/internal/queue"
	"cdp/backend-go/internal/store"

	"github.com/gin-contrib/cors"
	"github.com/gin-gonic/gin"
)

// Deps 注入依赖
type Deps struct {
	preparations preparationRegistry
	Management   *Management
	DB           *store.DB
	Py           *client.PyClient
	Q            *queue.Queue
	Cfg          *Config
	UploadMaxMB  int
}

// Config 仅路由层关心的子集
type Config struct {
	DataDir     string
	CORSOrigins []string
	UploadMaxMB int
}

// RegisterRoutes 注册所有路由
func RegisterRoutes(r *gin.Engine, d *Deps) {
	// CORS
	r.Use(cors.New(cors.Config{
		AllowOrigins:     append([]string{}, d.Cfg.CORSOrigins...),
		AllowMethods:     []string{"GET", "POST", "PUT", "DELETE", "OPTIONS"},
		AllowHeaders:     []string{"Origin", "Content-Type", "Accept", "Authorization", "X-Requested-With"},
		ExposeHeaders:    []string{"Content-Length", "Content-Disposition"},
		AllowCredentials: false,
		MaxAge:           12 * time.Hour,
	}))

	// 健康检查
	r.GET("/health", HealthHandler)

	// 业务 API
	api := r.Group("/api")
	if d.Management != nil {
		d.Management.register(api)
	}
	{
		// 供前端 dev proxy（只转发 /api 前缀）访问的 Go 健康检查
		api.GET("/health", HealthHandler)

		imgs := api.Group("/images")
		{
			imgs.POST("/wayback", LocalFetchHandler(d)) // compatibility alias
			imgs.POST("/local", LocalFetchHandler(d))
			imgs.GET("/local/progress/:id", LocalProgressHandler(d))
			imgs.GET("", ListImagesHandler(d))
			imgs.GET("/:id/preview", ImagePreviewHandler(d))
			imgs.DELETE("/:id", DeleteImageHandler(d))
		}
		api.GET("/algorithms", ListAlgorithmsHandler(d))

		tasks := api.Group("/tasks")
		{
			tasks.POST("", CreateTaskHandler(d))
			tasks.GET("/:id", GetTaskHandler(d))
			tasks.GET("/:id/result", GetTaskResultHandler(d))
			tasks.GET("/:id/result/:file", DownloadResultHandler(d))
		}
	}

	// 404
	r.NoRoute(func(c *gin.Context) {
		c.JSON(http.StatusNotFound, gin.H{"error": "not found", "path": c.Request.URL.Path})
	})
}
