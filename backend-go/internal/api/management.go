package api

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"cdp/backend-go/internal/ai"
	"cdp/backend-go/internal/model"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// The lock serializes configuration changes and crop admission. Existing batches
// read their immutable snapshot and never consult the current model defaults.
type Management struct {
	mu        sync.RWMutex
	d         *Deps
	synced    int64
	lastError string
}

func ControlToken(dir string) (string, error) {
	path := filepath.Join(dir, "control.token")
	b, e := os.ReadFile(path)
	if e == nil {
		return strings.TrimSpace(string(b)), nil
	}
	if !os.IsNotExist(e) {
		return "", e
	}
	b = make([]byte, 32)
	if _, e = rand.Read(b); e != nil {
		return "", e
	}
	token := hex.EncodeToString(b)
	f, e := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if e != nil {
		return "", e
	}
	defer f.Close()
	_, e = f.WriteString(token)
	return token, e
}

func NewManagement(d *Deps) (*Management, error) {
	if e := d.DB.MigrateSettings(); e != nil {
		return nil, e
	}
	if e := d.DB.RecoverSettings(); e != nil {
		return nil, e
	}
	token, e := ControlToken(d.Cfg.DataDir)
	if e != nil {
		return nil, e
	}
	d.Py.ControlToken = token
	m := &Management{d: d}
	d.Management = m
	return m, nil
}

func (m *Management) Sync(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	e := m.sync(ctx)
	if e != nil {
		m.synced = 0
		m.lastError = e.Error()
	} else {
		m.lastError = ""
	}
	return e
}

// ConfigureAI is called once the gateway is constructed and whenever settings
// are activated through the management workflow.
func (m *Management) ConfigureAI(s model.Settings) {
	applyAIConfig(m.d.AI, s)
}
func (m *Management) sync(ctx context.Context) error {
	r, e := m.d.DB.ActiveRevision()
	if errors.Is(e, sql.ErrNoRows) {
		var b struct {
			Settings model.Settings `json:"settings"`
			Path     string         `json:"model_path"`
			Hash     string         `json:"sha256"`
		}
		if e = m.d.Py.JSON(ctx, "GET", "/py/admin/bootstrap", nil, &b); e != nil {
			return e
		}
		envAI := ai.ProviderConfigFromEnv()
		b.Settings.AI = model.AISettings{APIBase: envAI.BaseURL, APIKey: envAI.APIKey, Model: envAI.Model, TimeoutMS: int(envAI.Timeout / time.Millisecond)}
		if _, err := m.d.DB.Model("builtin-hebei"); errors.Is(err, sql.ErrNoRows) {
			builtin := model.ManagedModel{ID: "builtin-hebei", Name: "河北变化检测", Path: b.Path, Hash: b.Hash, Status: "unvalidated", Mapping: map[string]string{}}
			if e = m.d.DB.SaveModel(builtin); e != nil {
				return e
			}
		}
		id, err := m.d.DB.NewRevision(b.Settings)
		if err != nil {
			return err
		}
		// Persist the migrated values even when an unavailable drive prevents activation
		// in Python. The UI reports unsynchronized and lets the user repair the sources.
		if e = m.d.DB.ActivateRevision(id); e != nil {
			return e
		}
		r, e = m.d.DB.ActiveRevision()
	}
	if e != nil {
		return e
	}
	r.Settings = defaultAISettings(r.Settings)
	var status struct {
		Revision int64 `json:"revision"`
	}
	if e = m.applyPython(ctx, r); e != nil {
		return e
	}
	applyAIConfig(m.d.AI, r.Settings)
	if e = m.d.Q.Configure(r.Settings.Runtime["workers"], r.Settings.Runtime["queue_capacity"]); e != nil {
		return e
	}
	if e = m.d.Py.JSON(ctx, "GET", "/py/admin/status", nil, &status); e != nil {
		return e
	}
	if status.Revision != r.ID {
		return fmt.Errorf("Python 配置版本未确认")
	}
	m.synced = r.ID
	return nil
}
func (m *Management) applyPython(ctx context.Context, r *model.Revision) error {
	return m.d.Py.JSON(ctx, "POST", "/py/admin/apply", gin.H{"revision": r.ID, "settings": pythonSettings(r.Settings)}, nil)
}

func pythonSettings(s model.Settings) any {
	return gin.H{"sources": s.Sources, "default_model": s.DefaultModel, "detection": s.Detection, "runtime": s.Runtime, "tiles": s.Tiles, "browser": s.Browser}
}

