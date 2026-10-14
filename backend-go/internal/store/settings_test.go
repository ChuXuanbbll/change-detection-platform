package store

import (
	"cdp/backend-go/internal/model"
	"path/filepath"
	"testing"
)

func TestSettingsRecoveryKeepsCommittedVersionAndHistory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.db")
	db, e := Open(path)
	if e != nil {
		t.Fatal(e)
	}
	if e = db.MigrateSettings(); e != nil {
		t.Fatal(e)
	}
	active, e := db.NewRevision(model.Settings{DefaultModel: "original"})
	if e != nil {
		t.Fatal(e)
	}
	if e = db.ActivateRevision(active); e != nil {
		t.Fatal(e)
	}
	pending, e := db.NewRevision(model.Settings{DefaultModel: "interrupted"})
	if e != nil {
		t.Fatal(e)
	}
	if e = db.SaveOperation(model.Operation{ID: "interrupted", Status: "running"}); e != nil {
		t.Fatal(e)
	}
	db.Close()
	db, e = Open(path)
	if e != nil {
		t.Fatal(e)
	}
	defer db.Close()
	if e = db.RecoverSettings(); e != nil {
		t.Fatal(e)
	}
	r, e := db.ActiveRevision()
	if e != nil || r.ID != active || r.Settings.DefaultModel != "original" {
		t.Fatalf("%+v %v", r, e)
	}
	r, e = db.Revision(pending)
	if e != nil || r.State != "abandoned" {
		t.Fatalf("%+v %v", r, e)
	}
	op, e := db.Operation("interrupted")
	if e != nil || op.Status != "failed" {
		t.Fatalf("%+v %v", op, e)
	}
	if e = db.ActivateRevision(9999); e == nil {
		t.Fatal("invalid revision committed")
	}
	r, e = db.ActiveRevision()
	if e != nil || r.ID != active {
		t.Fatal("failed transaction removed active version")
	}
}
