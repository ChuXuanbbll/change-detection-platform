package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"cdp/backend-go/internal/model"
	"cdp/backend-go/internal/queue"
	"cdp/backend-go/internal/store"
	"github.com/gin-gonic/gin"
)

func TestDefaultModelTaskUsesCUDA(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db, err := store.Open(filepath.Join(t.TempDir(), "meta.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.Migrate(); err != nil {
		t.Fatal(err)
	}
	for _, phase := range []string{"before", "after"} {
		if err := db.InsertImage(&model.Image{ID: phase, Source: "local", Phase: phase,
			FilePath: phase + ".tif", CreatedAt: time.Now().UTC()}); err != nil {
			t.Fatal(err)
		}
	}
	q := queue.New(8)
	defer q.Stop(context.Background())
	router := gin.New()
	router.POST("/api/tasks", CreateTaskHandler(&Deps{DB: db, Q: q}))
	for _, algorithm := range []string{"", `,"algorithm":"hebei_onnx"`} {
		request := httptest.NewRequest("POST", "/api/tasks", strings.NewReader(
			`{"before_id":"before","after_id":"after"`+algorithm+`,"params":{"threshold":0.7}}`))
		request.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		router.ServeHTTP(response, request)
		if response.Code != http.StatusAccepted {
			t.Fatalf("%d: %s", response.Code, response.Body.String())
		}
		var task model.Task
		if err := json.Unmarshal(response.Body.Bytes(), &task); err != nil {
			t.Fatal(err)
		}
		saved, err := db.GetTask(task.ID)
		if err != nil {
			t.Fatal(err)
		}
		var params map[string]any
		if err := json.Unmarshal([]byte(saved.ParamsJSON), &params); err != nil {
			t.Fatal(err)
		}
		if saved.Algorithm != "hebei_onnx" || params["provider"] != "cuda" || params["threshold"] != 0.7 {
			t.Fatalf("unexpected saved task: %#v, params: %#v", saved, params)
		}
	}
}

func TestModelTaskRejectsNonCUDA(t *testing.T) {
	gin.SetMode(gin.TestMode)
	// No database or queue: invalid devices must fail before persistence.
	router := gin.New()
	router.POST("/api/tasks", CreateTaskHandler(&Deps{}))
	for _, provider := range []string{`"cpu"`, `"auto"`, `null`, `false`, `[]`, `{}`} {
		request := httptest.NewRequest("POST", "/api/tasks", strings.NewReader(
			`{"before_id":"before","after_id":"after","params":{"provider":`+provider+`}}`))
		request.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		router.ServeHTTP(response, request)
		if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "GPU_REQUIRED") {
			t.Fatalf("provider %s: %d %s", provider, response.Code, response.Body.String())
		}
	}
}

func TestResultIncludesPatchGeometryAndStatistics(t *testing.T) {
	gin.SetMode(gin.TestMode)
	dir := t.TempDir()
	db, err := store.Open(filepath.Join(dir, "meta.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.Migrate(); err != nil {
		t.Fatal(err)
	}
	if err := db.InsertTask(&model.Task{ID: "task", Algorithm: "hebei_onnx", Status: model.TaskSucceeded, CreatedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "change.geojson")
	if err := db.InsertResult(&model.Result{TaskID: "task", MaskPath: "mask.tif", GeoJSONPath: path, StatsJSON: `{"feature_count":2}`}); err != nil {
		t.Fatal(err)
	}
	router := gin.New()
	router.GET("/api/tasks/:id/result", GetTaskResultHandler(&Deps{DB: db}))
	for _, fixture := range []struct {
		name, data, expectedError string
	}{
		{"multiple", `{"type":"FeatureCollection","schema_version":2,"features":[{"type":"Feature","id":"patch-1","geometry":{"type":"Polygon","coordinates":[[[114,38],[115,38],[115,39],[114,38]]]},"properties":{"area_m2":455.21,"perimeter_m":100,"confidence_mean":0.9,"centroid":[114.5,38.5]}},{"type":"Feature","id":"patch-2","geometry":{"type":"Polygon","coordinates":[]},"properties":{"area_m2":800,"confidence_mean":null}}]}`, ""},
		{"empty", `{"type":"FeatureCollection","features":[]}`, ""},
		{"invalid", `{"type":"FeatureCollection","features":null}`, "GEOJSON_INVALID"},
		{"truncated", `{"type":`, "GEOJSON_INVALID"},
		{"missing", "", "GEOJSON_MISSING"},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			if fixture.data == "" {
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			} else if err := os.WriteFile(path, []byte(fixture.data), 0600); err != nil {
				t.Fatal(err)
			}
			response := httptest.NewRecorder()
			router.ServeHTTP(response, httptest.NewRequest("GET", "/api/tasks/task/result", nil))
			if response.Code != http.StatusOK {
				t.Fatalf("%d: %s", response.Code, response.Body.String())
			}
			var body struct {
				TaskID       string          `json:"task_id"`
				GeoJSON      json.RawMessage `json:"geojson"`
				GeoJSONError string          `json:"geojson_error"`
			}
			if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if body.TaskID != "task" || body.GeoJSONError != fixture.expectedError {
				t.Fatalf("unexpected package: %s", response.Body.String())
			}
			if fixture.expectedError == "" {
				var expected, actual any
				if err := json.Unmarshal([]byte(fixture.data), &expected); err != nil {
					t.Fatal(err)
				}
				if err := json.Unmarshal(body.GeoJSON, &actual); err != nil {
					t.Fatal(err)
				}
				expectedJSON, _ := json.Marshal(expected)
				actualJSON, _ := json.Marshal(actual)
				if string(expectedJSON) != string(actualJSON) {
					t.Fatalf("patch data was changed or omitted: %s", body.GeoJSON)
				}
			} else if len(body.GeoJSON) != 0 {
				t.Fatal("unreadable patch data must not masquerade as an empty collection")
			}
		})
	}
}