func maskAPIKey(key string) string {
	if key == "" {
		return ""
	}
	if strings.Contains(key, "...") {
		return key
	}
	if len(key) <= 8 {
		return "********"
	}
	return key[:6] + "..." + key[len(key)-4:]
}

func redactedSettings(s model.Settings) model.Settings {
	s.AI.APIKey = maskAPIKey(s.AI.APIKey)
	return s
}

func mergeSecretSettings(next, old model.Settings) model.Settings {
	if next.AI.APIKey == "" || strings.Contains(next.AI.APIKey, "...") {
		next.AI.APIKey = old.AI.APIKey
	}
	return next
}

func applyAIConfig(g *ai.Gateway, s model.Settings) {
	if g == nil {
		return
	}
	timeout := time.Duration(s.AI.TimeoutMS) * time.Millisecond
	if timeout <= 0 {
		timeout = 45 * time.Second
	}
	g.ConfigureProvider(ai.ProviderConfig{BaseURL: strings.TrimRight(s.AI.APIBase, "/"), APIKey: s.AI.APIKey, Model: s.AI.Model, Timeout: timeout})
}

func defaultAISettings(s model.Settings) model.Settings {
	if s.AI.APIBase != "" || s.AI.APIKey != "" || s.AI.Model != "" || s.AI.TimeoutMS != 0 {
		return s
	}
	c := ai.ProviderConfigFromEnv()
	s.AI = model.AISettings{APIBase: c.BaseURL, APIKey: c.APIKey, Model: c.Model, TimeoutMS: int(c.Timeout / time.Millisecond)}
	return s
}

func localHost(value string) bool {
	host := value
	if h, _, e := net.SplitHostPort(value); e == nil {
		host = h
	}
	host = strings.Trim(host, "[]")
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
func localAdmin(c *gin.Context) {
	host, _, e := net.SplitHostPort(c.Request.RemoteAddr)
	trustedProxy := false
	if os.Getenv("CDP_TRUST_LOCAL_PROXY") == "1" {
		token, err := os.ReadFile(filepath.Join(os.Getenv("DATA_DIR"), "control.token"))
		trustedProxy = err == nil && len(token) >= 32 && subtle.ConstantTimeCompare(token, []byte(c.GetHeader("X-CDP-Proxy"))) == 1
	}
	if e != nil || (!localHost(host) && !trustedProxy) || !localHost(c.Request.Host) || c.GetHeader("X-Forwarded-For") != "" || c.GetHeader("Forwarded") != "" {
		c.AbortWithStatusJSON(403, gin.H{"error": "管理入口仅允许本机直连"})
		return
	}
	if origin := c.GetHeader("Origin"); origin != "" {
		u, e := url.Parse(origin)
		if e != nil || !localHost(u.Host) || (u.Scheme != "http" && u.Scheme != "https") {
			c.AbortWithStatusJSON(403, gin.H{"error": "不允许的 Origin"})
			return
		}
	}
	if c.Request.Method != "GET" && c.Request.Method != "HEAD" {
		ct := c.ContentType()
		if ct != "application/json" && ct != "application/octet-stream" {
			c.AbortWithStatusJSON(415, gin.H{"error": "管理请求必须使用 JSON 或 ONNX 二进制"})
			return
		}
	}
	c.Next()
}
func apiError(c *gin.Context, e error) { c.JSON(400, gin.H{"error": e.Error()}) }

func (m *Management) operation(c *gin.Context, kind string, run func(context.Context) (any, error)) {
	op := model.Operation{ID: uuid.NewString(), Kind: kind, Status: "running"}
	if e := m.d.DB.SaveOperation(op); e != nil {
		apiError(c, e)
		return
	}
	go func(op model.Operation) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
		defer cancel()
		var result any
		var err error
		func() {
			defer func() {
				if p := recover(); p != nil {
					err = fmt.Errorf("管理操作异常: %v", p)
				}
			}()
			result, err = run(ctx)
		}()
		op.Status = "succeeded"
		if err != nil {
			op.Status = "failed"
			op.Error = err.Error()
		} else {
			op.Result, _ = json.Marshal(result)
		}
		if e := m.d.DB.SaveOperation(op); e != nil {
			m.mu.Lock()
			m.lastError = "保存操作结果失败: " + e.Error()
			m.mu.Unlock()
		}
	}(op)
	c.JSON(202, op)
}

