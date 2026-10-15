package ai

import (
	"context"
	"testing"
	"time"
)

type testExecutor struct{}

func (testExecutor) Execute(context.Context, Call) Result {
	return Result{Status: "ok", Data: map[string]any{"safe": true, "file_path": "secret", "source_path": "secret", "provider": "cuda", "parameters": map[string]any{"threshold": 0.5}, "credentials": map[string]any{"api_key": "secret", "authorization": "secret", "token": "secret"}, "nullable": map[string]any(nil)}, Refs: []any{map[string]any{"output_dir": "secret", "id": "r1"}}}
}

type diagnosticExecutor struct{}

func (diagnosticExecutor) Execute(context.Context, Call) Result {
	return Result{
		Status:   "error",
		Warnings: []string{"provider returned C:\\workspace\\scene.tif; use retry"},
		Error: &ToolError{
			Code:    "UPSTREAM_FAILED",
			Message: "request failed at C:\\workspace\\scene.tif with token: secret",
			Field:   stringPtr("model_path"),
			RecoverableActions: []string{
				"retry with provider", "inspect C:\\workspace\\scene.tif",
			},
		},
	}
}

func testSpec() ToolSpec {
	return ToolSpec{Name: "region.search", Version: "1.0.0", Description: "search", Effect: "query", InputSchema: map[string]any{"$schema": "https://json-schema.org/draft/2020-12/schema", "type": "object", "additionalProperties": false, "required": []any{"query"}, "properties": map[string]any{"query": map[string]any{"type": "string", "minLength": 1}}}, OutputSchema: map[string]any{"$schema": "https://json-schema.org/draft/2020-12/schema", "type": "object"}}
}

func TestRegistryRejectsUnknownFieldsAndScrubs(t *testing.T) {
	r, err := NewRegistry([]ToolSpec{testSpec()}, testExecutor{})
	if err != nil {
		t.Fatal(err)
	}
	bad := r.Invoke(context.Background(), Call{SchemaVersion: "tool-call.v1", RequestID: "r", Tool: "region.search", ToolVersion: "1.0.0", Input: map[string]any{"query": "x", "evil": true}})
	if bad.Error == nil {
		t.Fatal("unknown field accepted")
	}
	good := r.Invoke(context.Background(), Call{SchemaVersion: "tool-call.v1", RequestID: "r2", Tool: "region.search", ToolVersion: "1.0.0", Input: map[string]any{"query": "x"}})
	data := good.Data.(map[string]any)
	if _, ok := data["file_path"]; ok {
		t.Fatal("path leaked")
	}
	for _, key := range []string{"source_path", "provider", "parameters"} {
		if _, ok := data[key]; ok {
			t.Fatalf("restricted field %s leaked", key)
		}
	}
	credentials := data["credentials"].(map[string]any)
	if len(credentials) != 0 {
		t.Fatalf("credentials leaked: %#v", credentials)
	}
	if nullable, exists := data["nullable"]; !exists || nullable != nil {
		t.Fatalf("nullable field should remain null, got %#v", nullable)
	}
}

func TestRegistryScrubsWarningsAndErrors(t *testing.T) {
	r, err := NewRegistry([]ToolSpec{testSpec()}, diagnosticExecutor{})
	if err != nil {
		t.Fatal(err)
	}
	result := r.Invoke(context.Background(), Call{SchemaVersion: callSchemaVersion, RequestID: "diagnostic", Tool: "region.search", ToolVersion: "1.0.0", Input: map[string]any{"query": "x"}})
	if result.Status != "error" || result.Error == nil {
		t.Fatalf("result = %#v", result)
	}
	if result.Error.Message == "" || result.Error.Message == "request failed at C:\\workspace\\scene.tif with token: secret" {
		t.Fatalf("error message was not redacted: %#v", result.Error)
	}
	if result.Error.Field != nil {
		t.Fatalf("restricted error field leaked: %#v", result.Error.Field)
	}
	if len(result.Warnings) != 0 || len(result.Error.RecoverableActions) != 1 || result.Error.RecoverableActions[0] != "retry with provider" {
		t.Fatalf("diagnostic warning leaked: %#v", result)
	}
}

func TestRegistryRejectsInvalidSpec(t *testing.T) {
	spec := testSpec()
	spec.InputSchema["$schema"] = "draft-07"
	if _, err := NewRegistry([]ToolSpec{spec}, testExecutor{}); err == nil {
		t.Fatal("invalid schema accepted")
	}
}

