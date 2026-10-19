package ai

import (
	"bytes"
	"embed"
	"fmt"
	"io/fs"
	"os"
	"path"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

//go:embed skills prompts
var skillFiles embed.FS

type SkillManifest struct {
	Name         string            `yaml:"-" json:"name"`
	Body         string            `yaml:"-" json:"-"`
	References   map[string]string `yaml:"-" json:"-"`
	ID           string            `yaml:"id" json:"id"`
	Version      string            `yaml:"version" json:"version"`
	Description  string            `yaml:"description" json:"description"`
	InputSchema  map[string]any    `yaml:"input_schema" json:"input_schema"`
	OutputSchema map[string]any    `yaml:"output_schema" json:"output_schema"`
	AllowedTools []string          `yaml:"allowed_tools" json:"allowed_tools"`
	MaxSteps     int               `yaml:"max_steps" json:"max_steps"`
	Steps        []SkillStep       `yaml:"steps" json:"steps"`
}

type SkillStep struct {
	ID        string   `yaml:"id" json:"id"`
	Tool      string   `yaml:"tool" json:"tool"`
	OnSuccess string   `yaml:"on_success" json:"on_success"`
	Requires  []string `yaml:"requires" json:"requires"`
}

func loadSkillManifests() (map[string]SkillManifest, error) {
	if directory := strings.TrimSpace(os.Getenv("AI_SKILLS_DIR")); directory != "" {
		return loadSkillPackages(os.DirFS(directory), ".")
	}
	return loadSkillPackages(skillFiles, "skills")
}

// runtime.yaml is a platform extension (schemas and permissions), not Skill prose.
func loadSkillPackages(source fs.FS, root string) (map[string]SkillManifest, error) {
	result := make(map[string]SkillManifest)
	entries, err := fs.ReadDir(source, root)
	if err != nil {
		return nil, err
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		folder := path.Join(root, entry.Name())
		data, err := fs.ReadFile(source, path.Join(folder, "runtime.yaml"))
		if err != nil {
			return nil, err
		}
		var skill SkillManifest
		decoder := yaml.NewDecoder(bytes.NewReader(data))
		decoder.KnownFields(true)
		if err := decoder.Decode(&skill); err != nil {
			return nil, fmt.Errorf("skill %s: %w", entry.Name(), err)
		}
		document, err := fs.ReadFile(source, path.Join(folder, "SKILL.md"))
		if err != nil {
			return nil, err
		}
		parts := strings.SplitN(strings.ReplaceAll(string(document), "\r\n", "\n"), "---\n", 3)
		if len(parts) != 3 || parts[0] != "" {
			return nil, fmt.Errorf("skill %s: invalid frontmatter", entry.Name())
		}
		var metadata struct {
			Name        string `yaml:"name"`
			Description string `yaml:"description"`
		}
		if err := yaml.Unmarshal([]byte(parts[1]), &metadata); err != nil {
			return nil, err
		}
		if metadata.Name != entry.Name() || strings.TrimSpace(metadata.Description) == "" || strings.TrimSpace(parts[2]) == "" {
			return nil, fmt.Errorf("skill %s: missing name, description or body", entry.Name())
		}
		skill.Name, skill.Description, skill.Body = metadata.Name, metadata.Description, strings.TrimSpace(parts[2])
		skill.References = map[string]string{}
		if err := fs.WalkDir(source, folder, func(file string, item fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			rel := strings.TrimPrefix(file, folder+"/")
			if !item.IsDir() && strings.HasPrefix(rel, "references/") && strings.HasSuffix(rel, ".md") {
				content, err := fs.ReadFile(source, file)
				if err != nil {
					return err
				}
				if len(content) > 32<<10 {
					return fmt.Errorf("reference too large: %s", rel)
				}
				skill.References[rel] = string(content)
			}
			return nil
		}); err != nil {
			return nil, err
		}
		if skill.ID == "" || skill.Version == "" {
			return nil, fmt.Errorf("skill %s missing id/version", entry.Name())
		}
		if err := validateSkillManifest(skill); err != nil {
			return nil, fmt.Errorf("skill %s: %w", entry.Name(), err)
		}
		if skill.MaxSteps <= 0 {
			skill.MaxSteps = 8
		}
		if _, exists := result[skill.ID]; exists {
			return nil, fmt.Errorf("duplicate skill %s", skill.ID)
		}
		result[skill.ID] = skill
	}
	if len(result) == 0 {
		return nil, fmt.Errorf("no Skill packages found in %s", root)
	}
	return result, nil
}

func validateSkillManifest(skill SkillManifest) error {
	if skill.InputSchema == nil || skill.OutputSchema == nil {
		return fmt.Errorf("input_schema and output_schema are required")
	}
	skill.InputSchema = normalizeJSONMap(skill.InputSchema)
	skill.OutputSchema = normalizeJSONMap(skill.OutputSchema)
	if err := validateSchemaDocument(skill.InputSchema, "input_schema"); err != nil {
		return err
	}
	if typ, _ := skill.InputSchema["type"].(string); typ != "object" {
		return fmt.Errorf("input_schema must be an object")
	}
	if additional, ok := skill.InputSchema["additionalProperties"].(bool); !ok || additional {
		return fmt.Errorf("input_schema must set additionalProperties to false")
	}
	if err := validateClosedSchema(skill.InputSchema, "input_schema"); err != nil {
		return err
	}
	if err := validateSchemaDocument(skill.OutputSchema, "output_schema"); err != nil {
		return err
	}
	if err := validateClosedSchema(skill.OutputSchema, "output_schema"); err != nil {
		return err
	}
	if len(skill.AllowedTools) == 0 || len(skill.Steps) == 0 {
		return fmt.Errorf("allowed_tools and steps are required")
	}
	allowed := make(map[string]bool, len(skill.AllowedTools))
	for _, tool := range skill.AllowedTools {
		if strings.TrimSpace(tool) == "" || allowed[tool] {
			return fmt.Errorf("allowed_tools contains duplicate or empty tool")
		}
		allowed[tool] = true
	}
	stepIDs := make(map[string]int, len(skill.Steps))
	stepTools := make(map[string]bool, len(skill.Steps))
	for index, step := range skill.Steps {
		if _, duplicate := stepIDs[step.ID]; strings.TrimSpace(step.ID) == "" || strings.TrimSpace(step.Tool) == "" || duplicate {
			return fmt.Errorf("steps must have unique id and tool")
		}
		if stepTools[step.Tool] {
			return fmt.Errorf("tool %q appears in multiple steps", step.Tool)
		}
		if !allowed[step.Tool] {
			return fmt.Errorf("step %q tool %q is not in allowed_tools", step.ID, step.Tool)
		}
		stepIDs[step.ID] = index
		stepTools[step.Tool] = true
	}
	for index, step := range skill.Steps {
		for _, prerequisite := range step.Requires {
			prerequisiteIndex, ok := stepIDs[prerequisite]
			if !ok {
				return fmt.Errorf("step %q requires unknown step %q", step.ID, prerequisite)
			}
			if prerequisiteIndex >= index {
				return fmt.Errorf("step %q requires non-prior step %q", step.ID, prerequisite)
			}
		}
	}
	for tool := range allowed {
		if !stepTools[tool] {
			return fmt.Errorf("allowed tool %q has no declared step", tool)
		}
	}
	if skill.MaxSteps > 0 && skill.MaxSteps < len(skill.Steps) {
		return fmt.Errorf("max_steps is smaller than the declared step count")
	}
	return nil
}

func promptText(name string) string {
	data, err := skillFiles.ReadFile("prompts/" + name + ".md")
	if err != nil {
		panic(err)
	} // Embedded, fixed application resources.
	return string(data)
}

func (g *Gateway) skillCatalog() []map[string]any {
	ids := make([]string, 0, len(g.skills))
	for id := range g.skills {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := make([]map[string]any, 0, len(ids))
	for _, id := range ids {
		skill := g.skills[id]
		out = append(out, map[string]any{"skill_id": id, "name": skill.Name, "description": skill.Description})
	}
	return out
}