func (m *Management) register(api *gin.RouterGroup) {
	g := api.Group("", localAdmin)
	g.GET("/settings", func(c *gin.Context) {
		m.mu.RLock()
		defer m.mu.RUnlock()
		r, e := m.d.DB.ActiveRevision()
		if e != nil {
			c.JSON(503, gin.H{"error": e.Error()})
			return
		}
		r.Settings = defaultAISettings(r.Settings)
		history, e := m.d.DB.RevisionHistory()
		if e != nil {
			apiError(c, e)
			return
		}
		var diagnostics any
		err := m.d.Py.JSON(c.Request.Context(), "GET", "/py/admin/status", nil, &diagnostics)
		effective := m.synced == r.ID && err == nil
		if info, ok := diagnostics.(map[string]any); ok {
			effective = effective && info["revision"] == float64(r.ID)
		}
		for i := range history {
			history[i].Settings = redactedSettings(history[i].Settings)
		}
		c.JSON(200, gin.H{"revision": r.ID, "settings": redactedSettings(r.Settings), "effective": effective, "error": m.lastError, "history": history, "diagnostics": gin.H{"queue": m.d.Q.Stats(), "python": diagnostics, "go_api_port": c.Request.Host, "python_url": m.d.Py.BaseURL, "database": filepath.Join(m.d.Cfg.DataDir, "meta.db")}})
	})
	g.GET("/settings/operations/:id", func(c *gin.Context) {
		op, e := m.d.DB.Operation(c.Param("id"))
		if e != nil {
			c.JSON(404, gin.H{"error": e.Error()})
			return
		}
		c.JSON(200, op)
	})
	g.POST("/settings/sync", func(c *gin.Context) {
		m.operation(c, "sync", func(ctx context.Context) (any, error) { return nil, m.Sync(ctx) })
	})
	g.POST("/settings/validate", func(c *gin.Context) {
		var s model.Settings
		if e := c.ShouldBindJSON(&s); e != nil {
			apiError(c, e)
			return
		}
		m.operation(c, "validate", func(ctx context.Context) (any, error) {
			var out any
			e := m.d.Py.JSON(ctx, "POST", "/py/admin/validate", pythonSettings(s), &out)
			return out, e
		})
	})
	apply := func(c *gin.Context) {
		var req struct {
			Expected int64          `json:"expected_revision"`
			Settings model.Settings `json:"settings"`
			Rollback int64          `json:"rollback_revision"`
		}
		if e := c.ShouldBindJSON(&req); e != nil {
			apiError(c, e)
			return
		}
		m.mu.RLock()
		r, e := m.d.DB.ActiveRevision()
		m.mu.RUnlock()
		if e != nil {
			apiError(c, e)
			return
		}
		if r.ID != req.Expected {
			c.JSON(409, gin.H{"error": "配置已被其他页面更新，请重新加载", "code": "REVISION_CONFLICT"})
			return
		}
		m.operation(c, "apply", func(ctx context.Context) (any, error) {
			m.mu.Lock()
			defer m.mu.Unlock()
			old, e := m.d.DB.ActiveRevision()
			if e != nil {
				return nil, e
			}
			old.Settings = defaultAISettings(old.Settings)
			if old.ID != req.Expected {
				return nil, fmt.Errorf("REVISION_CONFLICT: 配置已更新，请重新加载")
			}
			s := req.Settings
			s = mergeSecretSettings(s, old.Settings)
			if req.Rollback != 0 {
				prior, e := m.d.DB.Revision(req.Rollback)
				if e != nil {
					return nil, e
				}
				s = prior.Settings
				s = defaultAISettings(s)
			}
			mdl, e := m.d.DB.Model(s.DefaultModel)
			if e != nil {
				return nil, e
			}
			if !mdl.Enabled || mdl.Status != "ready" {
				return nil, fmt.Errorf("默认模型尚未验证启用")
			}
			var validated struct {
				Settings model.Settings `json:"settings"`
			}
			if e = m.d.Py.JSON(ctx, "POST", "/py/admin/validate", pythonSettings(s), &validated); e != nil {
				return nil, e
			}
			validated.Settings.AI = s.AI
			s = validated.Settings
			if s.Runtime["gpu_device"] != old.Settings.Runtime["gpu_device"] {
				var report model.ManagedModel
				if e = m.d.Py.JSON(ctx, "POST", "/py/admin/models/validate", gin.H{"path": mdl.Path, "sha256": mdl.Hash, "mapping": mdl.Mapping, "builtin": mdl.ID == "builtin-hebei", "gpu_device": s.Runtime["gpu_device"]}, &report); e != nil {
					return nil, e
				}
				if report.Status != "ready" {
					return nil, fmt.Errorf("GPU 验证未通过: %s", report.Error)
				}
			}

			if e = m.d.Q.Configure(s.Runtime["workers"], s.Runtime["queue_capacity"]); e != nil {
				return nil, e
			}
			id, e := m.d.DB.NewRevision(s)
			if e == nil {
				e = m.applyPython(ctx, &model.Revision{ID: id, Settings: s})
			}
			if e == nil {
				e = m.d.DB.ActivateRevision(id)
			}
			if e != nil {
				_ = m.d.DB.FailRevision(id)
				m.synced = 0
				recovery, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
				defer cancel()
				rollback := m.applyPython(recovery, old)
				_ = m.d.Q.Configure(old.Settings.Runtime["workers"], old.Settings.Runtime["queue_capacity"])
				if rollback == nil {
					m.synced = old.ID
				}
				m.lastError = e.Error()
				return nil, e
			}
			m.synced = id
			applyAIConfig(m.d.AI, s)
			m.lastError = ""
			return gin.H{"revision": id}, nil
		})
	}
	g.POST("/settings/apply", apply)
	g.POST("/settings/rollback", apply)
	g.GET("/models", func(c *gin.Context) {
		models, e := m.d.DB.Models()
		if e != nil {
			apiError(c, e)
			return
		}
		c.JSON(200, models)
	})
	g.POST("/models/import", m.importModel)
	g.POST("/models/:id/validate", func(c *gin.Context) {
		id := c.Param("id")
		var req struct {
			Mapping map[string]string `json:"mapping"`
		}
		if e := c.ShouldBindJSON(&req); e != nil {
			apiError(c, e)
			return
		}
		m.operation(c, "model-validation", func(ctx context.Context) (any, error) { return m.probe(ctx, id, req.Mapping) })
	})
	g.PUT("/models/:id", func(c *gin.Context) {
		m.mu.Lock()
		defer m.mu.Unlock()
		mdl, e := m.d.DB.Model(c.Param("id"))
		if e != nil {
			apiError(c, e)
			return
		}
		var req struct {
			Name        string `json:"name"`
			Description string `json:"description"`
			Enabled     bool   `json:"enabled"`
		}
		if e = c.ShouldBindJSON(&req); e != nil {
			apiError(c, e)
			return
		}
		if req.Enabled && mdl.Status != "ready" {
			apiError(c, fmt.Errorf("模型尚未通过 CUDA 验证"))
			return
		}
		if strings.TrimSpace(req.Name) == "" {
			apiError(c, fmt.Errorf("名称不能为空"))
			return
		}
		mdl.Name = req.Name
		mdl.Description = req.Description
		mdl.Enabled = req.Enabled
		if e = m.d.DB.SaveModel(mdl); e != nil {
			apiError(c, e)
			return
		}
		c.JSON(200, mdl)
	})
	g.DELETE("/models/:id", func(c *gin.Context) {
		m.mu.Lock()
		defer m.mu.Unlock()
		id := c.Param("id")
		history, e := m.d.DB.RevisionHistory()
		if e != nil {
			apiError(c, e)
			return
		}
		for _, r := range history {
			if r.Settings.DefaultModel == id {
				apiError(c, fmt.Errorf("配置历史引用此模型，只能停用"))
				return
			}
		}
		if e = m.d.DB.DeleteModel(id); e != nil {
			apiError(c, e)
			return
		}
		c.JSON(200, gin.H{"ok": true})
	})
}

