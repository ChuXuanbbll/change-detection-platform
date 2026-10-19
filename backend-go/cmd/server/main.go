package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"cdp/backend-go/internal/ai"
	"cdp/backend-go/internal/api"
	"cdp/backend-go/internal/client"
	"cdp/backend-go/internal/model"
	"cdp/backend-go/internal/progress"
	"cdp/backend-go/internal/queue"
	"cdp/backend-go/internal/store"

	"github.com/gin-gonic/gin"
	"gopkg.in/yaml.v3"
)

// maxErrMsgLen 落库的错误信息最大长度，避免日志字段过长
const maxErrMsgLen = 1000

// Config 与 config.yaml 对齐
type Config struct {
	GoAPIPort   int      `yaml:"go_api_port"`
	PyAlgoURL   string   `yaml:"py_algo_url"`
	DataDir     string   `yaml:"data_dir"`
	UploadMaxMB int      `yaml:"upload_max_mb"`
	CORSOrigins []string `yaml:"cors_origins"`
	Queue       struct {
		Workers int `yaml:"workers"`
		Buffer  int `yaml:"buffer"`
	} `yaml:"queue"`
}

func loadConfig(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Config
	if err := yaml.Unmarshal(b, &c); err != nil {
		return nil, err
	}
	if c.GoAPIPort == 0 {
		c.GoAPIPort = 8080
	}
	if c.PyAlgoURL == "" {
		c.PyAlgoURL = "http://localhost:8090"
	}
	if c.DataDir == "" {
		c.DataDir = "./data"
	}
	if c.UploadMaxMB == 0 {
		c.UploadMaxMB = 2048
	}
	return &c, nil
}

