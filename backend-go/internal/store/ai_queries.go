package store

// Durable documents used by the AI domain. Keeping these records in the same
// SQLite database makes workflow recovery deterministic without exposing the
// underlying file paths to the AI API.

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

type AIDocument struct {
	ID          string
	Kind        string
	Status      string
	RequestID   string
	Fingerprint string
	Document    json.RawMessage
	// Result stores the complete public tool result for idempotent calls. It
	// lets retries reproduce an original error instead of interpreting the
	// persisted resource document as a fresh successful call.
	Result    json.RawMessage
	CreatedAt time.Time
	UpdatedAt time.Time
}

func (d *DB) MigrateAIDomain() error {
	for _, stmt := range []string{
		`CREATE TABLE IF NOT EXISTS ai_documents(id TEXT PRIMARY KEY,kind TEXT NOT NULL,status TEXT NOT NULL,request_id TEXT NOT NULL DEFAULT '',fingerprint TEXT NOT NULL DEFAULT '',document TEXT NOT NULL,result_json TEXT NOT NULL DEFAULT '',created_at TEXT NOT NULL,updated_at TEXT NOT NULL)`,
		`CREATE INDEX IF NOT EXISTS idx_ai_documents_kind ON ai_documents(kind,updated_at)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_ai_documents_request ON ai_documents(kind,request_id) WHERE request_id <> ''`,
	} {
		if _, err := d.conn.Exec(stmt); err != nil {
			return fmt.Errorf("migrate ai documents: %w", err)
		}
	}
	// Existing installations may have the original document-only schema.
	// SQLite has no IF NOT EXISTS form for ADD COLUMN, so tolerate the
	// duplicate-column error while keeping startup migration idempotent.
	if _, err := d.conn.Exec(`ALTER TABLE ai_documents ADD COLUMN result_json TEXT NOT NULL DEFAULT ''`); err != nil && !strings.Contains(strings.ToLower(err.Error()), "duplicate column name") {
		return fmt.Errorf("migrate ai documents result: %w", err)
	}
	return nil
}

func (d *DB) SaveAIDocument(doc AIDocument) error {
	now := time.Now().UTC()
	if doc.CreatedAt.IsZero() {
		doc.CreatedAt = now
	}
	if doc.UpdatedAt.IsZero() {
		doc.UpdatedAt = now
	}
	b, err := json.Marshal(doc.Document)
	if err != nil {
		return err
	}
	_, err = d.conn.Exec(`INSERT INTO ai_documents(id,kind,status,request_id,fingerprint,document,result_json,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?) ON CONFLICT(id) DO UPDATE SET kind=excluded.kind,status=excluded.status,request_id=excluded.request_id,fingerprint=excluded.fingerprint,document=excluded.document,result_json=CASE WHEN excluded.result_json='' THEN ai_documents.result_json ELSE excluded.result_json END,updated_at=excluded.updated_at`, doc.ID, doc.Kind, doc.Status, doc.RequestID, doc.Fingerprint, string(b), string(doc.Result), doc.CreatedAt.Format(time.RFC3339Nano), doc.UpdatedAt.Format(time.RFC3339Nano))
	return err
}

func (d *DB) GetAIDocument(id string) (AIDocument, error) {
	var doc AIDocument
	var raw, created, updated string
	var result string
	err := d.conn.QueryRow(`SELECT id,kind,status,request_id,fingerprint,document,result_json,created_at,updated_at FROM ai_documents WHERE id=?`, id).Scan(&doc.ID, &doc.Kind, &doc.Status, &doc.RequestID, &doc.Fingerprint, &raw, &result, &created, &updated)
	if err != nil {
		return doc, err
	}
	doc.Document = json.RawMessage(raw)
	if result != "" {
		doc.Result = json.RawMessage(result)
	}
	doc.CreatedAt, doc.UpdatedAt = parseAITime(created), parseAITime(updated)
	return doc, nil
}

func (d *DB) FindAIRequest(kind, requestID string) (AIDocument, error) {
	var doc AIDocument
	var raw, created, updated string
	var result string
	err := d.conn.QueryRow(`SELECT id,kind,status,request_id,fingerprint,document,result_json,created_at,updated_at FROM ai_documents WHERE kind=? AND request_id=?`, kind, requestID).Scan(&doc.ID, &doc.Kind, &doc.Status, &doc.RequestID, &doc.Fingerprint, &raw, &result, &created, &updated)
	if err != nil {
		return doc, err
	}
	doc.Document = json.RawMessage(raw)
	if result != "" {
		doc.Result = json.RawMessage(result)
	}
	doc.CreatedAt, doc.UpdatedAt = parseAITime(created), parseAITime(updated)
	return doc, nil
}

