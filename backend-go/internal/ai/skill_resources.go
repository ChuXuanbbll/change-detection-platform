package ai

import "github.com/google/uuid"

func skillReferenceTool() ProviderTool {
	return ProviderTool{Type: "function", Function: ProviderFunction{
		Name: "skill__read_reference", Description: "Read one supporting reference linked by the selected SKILL.md, only when needed.",
		Parameters: map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{"path": map[string]any{"type": "string", "minLength": 1, "maxLength": 200}}, "required": []any{"path"}},
	}}
}

func (g *Gateway) readSkillReference(skillID string, input map[string]any) Result {
	result := Result{SchemaVersion: schemaVersion, RequestID: uuid.NewString(), Tool: "skill.read_reference", ToolVersion: "1.0.0", Status: "error"}
	if validateJSONSchema(skillReferenceTool().Function.Parameters, input) == nil {
		if content, ok := g.skills[skillID].References[stringValue(input["path"])]; ok {
			result.Status = "ok"
			result.Data = map[string]any{"path": input["path"], "content": content}
			return result
		}
	}
	result.Error = &ToolError{Code: "SKILL_REFERENCE_NOT_FOUND", Message: "Only packaged references of the selected Skill may be read", Retryable: false}
	return result
}
