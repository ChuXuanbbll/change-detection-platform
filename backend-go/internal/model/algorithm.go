package model

import "encoding/json"

// Algorithm 描述一个可用算法
type Algorithm struct {
	Name         string                 `json:"name"`
	DisplayName  string                 `json:"display_name,omitempty"`
	Description  string                 `json:"description,omitempty"`
	Tags         []string               `json:"tags,omitempty"`
	ParamsSchema map[string]interface{} `json:"params_schema,omitempty"`
	InputBands   int                    `json:"input_bands,omitempty"`
}

// LocalImageItem 本地裁切返回的每条影像
type LocalImageItem struct {
	DatePrecision string     `json:"date_precision"`
	SourceFile    string     `json:"source_file"`
	SourceFiles   []string   `json:"source_files,omitempty"`
	SceneIDs      []string   `json:"scene_ids,omitempty"`
	Path          string     `json:"path"`
	Date          string     `json:"date"`
	Bounds        [4]float64 `json:"bounds"`
	Width         int        `json:"width"`
	Height        int        `json:"height"`
}

// LocalFetchReq 本地裁切请求体（透传到 Python）
type LocalFetchReq struct {
	AdminCode    string          `json:"admin_code,omitempty"`
	AdminVersion string          `json:"admin_version,omitempty"`
	AOI          json.RawMessage `json:"aoi,omitempty"`
	Bounds       [4]float64      `json:"bounds"`
	StartDate    string          `json:"start_date"`
	EndDate      string          `json:"end_date"`
	MaxImages    int             `json:"max_images"`
	Zoom         int             `json:"zoom"`
	Buffer       int             `json:"buffer"`
	OutputDir    string          `json:"output_dir"`
	RegionID     string          `json:"region_id,omitempty"`
	SceneIDs     []string        `json:"scene_ids,omitempty"`
}

// LocalFetchResp Python 返回
type LocalFetchResp struct {
	AOI    json.RawMessage  `json:"aoi,omitempty"`
	Images []LocalImageItem `json:"images"`
	Count  int              `json:"count"`
}
