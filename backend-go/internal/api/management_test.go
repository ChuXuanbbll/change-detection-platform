package api

import (
	"context"
	"encoding/json"
	"fmt"
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
	"cdp/backend-go/internal/queue"
	"cdp/backend-go/internal/store"
	"github.com/gin-gonic/gin"
)

type interruptedModelBody struct{ sent bool }

func (r *interruptedModelBody) Read(p []byte) (int, error) {
	if r.sent {
		return 0, io.ErrUnexpectedEOF
	}
	r.sent = true
	return copy(p, "partial ONNX upload"), nil
}

func TestInterruptedModelUploadRemovesPartialFile(t *testing.T) {
	gin.SetMode(gin.TestMode)
	dir := t.TempDir()
	m := &Management{d: &Deps{Cfg: &Config{DataDir: dir, UploadMaxMB: 1}}}
	r := gin.New()
	r.POST("/", m.importModel)
	req := httptest.NewRequest("POST", "/", &interruptedModelBody{})
	req.Header.Set("Content-Type", "application/octet-stream")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != 400 {
		t.Fatalf("interrupted upload returned %d: %s", w.Code, w.Body)
	}
	files, err := os.ReadDir(filepath.Join(dir, "models"))
	if err != nil || len(files) != 0 {
		t.Fatalf("partial upload retained: %v, %v", files, err)
	}
}

func TestAdministrationRejectsRemoteAndCrossOrigin(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, tc := range []struct {
		host, remote, origin, forwarded, ct string
		want                                int
	}{
		{"localhost:8080", "127.0.0.1:123", "http://localhost:5173", "", "application/json", 200},
		{"localhost:8080", "192.168.1.9:123", "", "", "application/json", 403},
		{"evil.example", "127.0.0.1:123", "", "", "application/json", 403},
		{"localhost", "127.0.0.1:123", "https://evil.example", "", "application/json", 403},
		{"localhost", "127.0.0.1:123", "", "192.168.1.9", "application/json", 403},
		{"localhost", "127.0.0.1:123", "", "", "text/plain", 415},
	} {
		t.Run(fmt.Sprint(tc), func(t *testing.T) {
			r := gin.New()
			r.POST("/", localAdmin, func(c *gin.Context) { c.Status(200) })
			req := httptest.NewRequest("POST", "http://"+tc.host+"/", strings.NewReader("{}"))
			req.RemoteAddr = tc.remote
			req.Header.Set("Origin", tc.origin)
			req.Header.Set("X-Forwarded-For", tc.forwarded)
			req.Header.Set("Content-Type", tc.ct)
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)
			if w.Code != tc.want {
				t.Fatalf("%d: %s", w.Code, w.Body)
			}
		})
	}
}

func TestSettingsRedactAndPreserveAIKey(t *testing.T) {
	old := model.Settings{AI: model.AISettings{APIKey: "sk-a91-secret-647e"}}
	masked := redactedSettings(old)
	if masked.AI.APIKey != "sk-a91...647e" {
		t.Fatalf("unexpected masked key: %q", masked.AI.APIKey)
	}
	if mergeSecretSettings(masked, old).AI.APIKey != old.AI.APIKey {
		t.Fatal("masked key should preserve the stored secret")
	}
	newSettings := model.Settings{AI: model.AISettings{APIKey: "sk-new-key"}}
	if mergeSecretSettings(newSettings, old).AI.APIKey != "sk-new-key" {
		t.Fatal("new key should replace the stored secret")
	}
}

func TestBatchFreezesValidatedModelAndEffectiveParameters(t *testing.T) {
	db, e := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if e != nil {
		t.Fatal(e)
	}
	defer db.Close()
	if e = db.MigrateSettings(); e != nil {
		t.Fatal(e)
	}
	s := model.Settings{DefaultModel: "m", Detection: map[string]any{"threshold": .5}, Runtime: map[string]int{"workers": 1, "queue_capacity": 8}}
	id, e := db.NewRevision(s)
	if e != nil {
		t.Fatal(e)
	}
	if e = db.ActivateRevision(id); e != nil {
		t.Fatal(e)
	}
	mdl := model.ManagedModel{ID: "m", Hash: "hash-a", Path: "immutable.onnx", Status: "ready", Enabled: true}
	if e = db.SaveModel(mdl); e != nil {
		t.Fatal(e)
	}
	py := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/py/admin/status" {
			fmt.Fprintf(w, `{"revision":%d}`, id)
			return
		}
		var params map[string]any
		_ = json.NewDecoder(r.Body).Decode(&params)
		_ = json.NewEncoder(w).Encode(params)
	}))
	defer py.Close()
	m := &Management{d: &Deps{DB: db, Py: client.New(py.URL, time.Second, 1), Q: queue.New(8)}, synced: id}
	b, e := m.freeze(context.Background(), "batch", "m", id, map[string]any{"threshold": .7})
	if e != nil {
		t.Fatal(e)
	}
	mdl.Enabled = false
	mdl.Hash = "hash-b"
	if e = db.SaveModel(mdl); e != nil {
		t.Fatal(e)
	}
	saved, e := db.Batch(b.ID)
	if e != nil {
		t.Fatal(e)
	}
	if saved.Model.Hash != "hash-a" || saved.Params["threshold"] != .7 {
		t.Fatalf("snapshot changed: %+v", saved)
	}
	if _, e = m.freeze(context.Background(), "new", "m", id, nil); e == nil {
		t.Fatal("disabled model admitted")
	}
	if e = db.DeleteModel("m"); e == nil {
		t.Fatal("referenced model deleted")
	}
	if _, e = m.freeze(context.Background(), "new", "m", id+1, nil); e == nil {
		t.Fatal("stale revision admitted")
	}
}
