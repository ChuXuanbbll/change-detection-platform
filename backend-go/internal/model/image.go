package model

import "time"

// Image 对应 images 表
type Image struct {
	ID          string    `json:"id"`
	Source      string    `json:"source"` // "local"; legacy rows may retain "wayback" or "upload"
	Phase       string    `json:"phase"`  // "before" | "after" | ""
	CaptureDate time.Time `json:"capture_date"`
	FilePath    string    `json:"file_path"`
	ThumbPath   string    `json:"thumb_path"`
	CRS         string    `json:"crs"`
	BoundsWest  float64   `json:"bounds_west"`
	BoundsSouth float64   `json:"bounds_south"`
	BoundsEast  float64   `json:"bounds_east"`
	BoundsNorth float64   `json:"bounds_north"`
	Width       int       `json:"width"`
	Height      int       `json:"height"`
	Bands       int       `json:"bands"`
	Dtype       string    `json:"dtype"`
	NoData      *float64  `json:"nodata,omitempty"`
	MetaJSON    string    `json:"meta_json,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
}
