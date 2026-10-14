package store

import (
	"database/sql"
	"fmt"
	"strings"
	"time"

	"cdp/backend-go/internal/model"

	_ "modernc.org/sqlite"
)

// DB 封装 SQLite 连接
type DB struct {
	conn *sql.DB
}

// Open 打开/创建 SQLite 数据库（纯 Go 驱动）
func Open(path string) (*DB, error) {
	// _pragma=... 优化并发
	dsn := fmt.Sprintf("file:%s?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(ON)", path)
	c, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	c.SetMaxOpenConns(1) // SQLite 写串行；MVP 简单起见
	if err := c.Ping(); err != nil {
		return nil, err
	}
	return &DB{conn: c}, nil
}

// Close 关闭数据库
func (d *DB) Close() error { return d.conn.Close() }

// Migrate 自动建表（三表）
func (d *DB) Migrate() error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS images (
			id TEXT PRIMARY KEY,
			source TEXT NOT NULL,
			phase TEXT NOT NULL,
			capture_date DATETIME,
			file_path TEXT NOT NULL,
			thumb_path TEXT,
			crs TEXT,
			bounds_west REAL,
			bounds_south REAL,
			bounds_east REAL,
			bounds_north REAL,
			width INTEGER,
			height INTEGER,
			bands INTEGER,
			dtype TEXT,
			nodata REAL,
			meta_json TEXT,
			created_at DATETIME NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS tasks (
			id TEXT PRIMARY KEY,
			before_id TEXT NOT NULL,
			after_id TEXT NOT NULL,
			algorithm TEXT NOT NULL,
			params_json TEXT,
			status TEXT NOT NULL,
			progress INTEGER NOT NULL DEFAULT 0,
            stage TEXT NOT NULL DEFAULT 'queued',
			error_msg TEXT,
			started_at DATETIME,
			finished_at DATETIME,
			created_at DATETIME NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS results (
			task_id TEXT PRIMARY KEY,
			mask_path TEXT,
			confidence_path TEXT,
			geojson_path TEXT,
			stats_json TEXT,
			created_at DATETIME NOT NULL,
			FOREIGN KEY(task_id) REFERENCES tasks(id) ON DELETE CASCADE
		)`,
		`CREATE INDEX IF NOT EXISTS idx_images_phase ON images(phase)`,
		`CREATE INDEX IF NOT EXISTS idx_images_source ON images(source)`,
		`CREATE INDEX IF NOT EXISTS idx_images_created_at ON images(created_at)`,
		`CREATE INDEX IF NOT EXISTS idx_tasks_status ON tasks(status)`,
	}
	for _, s := range stmts {
		if _, err := d.conn.Exec(s); err != nil {
			return fmt.Errorf("migrate failed: %w", err)
		}
	}
	// Existing installations need the additive column too.
	rows, err := d.conn.Query("PRAGMA table_info(tasks)")
	if err != nil {
		return err
	}
	found := false
	for rows.Next() {
		var cid, notnull, pk int
		var name, kind string
		var value any
		if err := rows.Scan(&cid, &name, &kind, &notnull, &value, &pk); err != nil {
			rows.Close()
			return err
		}
		if name == "stage" {
			found = true
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	if !found {
		if _, err = d.conn.Exec("ALTER TABLE tasks ADD COLUMN stage TEXT NOT NULL DEFAULT 'queued'"); err != nil {
			return err
		}
		_, err = d.conn.Exec("UPDATE tasks SET stage=CASE WHEN status IN ('succeeded','completed') THEN 'completed' WHEN status='running' THEN 'aligning' ELSE 'queued' END")
	}
	return err
}

// ---------- images ----------

// allowedImageCols 列名白名单（防止 UpdateImage SQL 注入）
var allowedImageCols = map[string]struct{}{
	"source":       {},
	"phase":        {},
	"capture_date": {},
	"file_path":    {},
	"thumb_path":   {},
	"crs":          {},
	"bounds_west":  {},
	"bounds_south": {},
	"bounds_east":  {},
	"bounds_north": {},
	"width":        {},
	"height":       {},
	"bands":        {},
	"dtype":        {},
	"nodata":       {},
	"meta_json":    {},
}

// InsertImage 插入图像
func (d *DB) InsertImage(img *model.Image) error {
	_, err := d.conn.Exec(`INSERT INTO images
		(id,source,phase,capture_date,file_path,thumb_path,crs,bounds_west,bounds_south,bounds_east,bounds_north,width,height,bands,dtype,nodata,meta_json,created_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		img.ID, img.Source, img.Phase, nullTime(img.CaptureDate), img.FilePath, img.ThumbPath,
		img.CRS, img.BoundsWest, img.BoundsSouth, img.BoundsEast, img.BoundsNorth,
		img.Width, img.Height, img.Bands, img.Dtype, img.NoData, img.MetaJSON, timeTime(img.CreatedAt),
	)
	return err
}

// ListImages 列出图像，支持按 source/phase 过滤；按 created_at DESC 排序
func (d *DB) ListImages(limit int, source string, phase string) ([]model.Image, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	q := `SELECT id,source,phase,capture_date,file_path,thumb_path,crs,
		bounds_west,bounds_south,bounds_east,bounds_north,width,height,bands,dtype,nodata,meta_json,created_at
		FROM images`
	var (
		conds []string
		args  []interface{}
	)
	if source != "" {
		conds = append(conds, "source = ?")
		args = append(args, source)
	}
	if phase != "" {
		conds = append(conds, "phase = ?")
		args = append(args, phase)
	}
	if len(conds) > 0 {
		q += " WHERE " + strings.Join(conds, " AND ")
	}
	q += " ORDER BY created_at DESC LIMIT ?"
	args = append(args, limit)

	rows, err := d.conn.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]model.Image, 0)
	for rows.Next() {
		img, err := scanImageRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *img)
	}
	return out, rows.Err()
}

