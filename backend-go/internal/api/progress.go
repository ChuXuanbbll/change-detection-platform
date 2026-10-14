package api

import (
	"net/http"
	"sync"
	"time"

	"cdp/backend-go/internal/progress"
	"github.com/gin-gonic/gin"
)

type preparation struct {
	ID     string `json:"id"`
	Status string `json:"status"`
	progress.Snapshot
}

type preparationRegistry struct {
	mu    sync.Mutex
	items map[string]preparation
}

func (r *preparationRegistry) start(id string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.items == nil {
		r.items = make(map[string]preparation)
	}
	if _, exists := r.items[id]; exists {
		return false
	}
	r.items[id] = preparation{ID: id, Status: "running", Snapshot: progress.Snapshot{Stage: "preparing"}}
	return true
}

func (r *preparationRegistry) update(id string, s progress.Snapshot) {
	r.mu.Lock()
	defer r.mu.Unlock()
	p, ok := r.items[id]
	if !ok || p.Status != "running" || s.Progress < p.Progress || s.Progress >= 100 {
		return
	}
	p.Snapshot = s
	r.items[id] = p
}

func (r *preparationRegistry) finish(id string, success bool) {
	r.mu.Lock()
	p := r.items[id]
	if success {
		p.Status = "succeeded"
		p.Stage = "completed"
		p.Progress = 100
	} else {
		p.Status = "failed"
	}
	r.items[id] = p
	r.mu.Unlock()
	time.AfterFunc(10*time.Minute, func() { r.mu.Lock(); delete(r.items, id); r.mu.Unlock() })
}

func LocalProgressHandler(d *Deps) gin.HandlerFunc {
	return func(c *gin.Context) {
		d.preparations.mu.Lock()
		p, ok := d.preparations.items[c.Param("id")]
		d.preparations.mu.Unlock()
		c.Header("Cache-Control", "no-store")
		if !ok {
			c.JSON(http.StatusNotFound, gin.H{"code": "PROGRESS_NOT_FOUND", "error": "影像准备进度不存在或已过期"})
			return
		}
		c.JSON(http.StatusOK, p)
	}
}
