package store

import (
	"cdp/backend-go/internal/model"
	"fmt"
	"time"
)

func (d *DB) InsertImages(images []model.Image) error {
	tx, err := d.conn.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, i := range images {
		_, err = tx.Exec(`INSERT INTO images(id,source,phase,capture_date,file_path,thumb_path,crs,bounds_west,bounds_south,bounds_east,bounds_north,width,height,bands,dtype,meta_json,created_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, i.ID, i.Source, i.Phase, nullTime(i.CaptureDate), i.FilePath, i.ThumbPath, i.CRS, i.BoundsWest, i.BoundsSouth, i.BoundsEast, i.BoundsNorth, i.Width, i.Height, i.Bands, i.Dtype, i.MetaJSON, timeTime(i.CreatedAt))
		if err != nil {
			return err
		}
	}
	return tx.Commit()
}
func (d *DB) CompleteTask(r *model.Result) error {
	tx, err := d.conn.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := time.Now().UTC()
	_, err = tx.Exec(`INSERT INTO results(task_id,mask_path,confidence_path,geojson_path,stats_json,created_at) VALUES(?,?,?,?,?,?)`, r.TaskID, r.MaskPath, r.ConfidencePath, r.GeoJSONPath, r.StatsJSON, now)
	if err != nil {
		return err
	}
	updated, err := tx.Exec(`UPDATE tasks SET status='succeeded',progress=100,stage='completed',error_msg=NULL,finished_at=? WHERE id=? AND status='running'`, now, r.TaskID)
	if err != nil {
		return err
	}
	if n, _ := updated.RowsAffected(); n != 1 {
		return fmt.Errorf("task is no longer running")
	}
	return tx.Commit()
}
func (d *DB) InterruptTasks() error {
	_, err := d.conn.Exec(`UPDATE tasks SET status='failed',error_msg='SERVICE_INTERRUPTED: 服务退出或重启，任务未自动重跑',finished_at=? WHERE status IN ('pending','running')`, time.Now().UTC())
	return err
}