// GetImage 取单张
func (d *DB) GetImage(id string) (*model.Image, error) {
	row := d.conn.QueryRow(`SELECT id,source,phase,capture_date,file_path,thumb_path,crs,
		bounds_west,bounds_south,bounds_east,bounds_north,width,height,bands,dtype,nodata,meta_json,created_at
		FROM images WHERE id=?`, id)
	img, err := scanImageRow(row)
	if err != nil {
		return nil, err
	}
	return img, nil
}

// DeleteImage 按 id 删除一条 image 记录；返回受影响行数与可能的错误
func (d *DB) DeleteImage(id string) (int64, error) {
	res, err := d.conn.Exec(`DELETE FROM images WHERE id = ?`, id)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	return n, nil
}

// UpdateImage 按字段白名单更新 image
func (d *DB) UpdateImage(id string, fields map[string]any) error {
	if len(fields) == 0 {
		return nil
	}
	var (
		setParts []string
		args     []interface{}
	)
	for k, v := range fields {
		if _, ok := allowedImageCols[k]; !ok {
			return fmt.Errorf("disallowed column: %s", k)
		}
		setParts = append(setParts, k+" = ?")
		args = append(args, v)
	}
	args = append(args, id)
	q := "UPDATE images SET " + strings.Join(setParts, ", ") + " WHERE id = ?"
	_, err := d.conn.Exec(q, args...)
	return err
}

// scannable 既支持 *sql.Row 也支持 *sql.Rows
type scannable interface {
	Scan(dest ...interface{}) error
}

func scanImageRow(s scannable) (*model.Image, error) {
	var img model.Image
	var cap sql.NullTime
	var nodata sql.NullFloat64
	var meta sql.NullString
	var thumb sql.NullString
	if err := s.Scan(&img.ID, &img.Source, &img.Phase, &cap, &img.FilePath, &thumb,
		&img.CRS, &img.BoundsWest, &img.BoundsSouth, &img.BoundsEast, &img.BoundsNorth,
		&img.Width, &img.Height, &img.Bands, &img.Dtype, &nodata, &meta, &img.CreatedAt); err != nil {
		return nil, err
	}
	if cap.Valid {
		img.CaptureDate = cap.Time
	}
	if thumb.Valid {
		img.ThumbPath = thumb.String
	}
	if nodata.Valid {
		img.NoData = &nodata.Float64
	}
	if meta.Valid {
		img.MetaJSON = meta.String
	}
	return &img, nil
}

// ---------- tasks ----------

