package store

import (
	"cdp/backend-go/internal/model"
	"encoding/json"
	"fmt"
)

func (d *DB) MigrateSettings() error {
	for _, s := range []string{
		`CREATE TABLE IF NOT EXISTS config_revisions(id INTEGER PRIMARY KEY AUTOINCREMENT,document TEXT NOT NULL,state TEXT NOT NULL,created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP)`,
		`CREATE TABLE IF NOT EXISTS managed_models(id TEXT PRIMARY KEY,document TEXT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS config_operations(id TEXT PRIMARY KEY,document TEXT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS config_batches(id TEXT PRIMARY KEY,document TEXT NOT NULL)`,
	} {
		if _, e := d.conn.Exec(s); e != nil {
			return e
		}
	}
	return nil
}
func (d *DB) ActiveRevision() (*model.Revision, error) {
	return d.readRevision(`SELECT id,document,state,created_at FROM config_revisions WHERE state='active' ORDER BY id DESC LIMIT 1`)
}
func (d *DB) Revision(id int64) (*model.Revision, error) {
	return d.readRevision(`SELECT id,document,state,created_at FROM config_revisions WHERE id=?`, id)
}
func (d *DB) readRevision(q string, args ...any) (*model.Revision, error) {
	var r model.Revision
	var s string
	if e := d.conn.QueryRow(q, args...).Scan(&r.ID, &s, &r.State, &r.CreatedAt); e != nil {
		return nil, e
	}
	e := json.Unmarshal([]byte(s), &r.Settings)
	return &r, e
}
func (d *DB) RevisionHistory() ([]model.Revision, error) {
	rows, e := d.conn.Query(`SELECT id,document,state,created_at FROM config_revisions ORDER BY id DESC`)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	out := []model.Revision{}
	for rows.Next() {
		var r model.Revision
		var s string
		if e = rows.Scan(&r.ID, &s, &r.State, &r.CreatedAt); e != nil {
			return nil, e
		}
		if e = json.Unmarshal([]byte(s), &r.Settings); e != nil {
			return nil, e
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
func (d *DB) NewRevision(c model.Settings) (int64, error) {
	b, e := json.Marshal(c)
	if e != nil {
		return 0, e
	}
	r, e := d.conn.Exec(`INSERT INTO config_revisions(document,state)VALUES(?,'pending')`, string(b))
	if e != nil {
		return 0, e
	}
	return r.LastInsertId()
}
func (d *DB) ActivateRevision(id int64) error {
	tx, e := d.conn.Begin()
	if e != nil {
		return e
	}
	defer tx.Rollback()
	if _, e = tx.Exec(`UPDATE config_revisions SET state='archived' WHERE state='active'`); e != nil {
		return e
	}
	r, e := tx.Exec(`UPDATE config_revisions SET state='active' WHERE id=?`, id)
	if e != nil {
		return e
	}
	n, _ := r.RowsAffected()
	if n != 1 {
		return fmt.Errorf("revision missing")
	}
	return tx.Commit()
}
func (d *DB) RecoverSettings() error {
	_, e := d.conn.Exec(`UPDATE config_revisions SET state='abandoned' WHERE state='pending'`)
	if e != nil {
		return e
	}
	ops, e := d.Operations()
	if e != nil {
		return e
	}
	for _, op := range ops {
		if op.Status == "running" {
			op.Status = "failed"
			op.Error = "SERVICE_INTERRUPTED: 管理操作中断，请重新执行"
			if e = d.SaveOperation(op); e != nil {
				return e
			}
		}
	}
	return nil
}
func (d *DB) putDocument(table, id string, v any) error {
	b, e := json.Marshal(v)
	if e != nil {
		return e
	}
	_, e = d.conn.Exec(`INSERT INTO `+table+`(id,document)VALUES(?,?) ON CONFLICT(id) DO UPDATE SET document=excluded.document`, id, string(b))
	return e
}
func (d *DB) getDocument(table, id string, v any) error {
	var b string
	e := d.conn.QueryRow(`SELECT document FROM `+table+` WHERE id=?`, id).Scan(&b)
	if e != nil {
		return e
	}
	return json.Unmarshal([]byte(b), v)
}
func (d *DB) documents(table string) ([]json.RawMessage, error) {
	rows, e := d.conn.Query(`SELECT document FROM ` + table + ` ORDER BY rowid DESC`)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	out := []json.RawMessage{}
	for rows.Next() {
		var s string
		if e = rows.Scan(&s); e != nil {
			return nil, e
		}
		out = append(out, json.RawMessage(s))
	}
	return out, rows.Err()
}
func (d *DB) SaveModel(m model.ManagedModel) error { return d.putDocument("managed_models", m.ID, m) }
func (d *DB) Model(id string) (model.ManagedModel, error) {
	var m model.ManagedModel
	e := d.getDocument("managed_models", id, &m)
	return m, e
}
func (d *DB) Models() ([]model.ManagedModel, error) {
	docs, e := d.documents("managed_models")
	if e != nil {
		return nil, e
	}
	out := []model.ManagedModel{}
	for _, b := range docs {
		var m model.ManagedModel
		if e = json.Unmarshal(b, &m); e != nil {
			return nil, e
		}
		out = append(out, m)
	}
	return out, nil
}
func (d *DB) DeleteModel(id string) error {
	var n int
	if e := d.conn.QueryRow(`SELECT count(*) FROM config_batches WHERE json_extract(document,'$.model.id')=?`, id).Scan(&n); e != nil {
		return e
	}
	if n > 0 {
		return fmt.Errorf("模型已被批次引用，只能停用")
	}
	_, e := d.conn.Exec(`DELETE FROM managed_models WHERE id=?`, id)
	return e
}
func (d *DB) SaveOperation(o model.Operation) error {
	return d.putDocument("config_operations", o.ID, o)
}
func (d *DB) Operation(id string) (model.Operation, error) {
	var o model.Operation
	e := d.getDocument("config_operations", id, &o)
	return o, e
}
func (d *DB) Operations() ([]model.Operation, error) {
	docs, e := d.documents("config_operations")
	if e != nil {
		return nil, e
	}
	out := []model.Operation{}
	for _, b := range docs {
		var o model.Operation
		if e = json.Unmarshal(b, &o); e != nil {
			return nil, e
		}
		out = append(out, o)
	}
	return out, nil
}
func (d *DB) SaveBatch(b model.BatchSnapshot) error { return d.putDocument("config_batches", b.ID, b) }
func (d *DB) Batch(id string) (model.BatchSnapshot, error) {
	var b model.BatchSnapshot
	e := d.getDocument("config_batches", id, &b)
	return b, e
}

func (d *DB) FailRevision(id int64) error {
	_, e := d.conn.Exec(`UPDATE config_revisions SET state='failed' WHERE id=? AND state='pending'`, id)
	return e
}
