package ai

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

type Session struct {
	ID            string                   `json:"id"`
	CreatedAt     time.Time                `json:"created_at"`
	UpdatedAt     time.Time                `json:"updated_at"`
	SkillID       string                   `json:"skill_id,omitempty"`
	Messages      []SessionMessage         `json:"messages,omitempty"`
	Workspace     map[string]any           `json:"workspace,omitempty"`
	WorkflowIDs   []string                 `json:"workflow_ids,omitempty"`
	Actions       map[string]PendingAction `json:"actions,omitempty"`
	ClosedAt      *time.Time               `json:"closed_at,omitempty"`
	PendingIntent *Intent                  `json:"pending_intent,omitempty"`
}

type SessionMessage struct {
	ID         string           `json:"id"`
	Role       string           `json:"role"`
	Content    string           `json:"content,omitempty"`
	CreatedAt  time.Time        `json:"created_at"`
	Tool       string           `json:"tool,omitempty"`
	ToolCallID string           `json:"tool_call_id,omitempty"`
	Data       map[string]any   `json:"data,omitempty"`
	Provider   *ProviderMessage `json:"provider,omitempty"`
	WorkflowID string           `json:"workflow_id,omitempty"`
	Phase      string           `json:"phase,omitempty"`
}

type PendingAction struct {
	OperationIndex int        `json:"operation_index,omitempty"`
	ID             string     `json:"id"`
	WorkflowID     string     `json:"workflow_id,omitempty"`
	Command        UICommand  `json:"command"`
	Status         string     `json:"status"`
	Fingerprint    string     `json:"fingerprint,omitempty"`
	CreatedAt      time.Time  `json:"created_at"`
	AcknowledgedAt *time.Time `json:"acknowledged_at,omitempty"`
	Ack            *ActionAck `json:"ack,omitempty"`
}

type Workflow struct {
	ID                 string             `json:"id"`
	SessionID          string             `json:"session_id"`
	SkillID            string             `json:"skill_id,omitempty"`
	RequestID          string             `json:"request_id,omitempty"`
	RequestFingerprint string             `json:"request_fingerprint,omitempty"`
	Status             string             `json:"status"`
	Stage              string             `json:"stage,omitempty"`
	StepsUsed          int                `json:"steps_used,omitempty"`
	ToolCalls          []WorkflowToolCall `json:"tool_calls,omitempty"`
	Events             []WorkflowEvent    `json:"events,omitempty"`
	CreatedAt          time.Time          `json:"created_at"`
	UpdatedAt          time.Time          `json:"updated_at"`
	Intent             *Intent            `json:"intent,omitempty"`
	OperationIndex     int                `json:"operation_index,omitempty"`
}