func (m *Management) probe(ctx context.Context, id string, mapping map[string]string) (any, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	mdl, e := m.d.DB.Model(id)
	if e != nil {
		return nil, e
	}
	if mapping == nil {
		mapping = mdl.Mapping
	}
	var report model.ManagedModel
	gpu := 0
	if r, e := m.d.DB.ActiveRevision(); e == nil {
		gpu = r.Settings.Runtime["gpu_device"]
	}
	e = m.d.Py.JSON(ctx, "POST", "/py/admin/models/validate", gin.H{"path": mdl.Path, "sha256": mdl.Hash, "mapping": mapping, "builtin": id == "builtin-hebei", "gpu_device": gpu}, &report)
	if e != nil {
		mdl.Status = "invalid"
		mdl.Enabled = false
		mdl.Error = e.Error()
	} else {
		mdl.Hash = report.Hash
		mdl.Status = report.Status
		mdl.Mapping = report.Mapping
		mdl.Inputs = report.Inputs
		mdl.Outputs = report.Outputs
		mdl.Error = report.Error
		mdl.Enabled = report.Status == "ready"
	}
	if save := m.d.DB.SaveModel(mdl); save != nil {
		return nil, save
	}
	return mdl, e
}
func (m *Management) importModel(c *gin.Context) {
	dir := filepath.Join(m.d.Cfg.DataDir, "models")
	if e := os.MkdirAll(dir, 0755); e != nil {
		apiError(c, e)
		return
	}
	var source io.ReadCloser
	name := c.Query("name")
	if c.ContentType() == "application/json" {
		var req struct {
			Path string `json:"path"`
			Name string `json:"name"`
		}
		if e := c.ShouldBindJSON(&req); e != nil {
			apiError(c, e)
			return
		}
		if !filepath.IsAbs(req.Path) || !strings.EqualFold(filepath.Ext(req.Path), ".onnx") {
			apiError(c, fmt.Errorf("请提供服务端单文件 ONNX 的绝对路径"))
			return
		}
		f, e := os.Open(req.Path)
		if e != nil {
			apiError(c, e)
			return
		}
		source = f
		name = req.Name
	} else {
		source = http.MaxBytesReader(c.Writer, c.Request.Body, int64(m.d.Cfg.UploadMaxMB)<<20)
	}
	defer source.Close()
	f, e := os.CreateTemp(dir, "import-*.partial")
	if e != nil {
		apiError(c, e)
		return
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	hash := sha256.New()
	n, e := io.Copy(io.MultiWriter(f, hash), source)
	closeErr := f.Close()
	if e == nil {
		e = closeErr
	}
	if e != nil {
		apiError(c, e)
		return
	}
	if n == 0 {
		apiError(c, fmt.Errorf("模型文件为空"))
		return
	}
	digest := hex.EncodeToString(hash.Sum(nil))
	path := filepath.Join(dir, digest+".onnx")
	m.mu.Lock()
	models, e := m.d.DB.Models()
	if e == nil {
		for _, mdl := range models {
			if mdl.Hash == digest {
				m.mu.Unlock()
				c.JSON(200, mdl)
				return
			}
		}
	}
	if e != nil {
		m.mu.Unlock()
		apiError(c, e)
		return
	}
	if _, e = os.Stat(path); os.IsNotExist(e) {
		e = os.Rename(tmp, path)
	}
	if e != nil {
		m.mu.Unlock()
		apiError(c, e)
		return
	}
	if name == "" {
		name = "ONNX " + digest[:12]
	}
	mdl := model.ManagedModel{ID: uuid.NewString(), Name: name, Path: path, Hash: digest, Status: "unvalidated", Mapping: map[string]string{}}
	e = m.d.DB.SaveModel(mdl)
	m.mu.Unlock()
	if e != nil {
		apiError(c, e)
		return
	}
	m.operation(c, "model-import", func(ctx context.Context) (any, error) { return m.probe(ctx, mdl.ID, nil) })
}

func (m *Management) freeze(ctx context.Context, id, modelID string, revision int64, params map[string]any) (model.BatchSnapshot, error) {
	var b model.BatchSnapshot
	r, e := m.d.DB.ActiveRevision()
	if e != nil {
		return b, e
	}
	if revision != r.ID || m.synced != r.ID {
		return b, fmt.Errorf("配置尚未同步或页面版本过期，请刷新")
	}
	var status struct {
		Revision int64 `json:"revision"`
	}
	if e = m.d.Py.JSON(ctx, "GET", "/py/admin/status", nil, &status); e != nil {
		return b, e
	}
	if status.Revision != r.ID {
		return b, fmt.Errorf("Python 重启后配置尚未同步，请在管理页重新同步")
	}
	if modelID == "" {
		modelID = r.Settings.DefaultModel
	}
	mdl, e := m.d.DB.Model(modelID)
	if e != nil {
		return b, e
	}
	if !mdl.Enabled || mdl.Status != "ready" {
		return b, fmt.Errorf("所选模型尚未验证或已停用")
	}
	final := map[string]any{}
	for k, v := range r.Settings.Detection {
		final[k] = v
	}
	for k, v := range params {
		if _, ok := final[k]; !ok {
			if k == "provider" && v == "cuda" {
				continue
			}
			return b, fmt.Errorf("不支持的检测参数 %s", k)
		}
		final[k] = v
	}
	var canonical map[string]any
	if e = m.d.Py.JSON(ctx, "POST", "/py/admin/detection/validate", final, &canonical); e != nil {
		return b, e
	}
	canonical["provider"] = "cuda"
	canonical["_execution"] = gin.H{"model_id": mdl.ID, "model_path": mdl.Path, "sha256": mdl.Hash, "mapping": mdl.Mapping, "config_revision": r.ID, "batch_id": id, "runtime": r.Settings.Runtime}
	b = model.BatchSnapshot{ID: id, Revision: r.ID, Model: mdl, Params: canonical}
	return b, m.d.DB.SaveBatch(b)
}
