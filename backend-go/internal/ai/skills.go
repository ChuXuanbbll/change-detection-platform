package ai

import (
	"embed"
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

//go:embed skills/*.yaml
var skillFiles embed.FS

type SkillManifest struct {
	ID                 string            `yaml:"id" json:"id"`
	Version            string            `yaml:"version" json:"version"`
	Description        string            `yaml:"description" json:"description"`
	InputSchema        map[string]any    `yaml:"input_schema" json:"input_schema"`
	OutputSchema       map[string]any    `yaml:"output_schema" json:"output_schema"`
	AllowedTools       []string          `yaml:"allowed_tools" json:"allowed_tools"`
	MaxSteps           int               `yaml:"max_steps" json:"max_steps"`
	ClarificationRules []string          `yaml:"clarification_rules" json:"clarification_rules"`
	FailureMap         map[string]string `yaml:"failure_map" json:"failure_map"`
	Steps              []SkillStep       `yaml:"steps" json:"steps"`
}

type SkillStep struct {
	ID        string   `yaml:"id" json:"id"`
	Tool      string   `yaml:"tool" json:"tool"`
	OnSuccess string   `yaml:"on_success" json:"on_success"`
	Requires  []string `yaml:"requires" json:"requires"`
}

func loadSkillManifests() (map[string]SkillManifest, error) {
	result := make(map[string]SkillManifest)
	entries, err := skillFiles.ReadDir("skills")
	if err != nil {
		return nil, err
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".yaml") {
			continue
		}
		data, err := skillFiles.ReadFile("skills/" + entry.Name())
		if err != nil {
			return nil, err
		}
		var skill SkillManifest
		if err := yaml.Unmarshal(data, &skill); err != nil {
			return nil, fmt.Errorf("skill %s: %w", entry.Name(), err)
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
		result[skill.ID] = skill
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
	if skill.ClarificationRules == nil || skill.FailureMap == nil {
		return fmt.Errorf("clarification_rules and failure_map are required")
	}
	return nil
}
