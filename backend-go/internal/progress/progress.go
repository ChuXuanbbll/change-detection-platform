// Package progress reads best-effort snapshots from the shared output directory.
package progress

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sync"
	"time"
)

type Snapshot struct {
	Stage    string  `json:"stage"`
	Progress float64 `json:"progress"`
}

var ranges = map[string][2]float64{
	"preparing": {0, 95}, "reading_images": {0, 95}, "cropping": {0, 95},
	"mosaicking": {0, 95}, "saving_images": {0, 99},
	"aligning": {0, 5}, "model_loading": {5, 10}, "inference": {10, 85},
	"reprojecting": {85, 90}, "postprocessing": {90, 94}, "vectorizing": {94, 98},
	"statistics": {98, 99}, "saving": {99, 99},
}

func read(path string, value any) bool {
	data, err := os.ReadFile(path)
	return err == nil && len(data) <= 4096 && json.Unmarshal(data, value) == nil
}

func Read(outputDir string) (Snapshot, bool) {
	dir := filepath.Join(outputDir, "progress")
	var raw struct {
		Stage    string
		Progress *float64
	}
	if !read(filepath.Join(dir, "stage.json"), &raw) || raw.Progress == nil {
		return Snapshot{}, false
	}
	s := Snapshot{Stage: raw.Stage, Progress: *raw.Progress}
	bounds, valid := ranges[s.Stage]
	if !valid || math.IsNaN(s.Progress) || math.IsInf(s.Progress, 0) || s.Progress < bounds[0] || s.Progress > bounds[1] {
		return Snapshot{}, false
	}
	if s.Stage == "model_loading" || s.Stage == "inference" {
		if p, ok := inference(dir); ok {
			return Snapshot{Stage: "inference", Progress: p}, true
		}
	}
	return s, true
}

func inference(dir string) (float64, bool) {
	type worker struct{ Rank, World, Done, Total *int }
	var first worker
	if !read(filepath.Join(dir, "worker-0.json"), &first) || first.World == nil || *first.World < 1 || *first.World > 256 {
		return 0, false
	}
	world := *first.World
	done, total := int64(0), int64(0)
	for rank := 0; rank < world; rank++ {
		var w worker
		if !read(filepath.Join(dir, fmt.Sprintf("worker-%d.json", rank)), &w) || w.Rank == nil || w.World == nil || w.Done == nil || w.Total == nil {
			return 0, false
		}
		if *w.Rank != rank || *w.World != world || *w.Total < 0 || *w.Total > 1_000_000_000 || *w.Done < 0 || *w.Done > *w.Total {
			return 0, false
		}
		done += int64(*w.Done)
		total += int64(*w.Total)
	}
	if total == 0 {
		return 85, true
	}
	return 10 + 75*float64(done)/float64(total), true
}

// Watch returns an idempotent stop function which joins the reader and takes a
// final snapshot. Call it before committing a terminal state.
func Watch(dir string, update func(Snapshot)) func() {
	stop, exited := make(chan struct{}), make(chan struct{})
	sample := func() {
		if s, ok := Read(dir); ok {
			update(s)
		}
	}
	go func() {
		defer close(exited)
		ticker := time.NewTicker(500 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				sample()
			case <-stop:
				sample()
				return
			}
		}
	}()
	var once sync.Once
	return func() { once.Do(func() { close(stop); <-exited }) }
}