// InsertTask 插入任务
func (d *DB) InsertTask(t *model.Task) error {
	_, err := d.conn.Exec(`INSERT INTO tasks
		(id,before_id,after_id,algorithm,params_json,status,progress,error_msg,started_at,finished_at,created_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
		t.ID, t.BeforeID, t.AfterID, t.Algorithm, t.ParamsJSON, t.Status, t.Progress, t.ErrorMsg,
		nullableTime(t.StartedAt), nullableTime(t.FinishedAt), t.CreatedAt,
	)
	return err
}

// GetTask 取任务
func (d *DB) GetTask(id string) (*model.Task, error) {
	row := d.conn.QueryRow(`SELECT id,before_id,after_id,algorithm,params_json,status,progress,stage,error_msg,started_at,finished_at,created_at
		FROM tasks WHERE id=?`, id)
	var t model.Task
	var started, finished sql.NullTime
	var pj, em sql.NullString
	if err := row.Scan(&t.ID, &t.BeforeID, &t.AfterID, &t.Algorithm, &pj, &t.Status, &t.Progress, &t.Stage, &em, &started, &finished, &t.CreatedAt); err != nil {
		return nil, err
	}
	if pj.Valid {
		t.ParamsJSON = pj.String
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
	return &t, nil
}

// UpdateTaskStatus 更新任务状态/进度/错误
// errMsg 为 nil 表示清除错误（成功状态）
// 若 progress < 0 则不更新 progress；若 status 为空则不更新 status
func (d *DB) UpdateTaskStatus(id, status string, progress int, errMsg *string) error {
	started := time.Now().UTC()
	if status == model.TaskRunning {
		_, err := d.conn.Exec(`UPDATE tasks SET status=?, progress=MAX(progress,?), stage='aligning', error_msg=?, started_at=COALESCE(started_at,?) WHERE id=? AND status IN ('pending','running')`,
			status, progress, nullStr(errMsg), started, id)
		return err
	}
	if status == model.TaskSucceeded || status == model.TaskCompleted || status == model.TaskFailed {
		_, err := d.conn.Exec(`UPDATE tasks SET status=?, progress=MAX(progress,?), error_msg=?, finished_at=? WHERE id=? AND status IN ('pending','running')`,
			status, progress, nullStr(errMsg), started, id)
		return err
	}
	_, err := d.conn.Exec(`UPDATE tasks SET status=CASE WHEN ?='' THEN status ELSE ? END, progress=MAX(progress,?), error_msg=? WHERE id=? AND status IN ('pending','running')`,
		status, status, progress, nullStr(errMsg), id)
	return err
}

// UpdateTaskProgress 仅更新进度（不影响状态/error_msg/时间戳）
func (d *DB) UpdateTaskProgress(id string, progress int) error {
	_, err := d.conn.Exec(`UPDATE tasks SET progress=? WHERE id=? AND status='running' AND progress<=? AND ? BETWEEN 0 AND 99`, progress, id, progress, progress)
	return err
}

// UpdateTaskStage prevents late snapshots from changing terminal tasks.
func (d *DB) UpdateTaskStage(id, stage string, progress int) error {
	_, err := d.conn.Exec(`UPDATE tasks SET stage=?,progress=? WHERE id=? AND status='running' AND progress<=? AND ? BETWEEN 0 AND 99 AND (stage<>? OR progress<>?)`, stage, progress, id, progress, progress, stage, progress)
	return err
}

// InsertResult 插入/覆盖一条结果元信息
func (d *DB) InsertResult(r *model.Result) error {
	created := time.Now().UTC()
	if !r.CreatedAt.IsZero() {
		created = r.CreatedAt
	}
	_, err := d.conn.Exec(`INSERT OR REPLACE INTO results
		(task_id,mask_path,confidence_path,geojson_path,stats_json,created_at)
		VALUES (?,?,?,?,?,?)`,
		r.TaskID, r.MaskPath, r.ConfidencePath, r.GeoJSONPath, r.StatsJSON, created,
	)
	return err
}

// GetResult 取结果元信息
func (d *DB) GetResult(taskID string) (*model.Result, error) {
	row := d.conn.QueryRow(`SELECT task_id,mask_path,confidence_path,geojson_path,stats_json,created_at
		FROM results WHERE task_id=?`, taskID)
	var r model.Result
	var mp, cp, gp, sj sql.NullString
	if err := row.Scan(&r.TaskID, &mp, &cp, &gp, &sj, &r.CreatedAt); err != nil {
		return nil, err
	}
	if mp.Valid {
		r.MaskPath = mp.String
	}
	if cp.Valid {
		r.ConfidencePath = cp.String
	}
	if gp.Valid {
		r.GeoJSONPath = gp.String
	}
	if sj.Valid {
		r.StatsJSON = sj.String
	}
	return &r, nil
}

// helpers
func nullTime(t time.Time) interface{} {
	if t.IsZero() {
		return nil
	}
	return t
}

func timeTime(t time.Time) time.Time {
	if t.IsZero() {
		return time.Now().UTC()
	}
	return t
}

func nullableTime(t *time.Time) interface{} {
	if t == nil {
		return nil
	}
	return *t
}

func nullStr(s *string) interface{} {
	if s == nil {
		return nil
	}
	return *s
}
