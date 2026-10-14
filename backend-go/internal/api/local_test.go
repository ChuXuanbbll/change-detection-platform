package api

import (
	"cdp/backend-go/internal/client"
	"cdp/backend-go/internal/model"
	"cdp/backend-go/internal/store"
	"encoding/json"
	"fmt"
	"github.com/gin-gonic/gin"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLocalOutsideProvinceIsValidationError(t *testing.T) {
	calls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Path != "/py/local/fetch" {
			t.Errorf("unexpected endpoint: %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"detail":"选区位于河北范围之外"}`))
	}))
	defer upstream.Close()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.POST("/api/images/local", LocalFetchHandler(&Deps{Cfg: &Config{DataDir: t.TempDir()}, Py: client.New(upstream.URL, time.Second, 3)}))
	req := httptest.NewRequest("POST", "/api/images/local", strings.NewReader(`{"bounds":[116.38,39.88,116.42,39.92],"start_date":"2024-01-01","end_date":"2026-09-22","max_images":6}`))
	req.Header.Set("Content-Type", "application/json")
	out := httptest.NewRecorder()
	r.ServeHTTP(out, req)
	if out.Code != 400 || !strings.Contains(out.Body.String(), "河北范围之外") {
		t.Fatalf("unexpected response: %d %s", out.Code, out.Body.String())
	}
	if calls != 1 {
		t.Fatalf("validation error retried %d times", calls)
	}
}

func TestLocalSelectedScenesAndMosaicMetadata(t *testing.T) {
	root := t.TempDir()
	db, err := store.Open(filepath.Join(root, "meta.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.Migrate(); err != nil {
		t.Fatal(err)
	}
	calls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/py/thumbnail" {
			_, _ = w.Write([]byte(`{"ok":true}`))
			return
		}
		calls++
		var request model.LocalFetchReq
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
		}
		if request.RegionID != "xiongan" || len(request.SceneIDs) != 2 || request.SceneIDs[0] != "fine" {
			t.Errorf("selected sources lost: %#v", request)
		}
		_ = json.NewEncoder(w).Encode(model.LocalFetchResp{Count: 1, Images: []model.LocalImageItem{{
			Path: filepath.Join(request.OutputDir, "mosaic.tif"), Date: "2025-05-01", DatePrecision: "month",
			SourceFile: "fine.tif", SourceFiles: []string{"fine.tif", "coarse.tif"}, SceneIDs: []string{"fine", "coarse"},
			Bounds: [4]float64{114, 38, 115, 39}, Width: 100, Height: 100,
		}}})
	}))
	defer upstream.Close()
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.POST("/api/images/local", LocalFetchHandler(&Deps{DB: db, Cfg: &Config{DataDir: root}, Py: client.New(upstream.URL, time.Second, 1)}))
	for _, body := range []string{`{"bounds":[114,38,115,39],"region_id":"xiongan","scene_ids":["fine","coarse"]}`, `{"bounds":[114,38,115,39],"region_id":"xiongan","scene_ids":[]}`, `{"bounds":[114,38,115,39],"region_id":"xiongan","scene_ids":null}`} {
		request := httptest.NewRequest("POST", "/api/images/local", strings.NewReader(body))
		request.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		router.ServeHTTP(response, request)
		if strings.Contains(body, `"scene_ids":[]`) || strings.Contains(body, `"scene_ids":null`) {
			if response.Code != 400 {
				t.Fatalf("empty selection fell back: %d", response.Code)
			}
			continue
		}
		if response.Code != 201 {
			t.Fatalf("%d: %s", response.Code, response.Body.String())
		}
		var result struct {
			Items []model.Image `json:"items"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		var metadata map[string]any
		if err := json.Unmarshal([]byte(result.Items[0].MetaJSON), &metadata); err != nil {
			t.Fatal(err)
		}
		if len(metadata["source_files"].([]any)) != 2 || len(metadata["scene_ids"].([]any)) != 2 {
			t.Fatalf("mosaic provenance lost: %#v", metadata)
		}
	}
	if calls != 1 {
		t.Fatalf("empty selection reached upstream: %d", calls)
	}
}

func TestLocalFetchAllPhasesPersistsSourceImagery(t *testing.T) {
	root := t.TempDir()
	db, err := store.Open(filepath.Join(root, "meta.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.Migrate(); err != nil {
		t.Fatal(err)
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/py/thumbnail" {
			_, _ = w.Write([]byte(`{"ok":true}`))
			return
		}
		var request model.LocalFetchReq
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
		}
		if request.MaxImages != 0 || request.StartDate != "0001-01-01" || request.EndDate != "9999-12-31" {
			t.Errorf("unexpected all-phase request: %#v", request)
		}
		images := make([]model.LocalImageItem, 25)
		for i := range images {
			images[i] = model.LocalImageItem{Path: filepath.Join(request.OutputDir, fmt.Sprintf("scene-%d.tif", i)),
				Date: fmt.Sprintf("%d-09-01", 1990+i), SourceFile: fmt.Sprintf("source-%d.tif", i),
				Bounds: [4]float64{114, 38, 115, 39}, Width: 100, Height: 100}
		}
		images[1].DatePrecision = "year"
		_ = json.NewEncoder(w).Encode(model.LocalFetchResp{Images: images, Count: len(images)})
	}))
	defer upstream.Close()
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.POST("/api/images/local", LocalFetchHandler(&Deps{DB: db, Cfg: &Config{DataDir: root}, Py: client.New(upstream.URL, time.Second, 1)}))
	request := httptest.NewRequest("POST", "/api/images/local", strings.NewReader(`{"bounds":[114,38,115,39]}`))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusCreated {
		t.Fatalf("%d: %s", response.Code, response.Body.String())
	}
	var result struct {
		Items []model.Image `json:"items"`
		Count int           `json:"count"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Count != 25 {
		t.Fatalf("expected all 25 phases, got %d", result.Count)
	}
	saved, err := db.GetImage(result.Items[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	var metadata map[string]any
	if err := json.Unmarshal([]byte(saved.MetaJSON), &metadata); err != nil {
		t.Fatal(err)
	}
	if metadata["source_file"] != "source-0.tif" || metadata["date_precision"] != "month" {
		t.Fatalf("source metadata lost: %#v", metadata)
	}
	annual, err := db.GetImage(result.Items[1].ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(annual.MetaJSON), &metadata); err != nil {
		t.Fatal(err)
	}
	if metadata["date_precision"] != "year" {
		t.Fatalf("annual precision lost: %#v", metadata)
	}
}

func TestLocalFetchRejectsInvalidExplicitLimit(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.POST("/api/images/local", LocalFetchHandler(&Deps{}))
	for _, limit := range []int{-1, 21} {
		request := httptest.NewRequest("POST", "/api/images/local", strings.NewReader(fmt.Sprintf(`{"bounds":[114,38,115,39],"max_images":%d}`, limit)))
		request.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		router.ServeHTTP(response, request)
		if response.Code != http.StatusBadRequest {
			t.Fatalf("%d: %s", response.Code, response.Body.String())
		}
	}
}
