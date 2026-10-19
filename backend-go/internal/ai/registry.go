package ai

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strings"
	"time"
)

const (
	schemaVersion     = "tool-result.v1"
	callSchemaVersion = "tool-call.v1"
)

var (
	camelBoundary  = regexp.MustCompile(`([a-z0-9])([A-Z])`)
	restrictedText = regexp.MustCompile(`(?i)(?:[a-z]:\\|\\\\|/(?:data|home|tmp|var|mnt|opt|usr|root|workspace)/|(?:^|[\s"'])[^\s"']+\.(?:tif{1,2}|onnx|geojson|sqlite|db|shp|zip)(?:$|[\s"'])|\b(?:api[_-]?key|authorization|token|password|secret)\s*[:=]|\bbearer\s+\S+|\b(?:select|insert|update|delete)\s+.{1,64}\s+from\b|(?:^|\s)(?:cmd(?:\.exe)?|powershell|pwsh|bash|sh)\b)`)
)

type Registry struct {
	tools    map[string]ToolSpec
	executor Executor
	retry    retryTiming
}

func NewRegistry(specs []ToolSpec, executor Executor) (*Registry, error) {
	r := &Registry{tools: make(map[string]ToolSpec), executor: executor}
	for i, spec := range specs {
		// Normalize schemas supplied by Go callers so numeric and array values
		// behave identically to schemas decoded from JSON.
		spec.InputSchema = normalizeJSONMap(spec.InputSchema)
		spec.OutputSchema = normalizeJSONMap(spec.OutputSchema)
		if err := ValidateToolSpec(spec); err != nil {
			return nil, fmt.Errorf("tool[%d] %q: %w", i, spec.Name, err)
		}
		key := toolKey(spec.Name, spec.Version)
		if _, exists := r.tools[key]; exists {
			return nil, fmt.Errorf("duplicate tool %s", key)
		}
		r.tools[key] = cloneToolSpec(spec)
	}
	return r, nil
}

func (r *Registry) Specs() []ToolSpec {
	result := make([]ToolSpec, 0, len(r.tools))
	for _, spec := range r.tools {
		result = append(result, cloneToolSpec(spec))
	}
	return result
}

func (r *Registry) Lookup(name, version string) (ToolSpec, bool) {
	spec, ok := r.tools[toolKey(name, version)]
	return cloneToolSpec(spec), ok
}

// ProviderTools returns only tools authorized by the selected Skill and
// validates the exact parameter documents that will cross the provider
// boundary. Tool outputs remain validated locally by Invoke.
func (r *Registry) ProviderTools(allowed []string) ([]ProviderTool, error) {
	allowedSet := make(map[string]struct{}, len(allowed))
	for _, name := range allowed {
		allowedSet[name] = struct{}{}
	}
	specs := r.Specs()
	sort.Slice(specs, func(i, j int) bool {
		if specs[i].Name == specs[j].Name {
			return specs[i].Version < specs[j].Version
		}
		return specs[i].Name < specs[j].Name
	})
	result := make([]ProviderTool, 0, len(specs))
	names := make(map[string]string)
	for _, spec := range specs {
		if _, ok := allowedSet[spec.Name]; !ok {
			continue
		}
		parameters := cloneMap(spec.InputSchema)
		if err := validateProviderParameters(parameters); err != nil {
			return nil, fmt.Errorf("provider parameters for %s@%s: %w", spec.Name, spec.Version, err)
		}
		name := providerToolName(spec.Name)
		if previous, exists := names[name]; exists {
			return nil, fmt.Errorf("provider tool name collision between %s and %s", previous, spec.Name)
		}
		names[name] = spec.Name
		result = append(result, ProviderTool{Type: "function", Function: ProviderFunction{Name: name, Description: spec.Description, Parameters: parameters}})
	}
	return result, nil
}

