package api

import (
	"cdp/backend-go/internal/client"
	"cdp/backend-go/internal/model"
	"cdp/backend-go/internal/store"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

func TestPolygonFetchForwardsGeometryAndPersistsMetadata(t *testing.T) {
	root := t.TempDir()
	db, err := store.Open(filepath.Join(root, "meta.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.Migrate(); err != nil {
		t.Fatal(err)
	}
	aoi := `{"type":"Polygon","coordinates":[[[114,38],[115,38],[114,39],[114,38]]]}`
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/py/thumbnail" {
			_, _ = w.Write([]byte(`{"ok":true}`))
			return
		}
		var req model.LocalFetchReq
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
		}
		if string(req.AOI) != aoi || req.RegionID != "130100" || len(req.SceneIDs) != 2 {
			t.Errorf("polygon or scene selection lost: %#v", req)
		}
		_ = json.NewEncoder(w).Encode(model.LocalFetchResp{AOI: req.AOI, Count: 1, Images: []model.LocalImageItem{{
			Path: filepath.Join(req.OutputDir, "triangle.tif"), Date: "2025-01-01",
			Bounds: [4]float64{114, 38, 115, 39}, Width: 100, Height: 100,
		}}})
	}))
	defer upstream.Close()
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.POST("/api/images/local", LocalFetchHandler(&Deps{DB: db, Cfg: &Config{DataDir: root}, Py: client.New(upstream.URL, time.Second, 3)}))
	req := httptest.NewRequest("POST", "/api/images/local", strings.NewReader(`{"bounds":[114,38,115,39],"region_id":"130100","scene_ids":["a","b"],"aoi":`+aoi+`}`))
	req.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, req)
	if response.Code != http.StatusCreated {
		t.Fatalf("%d: %s", response.Code, response.Body.String())
	}
	var result struct {
		Items []model.Image `json:"items"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	saved, err := db.GetImage(result.Items[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	var meta struct {
		AOI      json.RawMessage `json:"aoi"`
		RegionID string          `json:"region_id"`
		Bounds   [4]float64      `json:"bounds"`
	}
	if err := json.Unmarshal([]byte(saved.MetaJSON), &meta); err != nil {
		t.Fatal(err)
	}
	if string(meta.AOI) != aoi || meta.RegionID != "130100" || meta.Bounds != [4]float64{114, 38, 115, 39} {
		t.Fatalf("polygon metadata lost: %#v", meta)
	}
}

func TestPolygonValidationErrorIsNotRetried(t *testing.T) {
	calls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(400)
		_, _ = w.Write([]byte(`{"detail":"多边形须完整位于当前行政区内"}`))
	}))
	defer upstream.Close()
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.POST("/api/images/local", LocalFetchHandler(&Deps{Cfg: &Config{DataDir: t.TempDir()}, Py: client.New(upstream.URL, time.Second, 3)}))
	req := httptest.NewRequest("POST", "/api/images/local", strings.NewReader(`{"bounds":[114,38,115,39],"region_id":"130100","aoi":{"type":"Polygon","coordinates":[]}}`))
	req.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, req)
	if response.Code != 400 || calls != 1 || !strings.Contains(response.Body.String(), "多边形须完整") {
		t.Fatalf("%d calls=%d: %s", response.Code, calls, response.Body.String())
	}
}

func TestPolygonFetchRejectsUnconfirmedAOIAndCleansBatch(t *testing.T) {
	for _, confirmation := range []string{"", `,"aoi":{"type":"Polygon","coordinates":[]}`} {
		t.Run(confirmation, func(t *testing.T) {
			root := t.TempDir()
			var output string
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var req model.LocalFetchReq
				_ = json.NewDecoder(r.Body).Decode(&req)
				output = req.OutputDir
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"images":[],"count":0` + confirmation + `}`))
			}))
			defer upstream.Close()
			router := gin.New()
			router.POST("/api/images/local", LocalFetchHandler(&Deps{Cfg: &Config{DataDir: root}, Py: client.New(upstream.URL, time.Second, 3)}))
			req := httptest.NewRequest("POST", "/api/images/local", strings.NewReader(`{"bounds":[114,38,115,39],"region_id":"130100","aoi":{"type":"Polygon","coordinates":[[[114,38],[115,38],[114,39],[114,38]]]}}`))
			req.Header.Set("Content-Type", "application/json")
			response := httptest.NewRecorder()
			router.ServeHTTP(response, req)
			if response.Code != http.StatusBadGateway || !strings.Contains(response.Body.String(), "AOI_NOT_CONFIRMED") {
				t.Fatalf("%d: %s", response.Code, response.Body.String())
			}
			if _, err := os.Stat(output); !os.IsNotExist(err) {
				t.Fatalf("unconfirmed batch was not cleaned: %v", err)
			}
		})
	}
}
