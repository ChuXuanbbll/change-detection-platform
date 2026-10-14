package api

import (
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
)

// HealthHandler GET /health
func HealthHandler(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{
		"status": "ok",
		"time":   time.Now().UTC().Format(time.RFC3339),
	})
}
