package ai

import (
	"context"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Legacy execution fixtures select from the actual advertised metadata. The
// dedicated disclosure test below selects a single skill and checks isolation.
func fixtureSkillSelection(request CompletionRequest) CompletionResponse {
	var input struct {
		Skills []struct {
			ID string `json:"skill_id"`
		} `json:"skills"`
	}
	_ = json.Unmarshal([]byte(request.Messages[1].Content), &input)
	ids := []string{}
	for _, skill := range input.Skills {
		ids = append(ids, skill.ID)
	}
	encoded, _ := json.Marshal(map[string]any{"skill_ids": ids})
	return CompletionResponse{Message: ProviderMessage{Role: "assistant", Content: string(encoded)}}
}

type disclosureProvider struct{ requests []CompletionRequest }

func (p *disclosureProvider) Complete(_ context.Context, request CompletionRequest) (CompletionResponse, error) {
	p.requests = append(p.requests, request)
	raw := `{"skill_ids":["change.detect.v1"]}`
	if request.Stage == "intent" {
		raw = `{"operations":[{"skill_id":"change.detect.v1","slots":{"operation":"detect","use_workspace_context":true,"phase_dates":["2025-03","2025-09"]}}],"needs_clarification":false,"question":""}`
	}
	return CompletionResponse{Message: ProviderMessage{Role: "assistant", Content: raw}}, nil
}

func TestSkillPackagesDiscloseMetadataBodyAndReferencesSeparately(t *testing.T) {
	skills, err := loadSkillManifests()
	if err != nil {
		t.Fatal(err)
	}
	p := &disclosureProvider{}
	g := &Gateway{skills: skills, provider: p}
	intent, err := g.parseIntent(context.Background(), &Session{Workspace: map[string]any{"region_id": "r1"}}, MessageRequest{Text: "当前框选检测2025-03和2025-09"})
	if err != nil || intent.NeedsClarification || len(p.requests) != 2 {
		t.Fatalf("intent=%+v err=%v", intent, err)
	}
	first, _ := json.Marshal(p.requests[0])
	second, _ := json.Marshal(p.requests[1])
	for _, skill := range skills {
		if strings.Contains(string(first), "input_schema") || strings.Contains(string(first), strings.Split(skill.Body, "\n")[0]) {
			t.Fatal("discovery exposed instructions or schema")
		}
		for _, reference := range skill.References {
			marker := strings.Split(reference, "\n")[0]
			if strings.Contains(string(first), marker) || strings.Contains(string(second), marker) {
				t.Fatal("reference was eagerly disclosed")
			}
		}
	}
	if !strings.Contains(p.requests[1].Messages[0].Content, "# 变化检测") || strings.Contains(p.requests[1].Messages[0].Content, "# 检测结果") {
		t.Fatal("intent did not isolate selected body")
	}
	for _, path := range []string{"../result-inspection/references/recovery.md", "runtime.yaml", "references/missing.md"} {
		if g.readSkillReference("change.detect.v1", map[string]any{"path": path}).Status != "error" {
			t.Fatal("reference escaped selected package")
		}
	}
	if g.readSkillReference("change.detect.v1", map[string]any{"path": "references/recovery.md"}).Status != "ok" {
		t.Fatal("packaged reference unreadable")
	}
	for _, tool := range skills["change.detect.v1"].AllowedTools {
		if strings.HasPrefix(tool, "detection.") || tool == "imagery.prepare" || tool == "operation.status" {
			t.Fatalf("detection exposes orchestration tool %s", tool)
		}
	}
}

func TestSkillPackagesCanBeUpdatedOutsideTheBinary(t *testing.T) {
	directory := t.TempDir()
	err := fs.WalkDir(skillFiles, "skills", func(file string, item fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if item.IsDir() {
			return nil
		}
		data, err := skillFiles.ReadFile(file)
		if err != nil {
			return err
		}
		target := filepath.Join(directory, filepath.FromSlash(strings.TrimPrefix(file, "skills/")))
		if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
			return err
		}
		return os.WriteFile(target, data, 0644)
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("AI_SKILLS_DIR", directory)
	skills, err := loadSkillManifests()
	if err != nil || len(skills) != 3 {
		t.Fatalf("load=%d %v", len(skills), err)
	}
	file := filepath.Join(directory, "change-detection", "SKILL.md")
	data, _ := os.ReadFile(file)
	if err := os.WriteFile(file, append(data, []byte("\nUpdated instruction.\n")...), 0644); err != nil {
		t.Fatal(err)
	}
	updated, err := loadSkillManifests()
	if err != nil || !strings.Contains(updated["change.detect.v1"].Body, "Updated instruction.") {
		t.Fatalf("updated package not loaded: %v", err)
	}
	if err := os.WriteFile(file, []byte("invalid frontmatter"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := loadSkillManifests(); err == nil {
		t.Fatal("invalid package silently fell back to embedded instructions")
	}
}
