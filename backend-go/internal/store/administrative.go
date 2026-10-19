package store

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strconv"
)

// AdministrativeFeature is shared by the map API and the AI domain. SourceID is
// a business imagery group, not an administrative parent.
type AdministrativeFeature struct {
	Type       string          `json:"type"`
	ID         string          `json:"id"`
	Geometry   json.RawMessage `json:"geometry"`
	Properties struct {
		Code     string     `json:"admin_code"`
		Name     string     `json:"name"`
		Level    string     `json:"level"`
		Parent   string     `json:"parent_code"`
		Path     []string   `json:"admin_path"`
		SourceID string     `json:"source_id"`
		Label    [2]float64 `json:"label"`
		Bounds   [4]float64 `json:"bounds"`
		Version  string     `json:"boundary_version"`
		DataDate string     `json:"data_date"`
	} `json:"properties"`
}

func (d *DB) ImportAdministrativeRegions(raw []byte) error {
	var collection struct {
		Type     string                  `json:"type"`
		Dataset  string                  `json:"dataset_id"`
		Source   string                  `json:"source"`
		DataDate string                  `json:"data_date"`
		Features []AdministrativeFeature `json:"features"`
	}
	if err := json.Unmarshal(raw, &collection); err != nil {
		return err
	}
	if collection.Type != "FeatureCollection" || collection.Dataset == "" || collection.Source == "" || len(collection.Features) == 0 {
		return fmt.Errorf("administrative dataset metadata is incomplete")
	}
	version := fmt.Sprintf("%s-%x", collection.Dataset, sha256.Sum256(raw))
	seen := map[string]bool{}
	for _, f := range collection.Features {
		p := f.Properties
		code, codeErr := strconv.Atoi(p.Code)
		var geometry struct {
			Type        string            `json:"type"`
			Coordinates []json.RawMessage `json:"coordinates"`
		}
		if f.Type != "Feature" || len(p.Code) != 6 || codeErr != nil || code < 100000 || p.Code != f.ID || seen[p.Code] || p.Name == "" || len(p.Path) == 0 ||
			(p.Level != "province" && p.Level != "prefecture" && p.Level != "county") ||
			json.Unmarshal(f.Geometry, &geometry) != nil || (geometry.Type != "Polygon" && geometry.Type != "MultiPolygon") || len(geometry.Coordinates) == 0 {
			return fmt.Errorf("invalid administrative feature %q", f.ID)
		}
		seen[p.Code] = true
	}
	for _, f := range collection.Features {
		if f.Properties.Parent != "" && !seen[f.Properties.Parent] {
			return fmt.Errorf("unknown administrative parent")
		}
	}
	tx, err := d.conn.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, statement := range []string{
		`CREATE TABLE IF NOT EXISTS administrative_datasets(id TEXT PRIMARY KEY,document TEXT NOT NULL,active INTEGER NOT NULL DEFAULT 0)`,
		`CREATE TABLE IF NOT EXISTS administrative_regions(dataset_id TEXT NOT NULL,admin_code TEXT NOT NULL,source_id TEXT NOT NULL,level TEXT NOT NULL,document TEXT NOT NULL,PRIMARY KEY(dataset_id,admin_code),FOREIGN KEY(dataset_id) REFERENCES administrative_datasets(id))`,
		`CREATE INDEX IF NOT EXISTS idx_administrative_source ON administrative_regions(dataset_id,source_id,level)`,
	} {
		if _, err = tx.Exec(statement); err != nil {
			return err
		}
	}
	var metadata map[string]json.RawMessage
	if err = json.Unmarshal(raw, &metadata); err != nil {
		return err
	}
	delete(metadata, "features")
	document, _ := json.Marshal(metadata)
	if _, err = tx.Exec(`INSERT OR IGNORE INTO administrative_datasets(id,document) VALUES(?,?)`, version, string(document)); err != nil {
		return err
	}
	for _, f := range collection.Features {
		f.Properties.Version = version
		f.Properties.DataDate = collection.DataDate
		document, _ = json.Marshal(f)
		if _, err = tx.Exec(`INSERT OR IGNORE INTO administrative_regions(dataset_id,admin_code,source_id,level,document) VALUES(?,?,?,?,?)`, version, f.ID, f.Properties.SourceID, f.Properties.Level, string(document)); err != nil {
			return err
		}
	}
	if _, err = tx.Exec(`UPDATE administrative_datasets SET active=CASE WHEN id=? THEN 1 ELSE 0 END`, version); err != nil {
		return err
	}
	return tx.Commit()
}

func (d *DB) AdministrativeRegions(sourceID, level string) ([]AdministrativeFeature, error) {
	rows, err := d.conn.Query(`SELECT r.document FROM administrative_regions r JOIN administrative_datasets d ON r.dataset_id=d.id AND d.active=1 WHERE (?='' OR r.source_id=?) AND (?='' OR r.level=?) ORDER BY r.admin_code`, sourceID, sourceID, level, level)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []AdministrativeFeature{}
	for rows.Next() {
		var raw string
		var f AdministrativeFeature
		if err = rows.Scan(&raw); err != nil {
			return nil, err
		}
		if err = json.Unmarshal([]byte(raw), &f); err != nil {
			return nil, err
		}
		result = append(result, f)
	}
	return result, rows.Err()
}

func (d *DB) AdministrativeRegion(code, version string) (*AdministrativeFeature, error) {
	var raw string
	err := d.conn.QueryRow(`SELECT r.document FROM administrative_regions r JOIN administrative_datasets d ON r.dataset_id=d.id WHERE r.admin_code=? AND ((?='' AND d.active=1) OR r.dataset_id=?)`, code, version, version).Scan(&raw)
	if err != nil {
		return nil, err
	}
	var f AdministrativeFeature
	err = json.Unmarshal([]byte(raw), &f)
	return &f, err
}
