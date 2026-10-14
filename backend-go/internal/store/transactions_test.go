package store

import (
	"cdp/backend-go/internal/model"
	"path/filepath"
	"testing"
)

func TestCompletionAndAtomicImages(t *testing.T) {
	db, e := Open(filepath.Join(t.TempDir(), "test.db"))
	if e != nil {
		t.Fatal(e)
	}
	defer db.Close()
	if e = db.Migrate(); e != nil {
		t.Fatal(e)
	}
	if e = db.InsertImages([]model.Image{{ID: "same"}, {ID: "same"}}); e == nil {
		t.Fatal("expected duplicate error")
	}
	if _, e = db.GetImage("same"); e == nil {
		t.Fatal("partial commit")
	}
	if e = db.InsertTask(&model.Task{ID: "task", Status: model.TaskRunning}); e != nil {
		t.Fatal(e)
	}
	if e = db.CompleteTask(&model.Result{TaskID: "task"}); e != nil {
		t.Fatal(e)
	}
	task, e := db.GetTask("task")
	if e != nil || task.Status != "succeeded" || task.FinishedAt == nil {
		t.Fatalf("%+v %v", task, e)
	}
}
