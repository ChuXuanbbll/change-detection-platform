package model

import "encoding/json"

type Source struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Path    string `json:"path"`
	Year    *int   `json:"year,omitempty"`
	Enabled bool   `json:"enabled"`
}
type Settings struct {
	Sources      []Source       `json:"sources"`
	DefaultModel string         `json:"default_model"`
	Detection    map[string]any `json:"detection"`
	Runtime      map[string]int `json:"runtime"`
	Tiles        map[string]int `json:"tiles"`
	Browser      map[string]int `json:"browser"`
}
type Revision struct {
	ID        int64    `json:"id"`
	Settings  Settings `json:"settings"`
	State     string   `json:"state"`
	CreatedAt string   `json:"created_at"`
}
type ManagedModel struct {
	ID          string            `json:"id"`
	Name        string            `json:"name"`
	Description string            `json:"description"`
	Path        string            `json:"path"`
	Hash        string            `json:"sha256"`
	Status      string            `json:"status"`
	Enabled     bool              `json:"enabled"`
	Mapping     map[string]string `json:"mapping"`
	Inputs      []any             `json:"inputs"`
	Outputs     []any             `json:"outputs"`
	Error       string            `json:"error"`
}
type Operation struct {
	ID     string          `json:"id"`
	Kind   string          `json:"kind"`
	Status string          `json:"status"`
	Error  string          `json:"error"`
	Result json.RawMessage `json:"result,omitempty"`
}
type BatchSnapshot struct {
	ID       string         `json:"id"`
	Revision int64          `json:"revision"`
	Model    ManagedModel   `json:"model"`
	Params   map[string]any `json:"params"`
}
