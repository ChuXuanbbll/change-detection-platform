package api

import (
	"cdp/backend-go/internal/client"
	"cdp/backend-go/internal/model"
	"cdp/backend-go/internal/progress"
	"cdp/backend-go/internal/store"
	"encoding/json"
	"github.com/gin-gonic/gin"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPreparationLiveProgressCompatibilityAndDuplicateID(t *testing.T) {
	root := t.TempDir()
	db, err := store.Open(filepath.Join(root, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err = db.Migrate(); err != nil {
		t.Fatal(err)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req model.LocalFetchReq
		_ = json.NewDecoder(r.Body).Decode(&req)
		dir := filepath.Join(req.OutputDir, "progress")
		_ = os.MkdirAll(dir, 0755)
		_ = os.WriteFile(filepath.Join(dir, "stage.json"), []byte(`{"stage":"mosaicking","progress":45}`), 0644)
		close(entered)
		<-release
		_ = json.NewEncoder(w).Encode(model.LocalFetchResp{Images: []model.LocalImageItem{{Path: filepath.Join(req.OutputDir, "image.tif"), Date: "2025-01-01"}}})
	}))
	defer upstream.Close()
	d := &Deps{DB: db, Cfg: &Config{DataDir: root}, Py: client.New(upstream.URL, time.Second, 1)}
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.POST("/api/images/local", LocalFetchHandler(d))
	router.GET("/api/images/local/progress/:id", LocalProgressHandler(d))
	body := `{"bounds":[114,38,115,39],"progress_id":"7bc91ad3-14b1-429c-b670-bf43d7691acd"}`
	post := func() *httptest.ResponseRecorder {
		out := httptest.NewRecorder()
		router.ServeHTTP(out, httptest.NewRequest("POST", "/api/images/local", strings.NewReader(body)))
		return out
	}
	finished := make(chan *httptest.ResponseRecorder, 1)
	go func() { finished <- post() }()
	<-entered
	// Always unblock the request if an assertion fails.
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	duplicate := post()
	if duplicate.Code != 409 {
		t.Fatalf("duplicate: %d", duplicate.Code)
	}
	query := func() preparation {
		out := httptest.NewRecorder()
		router.ServeHTTP(out, httptest.NewRequest("GET", "/api/images/local/progress/7bc91ad3-14b1-429c-b670-bf43d7691acd", nil))
		var p preparation
		_ = json.Unmarshal(out.Body.Bytes(), &p)
		return p
	}
	deadline := time.Now().Add(3 * time.Second)
	for query().Progress != 45 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if p := query(); p.Progress != 45 || p.Status != "running" {
		t.Fatalf("no live preparation: %+v", p)
	}
	close(release)
	out := <-finished
	if out.Code != 201 {
		t.Fatal(out.Body.String())
	}
	if p := query(); p.Progress != 100 || p.Status != "succeeded" {
		t.Fatal(p)
	}
	d.preparations.update("7bc91ad3-14b1-429c-b670-bf43d7691acd", progress.Snapshot{Stage: "mosaicking", Progress: 70})
	if p := query(); p.Progress != 100 {
		t.Fatal(p)
	}
	if post().Code != 409 {
		t.Fatal("finished ID reused")
	}
}