func validateProviderParameters(schema map[string]any) error {
	if err := validateSchemaDocument(schema, "parameters"); err != nil {
		return err
	}
	if schema["type"] != "object" {
		return fmt.Errorf("parameters must be an object schema")
	}
	return validateClosedSchema(schema, "parameters")
}

func validateClosedSchema(schema map[string]any, path string) error {
	if typ, _ := schema["type"].(string); typ == "object" {
		additional, ok := schema["additionalProperties"].(bool)
		if !ok || additional {
			return fmt.Errorf("%s must set additionalProperties to false", path)
		}
	}
	if properties, ok := schema["properties"].(map[string]any); ok {
		for name, raw := range properties {
			child, ok := raw.(map[string]any)
			if !ok {
				return fmt.Errorf("%s.properties.%s must be an object", path, name)
			}
			if err := validateClosedSchema(child, path+".properties."+name); err != nil {
				return err
			}
		}
	}
	if items, ok := schema["items"].(map[string]any); ok {
		if err := validateClosedSchema(items, path+".items"); err != nil {
			return err
		}
	}
	for _, keyword := range []string{"anyOf", "allOf"} {
		if branches, ok := schema[keyword].([]any); ok {
			for index, raw := range branches {
				branch, ok := raw.(map[string]any)
				if !ok {
					return fmt.Errorf("%s.%s[%d] must be an object", path, keyword, index)
				}
				if err := validateClosedSchema(branch, fmt.Sprintf("%s.%s[%d]", path, keyword, index)); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func (r *Registry) Invoke(ctx context.Context, call Call) Result {
	result := Result{SchemaVersion: schemaVersion, RequestID: call.RequestID, Tool: call.Tool, ToolVersion: call.ToolVersion, Status: "error"}
	spec, ok := r.Lookup(call.Tool, call.ToolVersion)
	if !ok {
		result.Error = &ToolError{Code: "TOOL_NOT_FOUND", Message: "tool or version is not registered", Retryable: false}
		return result
	}
	if call.SchemaVersion == "" {
		call.SchemaVersion = callSchemaVersion
	}
	if call.SchemaVersion != callSchemaVersion {
		result.Error = &ToolError{Code: "INVALID_SCHEMA_VERSION", Message: "unsupported tool call schema version", Retryable: false, Field: stringPtr("schema_version")}
		return result
	}
	if call.RequestID == "" {
		result.Error = &ToolError{Code: "INVALID_INPUT", Message: "request_id is required", Retryable: false, Field: stringPtr("request_id")}
		return result
	}
	if err := validateJSONSchema(spec.InputSchema, call.Input); err != nil {
		result.Error = &ToolError{Code: "INVALID_INPUT", Message: err.Error(), Retryable: false}
		return result
	}
	if r.executor == nil {
		result.Error = &ToolError{Code: "TOOL_EXECUTOR_UNAVAILABLE", Message: "tool executor is not configured", Retryable: true}
		return result
	}
	safeRetry, _ := spec.RetryPolicy["safe"].(bool)
	for attempt := 1; attempt <= maxRetryAttempts; attempt++ {
		if ctx.Err() != nil {
			result = cancelledToolResult(call)
			break
		}
		callCtx, cancel := context.WithCancel(ctx)
		timeout := timeoutFromPolicy(spec.TimeoutPolicy)
		if timeout <= 0 && spec.Effect == "query" {
			timeout = 45 * time.Second
		}
		if timeout > 0 {
			cancel()
			callCtx, cancel = context.WithTimeout(ctx, timeout)
		}
		result = r.executor.Execute(callCtx, call)
		callErr := callCtx.Err()
		cancel()
		if ctx.Err() != nil {
			result = cancelledToolResult(call)
		} else if callErr != nil {
			result = Result{SchemaVersion: schemaVersion, RequestID: call.RequestID, Tool: call.Tool, ToolVersion: call.ToolVersion, Status: "error", Error: &ToolError{Code: "TOOL_TIMEOUT", Message: "工具执行超时，请稍后重试。", Retryable: true}}
		}
		if attempt == maxRetryAttempts || spec.Effect != "query" || !safeRetry || result.Status != "error" || result.Error == nil || !result.Error.Retryable || ctx.Err() != nil {
			break
		}
		delay := r.retry.delay(attempt)
		observeRetry(ctx, "tool."+call.Tool, attempt+1, delay)
		if err := r.retry.sleep(ctx, delay); err != nil {
			result = cancelledToolResult(call)
			break
		}
	}
	// Domain executors are allowed to return typed Go slices/maps. Normalize
	// them through JSON before applying the JSON-Schema contract so []string,
	// []float64 and map[string]int behave exactly like provider JSON values.
	result.Data = scrubExternal(normalizeJSONValue(result.Data))
	result.Refs = scrubExternalRefs(normalizeJSONValues(result.Refs))
	result.Warnings = scrubWarnings(result.Warnings)
	result.Error = scrubToolError(result.Error)
	if result.SchemaVersion == "" {
		result.SchemaVersion = schemaVersion
	}
	if result.RequestID == "" {
		result.RequestID = call.RequestID
	}
	if result.Tool == "" {
		result.Tool = call.Tool
	}
	if result.ToolVersion == "" {
		result.ToolVersion = call.ToolVersion
	}
	if result.Status == "" {
		result.Status = "ok"
	}
	if result.Status == "ok" {
		if err := validateJSONSchema(spec.OutputSchema, result.Data); err != nil {
			result.Status = "error"
			result.Data, result.Refs = nil, nil
			result.Error = &ToolError{Code: "INVALID_TOOL_OUTPUT", Message: "tool output does not satisfy its declared schema: " + err.Error(), Retryable: false}
		}
	}
	if spec.MaxOutputSize > 0 {
		if encoded, err := json.Marshal(result); err == nil && len(encoded) > spec.MaxOutputSize {
			result.Status = "error"
			result.Data, result.Refs, result.Warnings = nil, nil, nil
			result.Error = &ToolError{Code: "OUTPUT_TOO_LARGE", Message: "tool output exceeds max_output_size", Retryable: false}
		}
	}
	return result
}

func cancelledToolResult(call Call) Result {
	return Result{SchemaVersion: schemaVersion, RequestID: call.RequestID, Tool: call.Tool, ToolVersion: call.ToolVersion, Status: "error", Error: &ToolError{Code: "TOOL_CANCELLED", Message: "已停止工具执行。", Retryable: false}}
}

func timeoutFromPolicy(policy map[string]any) time.Duration {
	if policy == nil {
		return 0
	}
	for _, key := range []string{"timeout_ms", "deadline_ms"} {
		if value, ok := number(policy[key]); ok && value > 0 {
			return time.Duration(value) * time.Millisecond
		}
	}
	return 0
}

func scrubExternal(value any) any {
	switch value := value.(type) {
	case map[string]any:
		if value == nil {
			return nil
		}
		out := make(map[string]any, len(value))
		for key, item := range value {
			if IsRestrictedOutputKey(key) {
				continue
			}
			out[key] = scrubExternal(item)
		}
		return out
	case []any:
		if value == nil {
			return nil
		}
		out := make([]any, len(value))
		for i, item := range value {
			out[i] = scrubExternal(item)
		}
		return out
	case string:
		if restrictedText.MatchString(value) {
			return "[redacted]"
		}
		return value
	default:
		return value
	}
}

// IsRestrictedOutputKey identifies fields that must never cross the Tool API
// boundary, independent of the executor's naming convention.
func IsRestrictedOutputKey(key string) bool {
	if strings.EqualFold(strings.TrimSpace(key), "admin_path") {
		return false
	}
	canonical := strings.ToLower(camelBoundary.ReplaceAllString(strings.TrimSpace(key), "$1_$2"))
	canonical = regexp.MustCompile(`[^a-z0-9]+`).ReplaceAllString(canonical, "_")
	canonical = strings.Trim(canonical, "_")
	compact := strings.ReplaceAll(canonical, "_", "")
	for _, name := range []string{
		"path", "file", "filename", "sourcefile", "sourcefiles", "sourcefilename",
		"outputdir", "sourceroot", "modelpath", "filepath", "token", "controltoken",
		"apikey", "authorization", "authheader", "password", "secret", "sql", "shell",
		"traceback", "provider", "params", "parameters",
	} {
		if compact == name {
			return true
		}
	}
	return strings.HasSuffix(compact, "path") || strings.HasSuffix(compact, "file")
}

func scrubWarnings(warnings []string) []string {
	if warnings == nil {
		return nil
	}
	out := make([]string, 0, len(warnings))
	for _, warning := range warnings {
		if restrictedText.MatchString(warning) {
			continue
		}
		out = append(out, warning)
	}
	return out
}

func scrubToolError(value *ToolError) *ToolError {
	if value == nil {
		return nil
	}
	copy := *value
	if restrictedText.MatchString(copy.Message) {
		copy.Message = "Tool failed; diagnostic details were redacted"
	}
	if copy.Field != nil && IsRestrictedOutputKey(*copy.Field) {
		copy.Field = nil
	}
	copy.RecoverableActions = scrubWarnings(copy.RecoverableActions)
	return &copy
}
func scrubExternalRefs(values []any) []any {
	if values == nil {
		return nil
	}
	out := make([]any, 0, len(values))
	for _, v := range values {
		out = append(out, scrubExternal(v))
	}
	return out
}

func normalizeJSONValue(value any) any {
	if value == nil {
		return nil
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return value
	}
	var normalized any
	if json.Unmarshal(encoded, &normalized) != nil {
		return value
	}
	return normalized
}

func normalizeJSONValues(values []any) []any {
	if values == nil {
		return nil
	}
	out := make([]any, len(values))
	for i, value := range values {
		out[i] = normalizeJSONValue(value)
	}
	return out
}

func ValidateToolSpec(spec ToolSpec) error {
	if strings.TrimSpace(spec.Name) == "" || strings.TrimSpace(spec.Version) == "" {
		return fmt.Errorf("name and version are required")
	}
	if !regexp.MustCompile(`^[a-z][a-z0-9_.-]*$`).MatchString(spec.Name) {
		return fmt.Errorf("name must contain lowercase letters, digits, dot, underscore, or hyphen")
	}
	if strings.TrimSpace(spec.Description) == "" {
		return fmt.Errorf("description is required")
	}
	switch spec.Effect {
	case "query", "workspace", "prepare", "compute", "artifact":
	default:
		return fmt.Errorf("invalid effect %q", spec.Effect)
	}
	if err := validateSchemaDocument(spec.InputSchema, "input_schema"); err != nil {
		return err
	}
	if additional, ok := spec.InputSchema["additionalProperties"].(bool); !ok || additional {
		return fmt.Errorf("input_schema must set additionalProperties to false")
	}
	// Tool calls are accepted directly by Registry.Invoke as well as through
	// ProviderTools. Enforce the closed-object contract at registration time so
	// nested objects cannot bypass it when a caller invokes the registry locally.
	if err := validateClosedSchema(spec.InputSchema, "input_schema"); err != nil {
		return err
	}
	if err := validateSchemaDocument(spec.OutputSchema, "output_schema"); err != nil {
		return err
	}
	if spec.MaxOutputSize < 0 || spec.MaxOutputSize > 1<<20 {
		return fmt.Errorf("max_output_size must be between 0 and 1048576 bytes")
	}
	return nil
}

func validateSchemaDocument(schema map[string]any, field string) error {
	if schema == nil {
		return fmt.Errorf("%s is required", field)
	}
	version, ok := schema["$schema"].(string)
	if !ok || version != "https://json-schema.org/draft/2020-12/schema" {
		return fmt.Errorf("%s must use JSON Schema Draft 2020-12", field)
	}
	if err := validateSchemaNode(schema, field); err != nil {
		return err
	}
	return nil
}

func validateSchemaNode(schema map[string]any, path string) error {
	knownKeywords := map[string]bool{"$schema": true, "$id": true, "title": true, "description": true, "type": true, "properties": true, "required": true, "additionalProperties": true, "items": true, "enum": true, "anyOf": true, "allOf": true, "minLength": true, "maxLength": true, "pattern": true, "minimum": true, "maximum": true, "minItems": true, "maxItems": true}
	for key := range schema {
		if !knownKeywords[key] {
			return fmt.Errorf("%s uses unsupported JSON Schema keyword %q", path, key)
		}
	}
	if typ, ok := schema["type"].(string); ok {
		switch typ {
		case "object", "array", "string", "number", "integer", "boolean", "null":
		default:
			return fmt.Errorf("%s has invalid type", path)
		}
	}
	if enum, ok := schema["enum"].([]any); ok && len(enum) == 0 {
		return fmt.Errorf("%s enum cannot be empty", path)
	}
	if rawRequired, hasRequired := schema["required"]; hasRequired {
		required, ok := schemaStrings(rawRequired)
		if !ok {
			return fmt.Errorf("%s required values must be strings", path)
		}
		for _, name := range required {
			if properties, hasProperties := schema["properties"].(map[string]any); hasProperties {
				if _, exists := properties[name]; !exists {
					return fmt.Errorf("%s required field %q is not declared", path, name)
				}
			}
		}
	}
	if properties, ok := schema["properties"].(map[string]any); ok {
		for name, raw := range properties {
			child, ok := raw.(map[string]any)
			if !ok {
				return fmt.Errorf("%s.properties.%s must be an object", path, name)
			}
			if err := validateSchemaNode(child, path+".properties."+name); err != nil {
				return err
			}
		}
	}
	if additional, exists := schema["additionalProperties"]; exists {
		if _, ok := additional.(bool); !ok {
			return fmt.Errorf("%s additionalProperties must be boolean", path)
		}
	}
	if items, ok := schema["items"].(map[string]any); ok {
		if err := validateSchemaNode(items, path+".items"); err != nil {
			return err
		}
	}
	return nil
}

func validateJSONSchema(schema map[string]any, value any) error {
	if schema == nil {
		return nil
	}
	if allOf, ok := schema["allOf"].([]any); ok {
		for _, item := range allOf {
			m, ok := item.(map[string]any)
			if !ok {
				return fmt.Errorf("invalid allOf schema")
			}
			if err := validateJSONSchema(m, value); err != nil {
				return err
			}
		}
	}
	if anyOf, ok := schema["anyOf"].([]any); ok && len(anyOf) > 0 {
		var last error
		for _, item := range anyOf {
			m, ok := item.(map[string]any)
			if !ok {
				continue
			}
			if err := validateJSONSchema(m, value); err == nil {
				last = nil
				break
			} else {
				last = err
			}
		}
		if last != nil {
			return fmt.Errorf("value matches none of anyOf schemas")
		}
	}
	if enum, ok := schema["enum"].([]any); ok {
		matched := false
		for _, candidate := range enum {
			if sameJSON(candidate, value) {
				matched = true
				break
			}
		}
		if !matched {
			return fmt.Errorf("value is not in enum")
		}
	}
	if typ, ok := schema["type"].(string); ok && !matchesType(typ, value) {
		return fmt.Errorf("expected %s", typ)
	}
	switch typed := value.(type) {
	case map[string]any:
		if rawRequired, hasRequired := schema["required"]; hasRequired {
			required, ok := schemaStrings(rawRequired)
			if !ok {
				return fmt.Errorf("required must be an array of strings")
			}
			for _, key := range required {
				if _, present := typed[key]; !present {
					return fmt.Errorf("missing required field %q", key)
				}
			}
		}
		if properties, ok := schema["properties"].(map[string]any); ok {
			for key, raw := range typed {
				child, exists := properties[key]
				if !exists {
					if additional, has := schema["additionalProperties"].(bool); has && !additional {
						return fmt.Errorf("unknown field %q", key)
					}
					continue
				}
				m, ok := child.(map[string]any)
				if ok {
					if err := validateJSONSchema(m, raw); err != nil {
						return fmt.Errorf("field %s: %w", key, err)
					}
				}
			}
		}
	case []any:
		if min, ok := number(schema["minItems"]); ok && float64(len(typed)) < min {
			return fmt.Errorf("array has fewer than minItems")
		}
		if max, ok := number(schema["maxItems"]); ok && float64(len(typed)) > max {
			return fmt.Errorf("array has more than maxItems")
		}
		if items, ok := schema["items"].(map[string]any); ok {
			for i, item := range typed {
				if err := validateJSONSchema(items, item); err != nil {
					return fmt.Errorf("item %d: %w", i, err)
				}
			}
		}
	case string:
		if min, ok := number(schema["minLength"]); ok && float64(len([]rune(typed))) < min {
			return fmt.Errorf("string is shorter than minLength")
		}
		if pattern, ok := schema["pattern"].(string); ok {
			matched, err := regexp.MatchString(pattern, typed)
			if err != nil || !matched {
				return fmt.Errorf("string does not match pattern")
			}
		}
	case float64:
		if min, ok := number(schema["minimum"]); ok && typed < min {
			return fmt.Errorf("number is below minimum")
		}
		if max, ok := number(schema["maximum"]); ok && typed > max {
			return fmt.Errorf("number is above maximum")
		}
	}
	return nil
}

func matchesType(typ string, value any) bool {
	switch typ {
	case "object":
		_, ok := value.(map[string]any)
		return ok
	case "array":
		_, ok := value.([]any)
		return ok
	case "string":
		_, ok := value.(string)
		return ok
	case "boolean":
		_, ok := value.(bool)
		return ok
	case "null":
		return value == nil
	case "number":
		n, ok := number(value)
		return ok && !math.IsNaN(n)
	case "integer":
		n, ok := number(value)
		return ok && math.Trunc(n) == n
	}
	return true
}

func sameJSON(a, b any) bool {
	aj, _ := json.Marshal(a)
	bj, _ := json.Marshal(b)
	return string(aj) == string(bj)
}
func number(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case int:
		return float64(n), true
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	}
	return 0, false
}
func schemaStrings(value any) ([]string, bool) {
	switch values := value.(type) {
	case []any:
		out := make([]string, len(values))
		for i, value := range values {
			name, ok := value.(string)
			if !ok {
				return nil, false
			}
			out[i] = name
		}
		return out, true
	case []string:
		return values, true
	default:
		return nil, false
	}
}
func toolKey(name, version string) string { return name + "@" + version }
func stringPtr(s string) *string          { return &s }
func cloneToolSpec(in ToolSpec) ToolSpec {
	in.InputSchema = cloneMap(in.InputSchema)
	in.OutputSchema = cloneMap(in.OutputSchema)
	in.TimeoutPolicy = cloneMap(in.TimeoutPolicy)
	in.RetryPolicy = cloneMap(in.RetryPolicy)
	return in
}
func cloneMap(in map[string]any) map[string]any {
	if in == nil {
		return nil
	}
	data, err := json.Marshal(in)
	if err == nil {
		var out map[string]any
		if json.Unmarshal(data, &out) == nil {
			return out
		}
	}
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func normalizeJSONMap(in map[string]any) map[string]any {
	if in == nil {
		return nil
	}
	data, err := json.Marshal(in)
	if err != nil {
		return in
	}
	var out map[string]any
	if json.Unmarshal(data, &out) != nil {
		return in
	}
	return out
}
