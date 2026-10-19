package ai

import (
	"context"
	"encoding/json"
)

// ToolSpec describes the stable public contract exposed to the language model.
// Schemas are JSON Schema Draft 2020-12 documents represented as decoded JSON.
type ToolSpec struct {
	Name          string         `json:"name"`
	Version       string         `json:"version"`
	Description   string         `json:"description"`
	InputSchema   map[string]any `json:"input_schema"`
	OutputSchema  map[string]any `json:"output_schema"`
	Effect        string         `json:"effect"`
	TimeoutPolicy map[string]any `json:"timeout_policy,omitempty"`
	RetryPolicy   map[string]any `json:"retry_policy,omitempty"`
	MaxOutputSize int            `json:"max_output_size"`
}

type Context struct {
	SessionID         string         `json:"session_id,omitempty"`
	WorkflowID        string         `json:"workflow_id,omitempty"`
	UIContextRevision string         `json:"ui_context_revision,omitempty"`
	Workspace         map[string]any `json:"workspace,omitempty"`
}

type Call struct {
	SchemaVersion string         `json:"schema_version"`
	RequestID     string         `json:"request_id"`
	Tool          string         `json:"tool"`
	ToolVersion   string         `json:"tool_version"`
	Input         map[string]any `json:"input"`
	Context       Context        `json:"context"`
}

type ToolError struct {
	Code               string   `json:"code"`
	Message            string   `json:"message"`
	Retryable          bool     `json:"retryable"`
	Field              *string  `json:"field,omitempty"`
	RecoverableActions []string `json:"recoverable_actions,omitempty"`
}

type Result struct {
	SchemaVersion string     `json:"schema_version"`
	RequestID     string     `json:"request_id"`
	Tool          string     `json:"tool"`
	ToolVersion   string     `json:"tool_version"`
	Status        string     `json:"status"`
	Data          any        `json:"data"`
	Refs          []any      `json:"refs"`
	Warnings      []string   `json:"warnings"`
	NextCursor    *string    `json:"next_cursor"`
	Error         *ToolError `json:"error"`
}

func (result Result) MarshalJSON() ([]byte, error) {
	type resultFields Result
	if result.Refs == nil {
		result.Refs = []any{}
	}
	if result.Warnings == nil {
		result.Warnings = []string{}
	}
	return json.Marshal(resultFields(result))
}

// Executor is the domain boundary. Implementations must return DTOs, never
// paths, credentials, SQL, shell commands, or traceback text.
type Executor interface {
	Execute(context.Context, Call) Result
}

// WorkflowProvider optionally exposes durable domain workflows created by a
// detection tool. Chat workflows and detection workflows have different IDs;
// this boundary lets the API query either without coupling ai to api.
type WorkflowProvider interface {
	GetWorkflow(context.Context, string) (any, error)
	GetEvents(context.Context, string, int64) ([]map[string]any, error)
	CancelWorkflow(context.Context, string) error
}

type UICommand struct {
	Action           string         `json:"action"`
	ActionID         string         `json:"action_id"`
	Args             map[string]any `json:"args,omitempty"`
	ExpectedRevision string         `json:"expected_revision,omitempty"`
}

type MessageRequest struct {
	Text      string         `json:"text"`
	RequestID string         `json:"request_id,omitempty"`
	Workspace map[string]any `json:"workspace,omitempty"`
	SkillID   string         `json:"skill_id,omitempty"`
}

type ActionAck struct {
	Status          string               `json:"status"`
	ContextRevision string               `json:"context_revision,omitempty"`
	Workspace       map[string]any       `json:"workspace,omitempty"`
	Error           *ToolError           `json:"error,omitempty"`
	Result          *DetectionSubmission `json:"result,omitempty"`
}

type DetectionSubmission struct {
	TaskID    string `json:"task_id"`
	PairCount int    `json:"pair_count"`
}
