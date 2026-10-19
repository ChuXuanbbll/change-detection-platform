package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"cdp/backend-go/internal/client"
	"cdp/backend-go/internal/model"
	"cdp/backend-go/internal/store"
)

func importAdministrativeFixture(t *testing.T, db *store.DB) {
	t.Helper()
	raw, err := os.ReadFile("../../../data/boundaries/hebei-administrative.geojson")
	if err != nil {
		t.Fatal(err)
	}
	if err = db.ImportAdministrativeRegions(raw); err != nil {
		t.Fatal(err)
	}
}

func TestCountySearchUsesKnowledgeAndKeepsBusinessSourceSeparate(t *testing.T) {
	domain, registry, _ := newDomainFixture(t)
	importAdministrativeFixture(t, domain.d.DB)
	for _, input := range []map[string]any{
		{"query": "容城县"}, {"region_id": "130629"},
		{"admin_path": []any{"河北省", "保定市", "容城县"}},
		{"region_id": "xiongan", "admin_path": []any{"河北省", "雄安新区", "容城县"}},
	} {
		result := registry.Invoke(context.Background(), domainCall("region.search", "county", input))
		if result.Status != "ok" {
			t.Fatalf("%+v: %+v", input, result)
		}
		raw, _ := json.Marshal(result.Data)
		var data struct {
			Matches []struct {
				Code    string `json:"admin_code"`
				Source  string `json:"source_id"`
				Version string `json:"boundary_version"`
			}
			Count int
		}
		if err := json.Unmarshal(raw, &data); err != nil || data.Count != 1 || data.Matches[0].Code != "130629" || data.Matches[0].Source != "xiongan" || data.Matches[0].Version == "" {
			t.Fatalf("%s %v", raw, err)
		}
	}
	// County knowledge remains queryable when raster services are unavailable.
	domain.d.Py = nil
	if _, _, err := domain.regionSearch(context.Background(), map[string]any{"query": "容城县"}); err != nil {
		t.Fatal(err)
	}
}

func TestCountyFetchFreezesStoredVersionAndRejectsConflictingGeometry(t *testing.T) {
	root := t.TempDir()
	db, err := store.Open(filepath.Join(root, "meta.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err = db.Migrate(); err != nil {
		t.Fatal(err)
	}
	importAdministrativeFixture(t, db)
	feature, _ := db.AdministrativeRegion("130629", "")
	calls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		calls++
		var request model.LocalFetchReq
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
		}
		var expected, actual any
		_ = json.Unmarshal(feature.Geometry, &expected)
		_ = json.Unmarshal(request.AOI, &actual)
		if request.AdminCode != feature.ID || request.AdminVersion != feature.Properties.Version || request.Bounds != feature.Properties.Bounds || !reflect.DeepEqual(expected, actual) {
			t.Errorf("county reference/geometry lost: %+v", request)
		}
		_ = json.NewEncoder(w).Encode(model.LocalFetchResp{AOI: request.AOI, Count: 1, Images: []model.LocalImageItem{{Path: filepath.Join(request.OutputDir, "county.tif"), Date: "2025-01-01", Bounds: feature.Properties.Bounds, Width: 80, Height: 80}}})
	}))
	defer upstream.Close()
	deps := &Deps{DB: db, Cfg: &Config{DataDir: root}, Py: client.New(upstream.URL, time.Second, 1)}
	request := localFetchReq{Bounds: [4]float64{114, 38, 115, 39}, RegionID: "xiongan", SceneIDs: json.RawMessage(`["a","b"]`), AdminCode: "130629"}
	prepared, err := LocalFetchService(context.Background(), deps, request)
	if err != nil {
		t.Fatal(err)
	}
	if prepared.Count != 1 {
		t.Fatalf("county image not saved: %+v", prepared)
	}
	image, err := db.GetImage(prepared.Items[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	var metadata map[string]any
	if err = json.Unmarshal([]byte(image.MetaJSON), &metadata); err != nil {
		t.Fatal(err)
	}
	var expectedGeometry any
	_ = json.Unmarshal(feature.Geometry, &expectedGeometry)
	if metadata["admin_code"] != feature.ID || metadata["admin_version"] != feature.Properties.Version || !reflect.DeepEqual(metadata["aoi"], expectedGeometry) {
		t.Fatal("derived image lost county provenance")
	}
	if calls != 1 {
		t.Fatalf("calls=%d", calls)
	}
	for _, invalid := range []localFetchReq{
		{Bounds: request.Bounds, RegionID: "130600", SceneIDs: request.SceneIDs, AdminCode: request.AdminCode},
		{Bounds: request.Bounds, RegionID: "xiongan", SceneIDs: request.SceneIDs, AdminCode: request.AdminCode, AdminVersion: "unknown"},
		{Bounds: request.Bounds, RegionID: "xiongan", SceneIDs: request.SceneIDs, AdminCode: request.AdminCode, AOI: json.RawMessage(`{"type":"Polygon","coordinates":[[[114,38],[115,38],[115,39],[114,38]]]}`)},
	} {
		if _, err = LocalFetchService(context.Background(), deps, invalid); err == nil {
			t.Fatal("conflicting county selection accepted")
		}
	}
	if calls != 1 {
		t.Fatal("invalid selection reached crop service")
	}
}
