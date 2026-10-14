package api

import (
	"archive/zip"
	"bytes"
	"encoding/binary"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"cdp/backend-go/internal/client"
	"cdp/backend-go/internal/model"
	"cdp/backend-go/internal/store"
	"github.com/gin-gonic/gin"
)

// Optional integration against the real Python service; no existing task data is changed.
func TestShapefileLiveConversion(t *testing.T) {
	baseURL := os.Getenv("SHP_PY_URL")
	if baseURL == "" {
		t.Skip("Set SHP_PY_URL to run Go → Python ZIP integration")
	}
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
	if err := db.InsertTask(&model.Task{ID: "live", Algorithm: "hebei_onnx", Status: model.TaskSucceeded, CreatedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "change.geojson")
	fixture := `{"type":"FeatureCollection","features":[{"type":"Feature","id":"patch-1","geometry":{"type":"Polygon","coordinates":[[[114,38],[115,38],[115,39],[114,39],[114,38]]]},"properties":{"area_m2":100,"confidence_mean":null}}]}`
	if err := os.WriteFile(path, []byte(fixture), 0600); err != nil {
		t.Fatal(err)
	}
	if err := db.InsertResult(&model.Result{TaskID: "live", GeoJSONPath: path}); err != nil {
		t.Fatal(err)
	}
	router := gin.New()
	router.GET("/api/tasks/:id/result/:file", DownloadResultHandler(&Deps{DB: db, Py: client.New(baseURL, time.Second, 1)}))
	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest("GET", "/api/tasks/live/result/change.zip", nil))
	if response.Code != 200 {
		t.Fatalf("%d: %s", response.Code, response.Body.String())
	}
	archive, err := zip.NewReader(bytes.NewReader(response.Body.Bytes()), int64(response.Body.Len()))
	if err != nil {
		t.Fatal(err)
	}
	files := map[string][]byte{}
	for _, file := range archive.File {
		stream, err := file.Open()
		if err != nil {
			t.Fatal(err)
		}
		files[file.Name], err = io.ReadAll(stream)
		stream.Close()
		if err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"change.shp", "change.shx", "change.dbf", "change.prj", "change.cpg", "README.txt", "change.geojson"} {
		if len(files[name]) == 0 {
			t.Fatalf("missing ZIP member %s", name)
		}
	}
	if binary.BigEndian.Uint32(files["change.shp"][:4]) != 9994 || binary.LittleEndian.Uint32(files["change.shp"][32:36]) != 5 {
		t.Fatal("invalid polygon SHP header")
	}
	if binary.LittleEndian.Uint32(files["change.dbf"][4:8]) != 1 {
		t.Fatal("expected one patch record")
	}
}

func TestShapefileDownload(t *testing.T) {
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
	mask := filepath.Join(dir, "mask.tif")
	if err := os.WriteFile(mask, []byte("tiff-fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := db.InsertResult(&model.Result{TaskID: "task", GeoJSONPath: path, MaskPath: mask, StatsJSON: `{"feature_count":0}`}); err != nil {
		t.Fatal(err)
	}
	fixture := `{"type":"FeatureCollection","features":[]}`
	if err := os.WriteFile(path, []byte(fixture), 0600); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	writer := zip.NewWriter(&buf)
	file, _ := writer.Create("change.shp")
	file.Write([]byte("shp-fixture"))
	writer.Close()
	upstreamStatus := http.StatusOK
	calls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		body, _ := io.ReadAll(r.Body)
		if r.Method != "POST" || r.URL.Path != "/py/export/shp" || string(body) != fixture || r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("unexpected conversion request: %s %s %s", r.Method, r.URL.Path, body)
		}
		if upstreamStatus != http.StatusOK {
			w.WriteHeader(upstreamStatus)
			w.Write([]byte(`{"detail":"图斑几何为空或无效"}`))
			return
		}
		w.Header().Set("Content-Type", "application/zip")
		w.Write(buf.Bytes())
	}))
	defer upstream.Close()
	router := gin.New()
	router.GET("/api/tasks/:id/result/:file", DownloadResultHandler(&Deps{DB: db, Py: client.New(upstream.URL, time.Second, 1)}))
	download := func(task, file string) *httptest.ResponseRecorder {
		response := httptest.NewRecorder()
		router.ServeHTTP(response, httptest.NewRequest("GET", "/api/tasks/"+task+"/result/"+file, nil))
		return response
	}
	response := download("task", "change.zip")
	if response.Code != 200 || response.Header().Get("Content-Type") != "application/zip" || response.Header().Get("Content-Disposition") != `attachment; filename="result_task_shp.zip"` || !bytes.Equal(response.Body.Bytes(), buf.Bytes()) {
		t.Fatalf("unexpected archive response: %d %v %s", response.Code, response.Header(), response.Body.String())
	}
	for _, file := range []string{"mask.tif", "change.geojson", "stats.csv"} {
		if response := download("task", file); response.Code != 200 {
			t.Fatalf("%s: %d %s", file, response.Code, response.Body.String())
		}
	}
	for _, status := range []int{400, 422, 500} {
		upstreamStatus = status
		response := download("task", "change.zip")
		expectedStatus, code := 400, "GEOJSON_INVALID"
		if status == 500 {
			expectedStatus, code = 502, "SHP_EXPORT_FAILED"
		}
		if response.Code != expectedStatus || !strings.Contains(response.Body.String(), code) {
			t.Fatalf("upstream %d: %d %s", status, response.Code, response.Body.String())
		}
	}
	before := calls
	if err := os.WriteFile(path, []byte(`{"type":`), 0600); err != nil {
		t.Fatal(err)
	}
	if response := download("task", "change.zip"); response.Code != 400 || !strings.Contains(response.Body.String(), "GEOJSON_INVALID") {
		t.Fatalf("invalid JSON: %d %s", response.Code, response.Body.String())
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if response := download("task", "change.zip"); response.Code != 404 || !strings.Contains(response.Body.String(), "GEOJSON_MISSING") {
		t.Fatalf("missing JSON: %d %s", response.Code, response.Body.String())
	}
	if response := download("missing-task", "change.zip"); response.Code != 404 || !strings.Contains(response.Body.String(), "RESULT_NOT_READY") {
		t.Fatalf("missing result: %d %s", response.Code, response.Body.String())
	}
	if response := download("task", "unknown.zip"); response.Code != 400 || !strings.Contains(response.Body.String(), "INVALID_FILE") {
		t.Fatalf("invalid file: %d %s", response.Code, response.Body.String())
	}
	if calls != before {
		t.Fatal("invalid or missing result must not call conversion service")
	}
	if contentTypeFor("change.zip") != "application/zip" {
		t.Fatal("incorrect ZIP content type")
	}
}
