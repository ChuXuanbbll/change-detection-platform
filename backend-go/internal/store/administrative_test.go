package store

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestAdministrativeSnapshotVersioningAndSourceMembership(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "meta.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	raw, err := os.ReadFile("../../../data/boundaries/hebei-administrative.geojson")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err = db.ImportAdministrativeRegions(raw); err != nil {
			t.Fatal(err)
		}
	}
	for _, test := range []struct {
		source, level string
		count         int
	}{
		{"", "province", 1}, {"", "prefecture", 11}, {"", "county", 167},
		{"xiongan", "county", 3}, {"130600", "county", 20},
		{"130100", "county", 21}, {"130682", "county", 1}, {"130181", "county", 1},
	} {
		rows, err := db.AdministrativeRegions(test.source, test.level)
		if err != nil || len(rows) != test.count {
			t.Fatalf("%+v: count=%d err=%v", test, len(rows), err)
		}
	}
	before, _ := db.AdministrativeRegion("130629", "")
	if before.Properties.Parent != "130600" || before.Properties.SourceID != "xiongan" {
		t.Fatalf("hierarchy conflated with source: %+v", before.Properties)
	}
	var changed map[string]any
	if err = json.Unmarshal(raw, &changed); err != nil {
		t.Fatal(err)
	}
	changed["retrieved_at"] = "new-snapshot"
	updated, _ := json.Marshal(changed)
	if err = db.ImportAdministrativeRegions(updated); err != nil {
		t.Fatal(err)
	}
	after, _ := db.AdministrativeRegion("130629", "")
	if after.Properties.Version == before.Properties.Version {
		t.Fatal("changed snapshot retained the old version")
	}
	frozen, err := db.AdministrativeRegion("130629", before.Properties.Version)
	if err != nil || string(frozen.Geometry) != string(before.Geometry) {
		t.Fatal("frozen boundary cannot be reproduced")
	}
	// A rejected import must leave the active snapshot untouched.
	features := changed["features"].([]any)
	changed["features"] = append(features, features[0])
	bad, _ := json.Marshal(changed)
	if db.ImportAdministrativeRegions(bad) == nil {
		t.Fatal("duplicate code accepted")
	}
	current, _ := db.AdministrativeRegion("130629", "")
	if current.Properties.Version != after.Properties.Version {
		t.Fatal("rejected import changed active data")
	}
	var datasets int
	if err = db.conn.QueryRow("SELECT count(*) FROM administrative_datasets").Scan(&datasets); err != nil || datasets != 2 {
		t.Fatalf("idempotence/version history: %d %v", datasets, err)
	}
}