func (d *DB) ListAIDocuments(kind string) ([]AIDocument, error) {
	rows, err := d.conn.Query(`SELECT id,kind,status,request_id,fingerprint,document,result_json,created_at,updated_at FROM ai_documents WHERE kind=? ORDER BY updated_at DESC`, kind)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AIDocument
	for rows.Next() {
		var doc AIDocument
		var raw, result, created, updated string
		if err := rows.Scan(&doc.ID, &doc.Kind, &doc.Status, &doc.RequestID, &doc.Fingerprint, &raw, &result, &created, &updated); err != nil {
			return nil, err
		}
		doc.Document = json.RawMessage(raw)
		if result != "" {
			doc.Result = json.RawMessage(result)
		}
		doc.CreatedAt, doc.UpdatedAt = parseAITime(created), parseAITime(updated)
		out = append(out, doc)
	}
	return out, rows.Err()
}

func parseAITime(value string) time.Time {
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02 15:04:05.999999999 -0700 MST"} {
		if parsed, err := time.Parse(layout, value); err == nil {
			return parsed
		}
	}
	return time.Time{}
}

// ListTasksForAI deliberately selects metadata only; it never returns paths.
func (d *DB) ListTasksForAI(status, algorithm string, limit int) ([]AITask, error) {
	return d.ListTasksForAIFilter(AITaskFilter{Status: status, Algorithm: algorithm, Limit: limit})
}

type AITaskFilter struct {
	Status, Algorithm, RegionID, CreatedAfter, CreatedBefore string
	Offset, Limit                                            int
}

func (d *DB) ListTasksForAIFilter(f AITaskFilter) ([]AITask, error) {
	limit := f.Limit
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	q := `SELECT id,before_id,after_id,algorithm,status,progress,stage,error_msg,started_at,finished_at,created_at FROM tasks`
	args := []any{}
	cond := []string{}
	if f.Status != "" {
		cond = append(cond, "status=?")
		args = append(args, f.Status)
	}
	if f.Algorithm != "" {
		cond = append(cond, "algorithm=?")
		args = append(args, f.Algorithm)
	}
	if f.RegionID != "" {
		cond = append(cond, "EXISTS (SELECT 1 FROM images ib WHERE ib.id=tasks.before_id AND json_extract(ib.meta_json,'$.region_id')=?)")
		args = append(args, f.RegionID)
	}
	if f.CreatedAfter != "" {
		cond = append(cond, "created_at>=?")
		args = append(args, f.CreatedAfter)
	}
	if f.CreatedBefore != "" {
		cond = append(cond, "created_at<=?")
		args = append(args, f.CreatedBefore)
	}
	if len(cond) > 0 {
		q += " WHERE " + joinAI(cond, " AND ")
	}
	offset := f.Offset
	if offset < 0 {
		offset = 0
	}
	q += " ORDER BY created_at DESC LIMIT ? OFFSET ?"
	args = append(args, limit, offset)
	rows, err := d.conn.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AITask
	for rows.Next() {
		var t AITask
		var em sql.NullString
		var started, finished sql.NullTime
		if err := rows.Scan(&t.ID, &t.BeforeID, &t.AfterID, &t.Algorithm, &t.Status, &t.Progress, &t.Stage, &em, &started, &finished, &t.CreatedAt); err != nil {
			return nil, err
		}
		if em.Valid {
			t.ErrorMsg = em.String
		}
		if started.Valid {
			t.StartedAt = &started.Time
		}
		if finished.Valid {
			t.FinishedAt = &finished.Time
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

type AITask struct {
	ID, BeforeID, AfterID, Algorithm, Status, Stage, ErrorMsg string
	Progress                                                  int
	StartedAt, FinishedAt                                     *time.Time
	CreatedAt                                                 time.Time
}

func (d *DB) AIConnectionError(err error) bool { return err == sql.ErrNoRows }
func joinAI(parts []string, sep string) string {
	out := ""
	for i, p := range parts {
		if i > 0 {
			out += sep
		}
		out += p
	}
	return out
}