func main() {
	cfgPath := os.Getenv("CONFIG_PATH")
	if cfgPath == "" {
		cfgPath = "config.yaml"
	}
	cfg, err := loadConfig(cfgPath)
	if err != nil {
		log.Fatalf("[main] 读取配置失败: %v", err)
	}

	// 允许用环境变量覆盖配置（容器 / compose 场景），优先于 config.yaml
	if v := os.Getenv("PY_ALGO_URL"); v != "" {
		cfg.PyAlgoURL = v
	}
	if v := os.Getenv("GO_API_PORT"); v != "" {
		if p, perr := strconv.Atoi(v); perr == nil {
			cfg.GoAPIPort = p
		}
	}
	if v := os.Getenv("DATA_DIR"); v != "" {
		cfg.DataDir = v
	}
	if v := os.Getenv("UPLOAD_MAX_MB"); v != "" {
		if m, merr := strconv.Atoi(v); merr == nil {
			cfg.UploadMaxMB = m
		}
	}

	// 数据目录必须为绝对路径：Python 端要求 output_dir 绝对（共享卷约定），
	// 相对路径会被其校验为 400 OUTPUT_DIR_NOT_ABSOLUTE。
	if abs, aerr := filepath.Abs(cfg.DataDir); aerr == nil {
		cfg.DataDir = abs
	}

	// 数据目录
	if err := os.MkdirAll(filepath.Join(cfg.DataDir, "images"), 0o755); err != nil {
		log.Fatalf("[main] 创建数据目录失败: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(cfg.DataDir, "results"), 0o755); err != nil {
		log.Fatalf("[main] 创建结果目录失败: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(cfg.DataDir, "shared"), 0o755); err != nil {
		log.Fatalf("[main] 创建 shared 目录失败: %v", err)
	}

	// SQLite
	dbPath := filepath.Join(cfg.DataDir, "meta.db")
	db, err := store.Open(dbPath)
	if err != nil {
		log.Fatalf("[main] 打开 SQLite 失败: %v", err)
	}
	defer db.Close()
	if err := db.Migrate(); err != nil {
		log.Fatalf("[main] 迁移 SQLite 失败: %v", err)
	}

	if err := db.InterruptTasks(); err != nil {
		log.Fatal(err)
	}

	// Python 客户端
	py := client.New(cfg.PyAlgoURL, 10*time.Second, 3)

	// 任务队列（MVP）
	q := queue.New(cfg.Queue.Buffer)
	// 真实 worker：调入 Python /py/detect 执行变更检测并落盘结果
	q.Start(1, func(taskID string) {
		log.Printf("[worker] 开始处理任务 %s", taskID)
		handleTask(db, py, cfg.DataDir, taskID)
	})

	// Gin
	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	r.Use(gin.Recovery(), gin.Logger())
	r.MaxMultipartMemory = int64(cfg.UploadMaxMB) << 20

	deps := &api.Deps{
		DB: db,
		Py: py,
		Q:  q,
		Cfg: &api.Config{
			DataDir:     cfg.DataDir,
			CORSOrigins: cfg.CORSOrigins,
			UploadMaxMB: cfg.UploadMaxMB,
		},
		UploadMaxMB: cfg.UploadMaxMB,
	}
	if err := api.LoadAdministrativeRegions(deps); err != nil {
		log.Fatalf("[administrative] 加载行政区边界失败: %v", err)
	}
	management, err := api.NewManagement(deps)
	if err != nil {
		log.Fatal(err)
	}
	aiDomain, err := api.NewAIDomain(deps)
	if err != nil {
		log.Fatalf("[ai] 初始化工具域失败: %v", err)
	}
	aiGateway, err := ai.NewGateway(cfg.DataDir, aiDomain.Specs(), aiDomain)
	if err != nil {
		log.Fatalf("[ai] 初始化网关失败: %v", err)
	}
	defer aiGateway.Close()
	deps.AI = aiGateway
	// Management is initialized before the gateway so the gateway can safely
	// receive the persisted provider settings after construction.
	if revision, e := db.ActiveRevision(); e == nil {
		management.ConfigureAI(revision.Settings)
	}
	api.RegisterRoutes(r, deps)
	go func() {
		for attempt := 0; attempt < 10; attempt++ {
			if err := management.Sync(context.Background()); err == nil {
				return
			} else {
				log.Printf("[settings] %v", err)
			}
			time.Sleep(3 * time.Second)
		}
	}()

	listenHost := os.Getenv("GO_LISTEN_HOST")
	if listenHost == "" {
		listenHost = "127.0.0.1"
	}
	addr := listenHost + ":" + strconv.Itoa(cfg.GoAPIPort)
	srv := &http.Server{
		Addr:              addr,
		Handler:           r,
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		log.Printf("[main] Go API 监听 %s", addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("[main] 监听失败: %v", err)
		}
	}()

	// 优雅退出
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop
	log.Println("[main] 收到退出信号，开始关闭...")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx)
	_ = py.JSON(ctx, "POST", "/py/admin/stop", nil, nil)
	_ = q.Stop(ctx)
	_ = db.InterruptTasks()
}

// handleTask 真实执行一次变更检测：取任务/影像 -> 调到 Python /py/detect -> 落盘结果并更新状态。
// 任何一步失败都会将任务标记为 failed 并写入截断后的错误信息。
func handleTask(db *store.DB, py *client.PyClient, dataDir, taskID string) {
	fail := func(errMsg string) {
		truncated := errMsg
		if len(truncated) > maxErrMsgLen {
			truncated = truncated[:maxErrMsgLen]
		}
		_ = db.UpdateTaskStatus(taskID, model.TaskFailed, -1, &truncated)
	}

	defer func() {
		if r := recover(); r != nil {
			fail(fmt.Sprintf("worker panic: %v", r))
		}
	}()
	task, err := db.GetTask(taskID)
	if err != nil {
		log.Printf("[worker] 任务 %s 不存在: %v", taskID, err)
		fail("task not found: " + err.Error())
		return
	}

	beforeImg, err := db.GetImage(task.BeforeID)
	if err != nil {
		log.Printf("[worker] 任务 %s before 影像缺失: %v", taskID, err)
		fail("before image missing: " + err.Error())
		return
	}
	afterImg, err := db.GetImage(task.AfterID)
	if err != nil {
		log.Printf("[worker] 任务 %s after 影像缺失: %v", taskID, err)
		fail("after image missing: " + err.Error())
		return
	}

	// 解析参数；无参数时用空 map
	var params map[string]interface{}
	if task.ParamsJSON != "" {
		if err := json.Unmarshal([]byte(task.ParamsJSON), &params); err != nil {
			log.Printf("[worker] 任务 %s 参数解析失败: %v", taskID, err)
			fail("invalid params_json: " + err.Error())
			return
		}
	}

	// 输出目录，结果文件按任务隔离落盘
	outputDir := filepath.Join(dataDir, "results", taskID)
	if err := os.MkdirAll(outputDir, 0o755); err != nil {
		log.Printf("[worker] 任务 %s 创建结果目录失败: %v", taskID, err)
		fail("mkdir output_dir: " + err.Error())
		return
	}

	// 标记 running 后调用 Python
	if err := db.UpdateTaskStatus(taskID, model.TaskRunning, 0, nil); err != nil {
		fail(err.Error())
		return
	}
	log.Printf("[worker] 任务 %s 调入 Python /py/detect (algorithm=%s)", taskID, task.Algorithm)
	stopProgress := progress.Watch(outputDir, func(s progress.Snapshot) {
		if err := db.UpdateTaskStage(taskID, s.Stage, int(s.Progress)); err != nil {
			log.Printf("[worker] progress %s: %v", taskID, err)
		}
	})
	defer stopProgress()
	resp, err := py.Detect(context.Background(), client.DetectReq{
		BeforePath: beforeImg.FilePath,
		AfterPath:  afterImg.FilePath,
		Algorithm:  task.Algorithm,
		Params:     params,
		OutputDir:  outputDir,
	})
	stopProgress()
	if err != nil {
		msg := "py detect failed: " + err.Error()
		log.Printf("[worker] 任务 %s 检测失败: %v", taskID, err)
		fail(msg)
		return
	}

	_ = db.UpdateTaskStage(taskID, "saving", 99)
	// 组装结果并落库（results.task_id 为主键，INSERT OR REPLACE）
	statsJSON := ""
	if resp.Stats != nil {
		if b, mErr := json.Marshal(resp.Stats); mErr == nil {
			statsJSON = string(b)
		}
	}
	result := &model.Result{
		TaskID:         taskID,
		MaskPath:       resp.MaskPath,
		ConfidencePath: resp.ConfidencePath,
		GeoJSONPath:    resp.GeoJSONPath,
		StatsJSON:      statsJSON,
	}
	if err := db.CompleteTask(result); err != nil {
		msg := "persist result: " + err.Error()
		log.Printf("[worker] 任务 %s 结果落库失败: %v", taskID, err)
		fail(msg)
		return
	}
	log.Printf("[worker] 任务 %s 完成，结果已落盘到 %s", taskID, outputDir)
}