func TestProviderToolsAreFilteredAndRejectOpenNestedParameters(t *testing.T) {
	base := testSpec()
	other := testSpec()
	other.Name = "task.status"
	registry, err := NewRegistry([]ToolSpec{base, other}, testExecutor{})
	if err != nil {
		t.Fatal(err)
	}
	tools, err := registry.ProviderTools([]string{"task.status"})
	if err != nil {
		t.Fatal(err)
	}
	if len(tools) != 1 || tools[0].Function.Name != "task__status" {
		t.Fatalf("provider tools = %+v", tools)
	}

	bad := testSpec()
	bad.InputSchema["properties"] = map[string]any{"query": map[string]any{"type": "string"}, "filter": map[string]any{"type": "object", "additionalProperties": true}}
	if _, err := NewRegistry([]ToolSpec{bad}, testExecutor{}); err == nil {
		t.Fatal("open nested input parameters were accepted at registration")
	}

	for name, branch := range map[string]map[string]any{
		"items": {"type": "array", "items": map[string]any{"type": "object"}},
		"anyOf": {"anyOf": []any{map[string]any{"type": "object"}}},
		"allOf": {"allOf": []any{map[string]any{"type": "object"}}},
	} {
		nested := testSpec()
		nested.InputSchema["properties"] = map[string]any{"value": branch}
		if _, err := NewRegistry([]ToolSpec{nested}, testExecutor{}); err == nil {
			t.Fatalf("open nested input parameters in %s were accepted", name)
		}
	}
}

func TestEmbeddedSkillsHaveCompleteSchemasAndProviderToolSteps(t *testing.T) {
	skills, err := loadSkillManifests()
	if err != nil {
		t.Fatal(err)
	}
	all := uiToolSpecs()
	added := map[string]bool{}
	for _, spec := range all {
		added[spec.Name] = true
	}
	for _, skill := range skills {
		for _, name := range skill.AllowedTools {
			if added[name] {
				continue
			}
			added[name] = true
			all = append(all, testToolSpecNamed(name))
		}
	}
	registry, err := NewRegistry(all, testExecutor{})
	if err != nil {
		t.Fatal(err)
	}
	for id, skill := range skills {
		providerTools, err := registry.ProviderTools(skill.AllowedTools)
		if err != nil {
			t.Fatalf("%s: %v", id, err)
		}
		if len(providerTools) != len(skill.AllowedTools) {
			t.Fatalf("%s exposed %d of %d tools", id, len(providerTools), len(skill.AllowedTools))
		}
	}
}

func testToolSpecNamed(name string) ToolSpec {
	input := map[string]any{"$schema": "https://json-schema.org/draft/2020-12/schema", "type": "object", "additionalProperties": false}
	output := map[string]any{"$schema": "https://json-schema.org/draft/2020-12/schema", "type": "object"}
	return ToolSpec{Name: name, Version: "1.0.0", Description: name, Effect: "query", MaxOutputSize: 1024, InputSchema: input, OutputSchema: output}
}

type slowExecutor struct{}

func (slowExecutor) Execute(ctx context.Context, _ Call) Result {
	<-ctx.Done()
	return Result{Status: "ok", Data: map[string]any{}}
}

func TestRegistryEnforcesTimeoutAndOutputSchema(t *testing.T) {
	spec := testSpec()
	spec.TimeoutPolicy = map[string]any{"timeout_ms": 5}
	r, err := NewRegistry([]ToolSpec{spec}, slowExecutor{})
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	result := r.Invoke(context.Background(), Call{SchemaVersion: callSchemaVersion, RequestID: "timeout", Tool: spec.Name, ToolVersion: spec.Version, Input: map[string]any{"query": "x"}})
	if result.Error == nil || result.Error.Code != "TOOL_TIMEOUT" {
		t.Fatalf("result = %+v", result)
	}
	if time.Since(started) > time.Second {
		t.Fatal("timeout was not enforced")
	}
	outputSpec := testSpec()
	outputSpec.OutputSchema = map[string]any{"$schema": "https://json-schema.org/draft/2020-12/schema", "type": "string"}
	outRegistry, err := NewRegistry([]ToolSpec{outputSpec}, testExecutor{})
	if err != nil {
		t.Fatal(err)
	}
	output := outRegistry.Invoke(context.Background(), Call{SchemaVersion: callSchemaVersion, RequestID: "output", Tool: outputSpec.Name, ToolVersion: outputSpec.Version, Input: map[string]any{"query": "x"}})
	if output.Error == nil || output.Error.Code != "INVALID_TOOL_OUTPUT" {
		t.Fatalf("output = %+v", output)
	}
}
