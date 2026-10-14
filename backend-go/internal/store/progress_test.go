package store

import (
	"cdp/backend-go/internal/model"
	"path/filepath"
	"testing"
)

func TestProgressMigrationAndTerminalGuards(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "old.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err = db.Migrate(); err != nil {
		t.Fatal(err)
	}
	if _, err = db.conn.Exec("ALTER TABLE tasks DROP COLUMN stage"); err != nil {
		t.Fatal(err)
	}
	if err = db.InsertTask(&model.Task{ID: "task", Status: "pending"}); err != nil {
		t.Fatal(err)
	}
	if err = db.Migrate(); err != nil {
		t.Fatal(err)
	}
	task, _ := db.GetTask("task")
	if task.Stage != "queued" {
		t.Fatal(task)
	}
	if err = db.UpdateTaskStatus("task", "running", 0, nil); err != nil {
		t.Fatal(err)
	}
	for _, p := range []int{60, 40, 100} {
		if err = db.UpdateTaskStage("task", "inference", p); err != nil {
			t.Fatal(err)
		}
	}
	task, _ = db.GetTask("task")
	if task.Progress != 60 {
		t.Fatal(task)
	}
	message := "GPU failed"
	if err = db.UpdateTaskStatus("task", "failed", -1, &message); err != nil {
		t.Fatal(err)
	}
	_ = db.UpdateTaskStage("task", "saving", 99)
	_ = db.UpdateTaskStatus("task", "running", 0, nil)
	if err = db.CompleteTask(&model.Result{TaskID: "task"}); err == nil {
		t.Fatal("failed task resurrected")
	}
	task, _ = db.GetTask("task")
	if task.Status != "failed" || task.Progress != 60 || task.Stage != "inference" {
		t.Fatal(task)
	}
	if _, err = db.GetResult("task"); err == nil {
		t.Fatal("late result persisted")
	}
}

func TestSaveFailureDoesNotCompleteTask(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err = db.Migrate(); err != nil {
		t.Fatal(err)
	}
	if err = db.InsertTask(&model.Task{ID: "task", Status: "running"}); err != nil {
		t.Fatal(err)
	}
	if err = db.UpdateTaskStage("task", "saving", 99); err != nil {
		t.Fatal(err)
	}
	if _, err = db.conn.Exec(`CREATE TRIGGER fail_result BEFORE INSERT ON results BEGIN SELECT RAISE(FAIL,'disk unavailable'); END`); err != nil {
		t.Fatal(err)
	}
	if err = db.CompleteTask(&model.Result{TaskID: "task"}); err == nil {
		t.Fatal("expected save error")
	}
	task, _ := db.GetTask("task")
	if task.Progress != 99 || task.Status != "running" {
		t.Fatal(task)
	}
	if _, err = db.conn.Exec("DROP TRIGGER fail_result"); err != nil {
		t.Fatal(err)
	}
	if err = db.CompleteTask(&model.Result{TaskID: "task"}); err != nil {
		t.Fatal(err)
	}
	_ = db.UpdateTaskStage("task", "inference", 50)
	task, _ = db.GetTask("task")
	if task.Progress != 100 || task.Stage != "completed" {
		t.Fatal(task)
	}
}
