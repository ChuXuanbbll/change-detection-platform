package api

import (
	"log"
	"net/http"

	"cdp/backend-go/internal/model"

	"github.com/gin-gonic/gin"
)

// ListAlgorithmsHandler GET /api/algorithms
// 透传 Python GET /py/algorithms；不可用时返 503。
func ListAlgorithmsHandler(d *Deps) gin.HandlerFunc {
	return func(c *gin.Context) {
		algos, err := d.Py.GetAlgorithms(c.Request.Context())
		if err != nil {
			log.Printf("[algorithms] py unavailable: %v", err)
			c.JSON(http.StatusServiceUnavailable, gin.H{
				"error":      "PYTHON_UNAVAILABLE",
				"detail":     err.Error(),
				"algorithms": []model.Algorithm{},
			})
			return
		}
		c.JSON(http.StatusOK, gin.H{
			"algorithms": algos,
			"count":      len(algos),
			"source":     "py",
		})
	}
}