type WorkflowToolCall struct {
	Tool           string         `json:"tool"`
	Version        string         `json:"version"`
	Status         string         `json:"status"`
	RequestID      string         `json:"request_id"`
	Input          map[string]any `json:"input,omitempty"`
	Data           any            `json:"data,omitempty"`
	OperationIndex int            `json:"operation_index,omitempty"`
}
type WorkflowEvent struct {
	ID        int64     `json:"id"`
	Type      string    `json:"type"`
	Data      any       `json:"data,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

type repository struct {
	dir       string
	mu        sync.Mutex
	sessions  map[string]Session
	workflows map[string]Workflow
}

func newRepository(dataDir string) (*repository, error) {
	dir := filepath.Join(dataDir, "ai")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	r := &repository{dir: dir, sessions: make(map[string]Session), workflows: make(map[string]Workflow)}
	if err := r.load("sessions.json", &r.sessions); err != nil {
		return nil, err
	}
	if err := r.load("workflows.json", &r.workflows); err != nil {
		return nil, err
	}
	return r, nil
}

func (r *repository) load(name string, out any) error {
	path := filepath.Join(r.dir, name)
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		// A previous Windows replacement may have completed the backup rename
		// before the process stopped. Recover it on the next startup.
		backup := path + ".bak"
		if recovered, recoverErr := os.ReadFile(backup); recoverErr == nil {
			if renameErr := os.Rename(backup, path); renameErr == nil {
				b = recovered
				err = nil
			}
		}
		if os.IsNotExist(err) {
			return nil
		}
	}
	if err != nil {
		return fmt.Errorf("read ai %s: %w", name, err)
	}
	if len(b) == 0 {
		return nil
	}
	if err := json.Unmarshal(b, out); err != nil {
		return fmt.Errorf("decode ai %s: %w", name, err)
	}
	return nil
}

func (r *repository) save(name string, value any) error {
	b, err := json.Marshal(value)
	if err != nil {
		return err
	}
	tmp := filepath.Join(r.dir, "."+name+".tmp")
	file, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := file.Write(b); err != nil {
		_ = file.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	target := filepath.Join(r.dir, name)
	if err := os.Rename(tmp, target); err != nil {
		// Windows may reject replacing an existing destination. Keep the old
		// file recoverable while retrying the swap.
		backup := target + ".bak"
		_ = os.Remove(backup)
		if backupErr := os.Rename(target, backup); backupErr != nil {
			_ = os.Remove(tmp)
			return err
		}
		if retryErr := os.Rename(tmp, target); retryErr != nil {
			_ = os.Remove(tmp)
			_ = os.Rename(backup, target)
			return retryErr
		}
		_ = os.Remove(backup)
	}
	return nil
}

func (r *repository) upsertSession(s Session) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	// A worker may finish an in-flight write after the close endpoint returns.
	// Preserve the durable close barrier and cancelled UI actions in that case.
	if prior, ok := r.sessions[s.ID]; ok && prior.ClosedAt != nil {
		s.ClosedAt = prior.ClosedAt
		for id, action := range prior.Actions {
			if action.Status == "cancelled" {
				if s.Actions == nil {
					s.Actions = make(map[string]PendingAction)
				}
				s.Actions[id] = action
			}
		}
	}
	prior, existed := r.sessions[s.ID]
	r.sessions[s.ID] = cloneSession(s)
	if err := r.save("sessions.json", r.sessions); err != nil {
		if existed {
			r.sessions[s.ID] = prior
		} else {
			delete(r.sessions, s.ID)
		}
		return err
	}
	return nil
}
func (r *repository) getSession(id string) (Session, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s, ok := r.sessions[id]
	if ok {
		s = cloneSession(s)
	}
	return s, ok
}
func (r *repository) listSessions() []Session {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Session, 0, len(r.sessions))
	for _, s := range r.sessions {
		out = append(out, cloneSession(s))
	}
	return out
}
func (r *repository) upsertWorkflow(w Workflow) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	// Cancellation wins over stale worker snapshots, including final results.
	if prior, ok := r.workflows[w.ID]; ok && prior.Status == "cancelled" {
		return nil
	}
	prior, existed := r.workflows[w.ID]
	r.workflows[w.ID] = cloneWorkflow(w)
	if err := r.save("workflows.json", r.workflows); err != nil {
		if existed {
			r.workflows[w.ID] = prior
		} else {
			delete(r.workflows, w.ID)
		}
		return err
	}
	return nil
}

// cancelChatWorkflow atomically appends the terminal event. Only this method
// writes cancelled workflows; stale workers can never replace the terminal.
func (r *repository) cancelChatWorkflow(id string) (Workflow, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	w, ok := r.workflows[id]
	if !ok {
		return Workflow{}, fmt.Errorf("workflow not found")
	}
	if w.Status == "succeeded" || w.Status == "failed" || w.Status == "cancelled" {
		return cloneWorkflow(w), nil
	}
	prior := cloneWorkflow(w)
	w.Status = "cancelled"
	w.UpdatedAt = time.Now().UTC()
	w.Events = append(w.Events, WorkflowEvent{ID: int64(len(w.Events) + 1), Type: "cancelled", Data: map[string]any{"status": "cancelled", "message": "已停止 AI 编排，已提交的检测任务继续运行。"}, CreatedAt: w.UpdatedAt})
	r.workflows[id] = cloneWorkflow(w)
	if err := r.save("workflows.json", r.workflows); err != nil {
		r.workflows[id] = prior
		return Workflow{}, err
	}
	return cloneWorkflow(w), nil
}
func (r *repository) getWorkflow(id string) (Workflow, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	w, ok := r.workflows[id]
	if ok {
		w = cloneWorkflow(w)
	}
	return w, ok
}

// If storage fails even while reporting failure, keep live subscribers from
// waiting forever. This fallback is explicitly non-durable and reports that
// limitation instead of claiming that a result was saved.
func (r *repository) failWorkflowInMemory(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	w, ok := r.workflows[id]
	if !ok || w.Status == "cancelled" {
		return
	}
	w.Status = "failed"
	w.UpdatedAt = time.Now().UTC()
	w.Events = append(w.Events, WorkflowEvent{ID: int64(len(w.Events) + 1), Type: "error", Data: map[string]any{"code": "AI_PERSISTENCE_FAILED", "message": "AI 状态保存失败，当前过程已停止。请检查服务器存储；已创建检测任务仍可在任务页查看。", "retryable": false}, CreatedAt: w.UpdatedAt})
	r.workflows[id] = cloneWorkflow(w)
}
func (r *repository) listWorkflows() []Workflow {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Workflow, 0, len(r.workflows))
	for _, workflow := range r.workflows {
		out = append(out, cloneWorkflow(workflow))
	}
	return out
}

func cloneSession(input Session) Session {
	var output Session
	data, _ := json.Marshal(input)
	_ = json.Unmarshal(data, &output)
	return output
}
func cloneWorkflow(input Workflow) Workflow {
	var output Workflow
	data, _ := json.Marshal(input)
	_ = json.Unmarshal(data, &output)
	return output
}
