package model

import "time"

// TaskStatus 任务状态枚举
const (
	TaskPending   = "pending"
	TaskRunning   = "running"
	TaskCompleted = "completed"
	TaskSucceeded = "succeeded"
	TaskFailed    = "failed"
)

// Task 对应 tasks 表
type Task struct {
	ID         string     `json:"id"`
	BeforeID   string     `json:"before_id"`
	AfterID    string     `json:"after_id"`
	Algorithm  string     `json:"algorithm"`
	ParamsJSON string     `json:"params_json,omitempty"`
	Status     string     `json:"status"`
	Stage      string     `json:"stage"`
	Progress   int        `json:"progress"`
	ErrorMsg   string     `json:"error_msg,omitempty"`
	StartedAt  *time.Time `json:"started_at,omitempty"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
}

// Result 对应 results 表
type Result struct {
	TaskID         string    `json:"task_id"`
	MaskPath       string    `json:"mask_path"`
	ConfidencePath string    `json:"confidence_path,omitempty"`
	GeoJSONPath    string    `json:"geojson_path,omitempty"`
	StatsJSON      string    `json:"stats_json,omitempty"`
	CreatedAt      time.Time `json:"created_at"`
}

// CreateTaskReq 创建任务请求体
type CreateTaskReq struct {
	BatchID   string                 `json:"batch_id"`
	BeforeID  string                 `json:"before_id" binding:"required"`
	AfterID   string                 `json:"after_id" binding:"required"`
	Algorithm string                 `json:"algorithm"`
	Params    map[string]interface{} `json:"params"`
}
