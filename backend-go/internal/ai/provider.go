package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

type Provider interface {
	Complete(context.Context, CompletionRequest) (CompletionResponse, error)
}

type CompletionRequest struct {
	Messages []ProviderMessage
	Tools    []ProviderTool
	Stage    string `json:"-"`
}

type ProviderMessage struct {
	Role       string             `json:"role"`
	Content    string             `json:"content,omitempty"`
	ToolCallID string             `json:"tool_call_id,omitempty"`
	ToolCalls  []ProviderToolCall `json:"tool_calls,omitempty"`
}

type ProviderTool struct {
	Type     string           `json:"type"`
	Function ProviderFunction `json:"function"`
}

type ProviderFunction struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"`
}

type ProviderToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type CompletionResponse struct{ Message ProviderMessage }

type ProviderConfig struct {
	BaseURL string
	APIKey  string
	Model   string
	Timeout time.Duration
}

func ProviderConfigFromEnv() ProviderConfig {
	timeout := 45 * time.Second
	if raw := os.Getenv("AI_TIMEOUT_MS"); raw != "" {
		if ms, err := strconv.Atoi(raw); err == nil && ms > 0 {
			timeout = time.Duration(ms) * time.Millisecond
		}
	}
	return ProviderConfig{BaseURL: strings.TrimRight(os.Getenv("AI_API_BASE"), "/"), APIKey: os.Getenv("AI_API_KEY"), Model: os.Getenv("AI_MODEL"), Timeout: timeout}
}

func (c ProviderConfig) Configured() bool { return c.BaseURL != "" && c.APIKey != "" && c.Model != "" }

type OpenAIProvider struct {
	config ProviderConfig
	client *http.Client
	retry  retryTiming
}

// ProviderError contains only safe, classified diagnostics. Raw HTTP bodies,
// URLs, transport errors and credentials never cross this boundary.
type ProviderError struct {
	Code       string `json:"code"`
	Message    string `json:"message"`
	Retryable  bool   `json:"retryable"`
	HTTPStatus int    `json:"http_status,omitempty"`
	RequestID  string `json:"request_id,omitempty"`
	Attempts   int    `json:"attempts"`
	Stage      string `json:"stage"`
}

func (e *ProviderError) Error() string { return e.Message }

func (e *ProviderError) Is(target error) bool {
	return (e.Code == "AI_CANCELLED" && target == context.Canceled) || (e.Code == "AI_TIMEOUT" && target == context.DeadlineExceeded)
}

func NewOpenAIProvider(config ProviderConfig) *OpenAIProvider {
	timeout := config.Timeout
	if timeout <= 0 {
		timeout = 45 * time.Second
	}
	config.Timeout = timeout
	return &OpenAIProvider{config: config, client: &http.Client{Timeout: timeout}}
}

func (p *OpenAIProvider) Complete(ctx context.Context, request CompletionRequest) (CompletionResponse, error) {
	stage := request.Stage
	if stage == "" {
		stage = "completion"
	}
	fail := func(code, message string) (CompletionResponse, error) {
		err := &ProviderError{Code: code, Message: message, Stage: stage}
		logProviderFailure(err)
		return CompletionResponse{}, err
	}
	if !p.config.Configured() {
		return fail("AI_NOT_CONFIGURED", "AI 服务尚未配置，请检查 AI_API_BASE、AI_API_KEY 和 AI_MODEL。")
	}
	payload := map[string]any{"model": p.config.Model, "messages": request.Messages}
	if len(request.Tools) > 0 {
		payload["tools"] = request.Tools
		payload["tool_choice"] = "auto"
		if (request.Stage == "intent" || request.Stage == "skill_selection") && len(request.Tools) == 1 {
			payload["tool_choice"] = map[string]any{"type": "function", "function": map[string]any{"name": request.Tools[0].Function.Name}}
		}
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return fail("AI_INVALID_REQUEST", "AI 请求内容无法编码，请检查请求参数。")
	}
	timeout := p.config.Timeout
	if timeout <= 0 {
		timeout = 45 * time.Second
	}
	budgetCtx, cancel := context.WithTimeout(ctx, time.Duration(maxRetryAttempts)*timeout+30*time.Second)
	defer cancel()
	for attempt := 1; attempt <= maxRetryAttempts; attempt++ {
		if budgetCtx.Err() != nil {
			classified := providerContextError(ctx, stage, attempt-1)
			logProviderFailure(classified)
			return CompletionResponse{}, classified
		}
		response, classified, retryAfter := p.completeAttempt(budgetCtx, body)
		if budgetCtx.Err() != nil {
			classified = providerContextError(ctx, stage, attempt)
		}
		if classified == nil {
			return response, nil
		}
		classified.Stage, classified.Attempts = stage, attempt
		logProviderFailure(classified)
		if !classified.Retryable || attempt == maxRetryAttempts || budgetCtx.Err() != nil {
			return CompletionResponse{}, classified
		}
		delay := p.retry.delay(attempt)
		if retryAfter > delay {
			delay = retryAfter
		}
		if remaining, ok := budgetCtx.Deadline(); ok && time.Until(remaining) <= delay {
			return CompletionResponse{}, classified
		}
		observeRetry(budgetCtx, stage, attempt+1, delay)
		if err := p.retry.sleep(budgetCtx, delay); err != nil {
			classified = providerContextError(ctx, stage, attempt)
			logProviderFailure(classified)
			return CompletionResponse{}, classified
		}
	}
	panic("unreachable retry loop")
}

func (p *OpenAIProvider) completeAttempt(ctx context.Context, body []byte) (CompletionResponse, *ProviderError, time.Duration) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.config.BaseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil || (req.URL.Scheme != "http" && req.URL.Scheme != "https") || req.URL.Host == "" {
		return CompletionResponse{}, &ProviderError{Code: "AI_INVALID_REQUEST", Message: "AI 服务地址无效，请检查管理页中的服务地址。"}, 0
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+p.config.APIKey)
	resp, err := p.client.Do(req)
	if err != nil {
		code, message := "AI_NETWORK_ERROR", "无法连接 AI 服务，请检查网络或稍后重试。"
		var networkError net.Error
		if errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &networkError) && networkError.Timeout()) {
			code, message = "AI_TIMEOUT", "AI 服务响应超时，请稍后重试。"
		}
		return CompletionResponse{}, &ProviderError{Code: code, Message: message, Retryable: true}, 0
	}
	defer resp.Body.Close()
	requestID := safeProviderRequestID(resp.Header.Get("X-Request-ID"), p.config.APIKey)
	if requestID == "" {
		requestID = safeProviderRequestID(resp.Header.Get("Request-ID"), p.config.APIKey)
	}
	data, readErr := io.ReadAll(io.LimitReader(resp.Body, (2<<20)+1))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		classified := classifyProviderHTTPError(resp.StatusCode, data)
		classified.RequestID = requestID
		return CompletionResponse{}, classified, providerRetryAfter(resp.Header.Get("Retry-After"), time.Now())
	}
	if readErr != nil {
		classified := &ProviderError{Code: "AI_NETWORK_ERROR", Message: "读取 AI 服务响应失败，请检查网络或稍后重试。", Retryable: true, HTTPStatus: resp.StatusCode, RequestID: requestID}
		var networkError net.Error
		if errors.Is(readErr, context.DeadlineExceeded) || (errors.As(readErr, &networkError) && networkError.Timeout()) {
			classified.Code, classified.Message = "AI_TIMEOUT", "AI 服务响应超时，请稍后重试。"
		}
		return CompletionResponse{}, classified, 0
	}
	invalid := func() (CompletionResponse, *ProviderError, time.Duration) {
		return CompletionResponse{}, &ProviderError{Code: "AI_INVALID_RESPONSE", Message: "AI 服务返回的内容不完整或格式不兼容，请检查服务和模型配置。", HTTPStatus: resp.StatusCode, RequestID: requestID}, 0
	}
	if len(data) > 2<<20 {
		return invalid()
	}
	var decoded struct {
		Choices []struct {
			Message ProviderMessage `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(data, &decoded); err != nil {
		return invalid()
	}
	if len(decoded.Choices) == 0 {
		return invalid()
	}
	message := decoded.Choices[0].Message
	if (message.Role != "" && message.Role != "assistant") || (strings.TrimSpace(message.Content) == "" && len(message.ToolCalls) == 0) {
		return invalid()
	}
	callIDs := make(map[string]bool, len(message.ToolCalls))
	for _, call := range message.ToolCalls {
		if call.ID == "" || callIDs[call.ID] || call.Function.Name == "" || (call.Type != "" && call.Type != "function") {
			return invalid()
		}
		callIDs[call.ID] = true
	}
	message.Role = "assistant"
	return CompletionResponse{Message: message}, nil, 0
}

func providerContextError(parent context.Context, stage string, attempts int) *ProviderError {
	if errors.Is(parent.Err(), context.Canceled) {
		return &ProviderError{Code: "AI_CANCELLED", Message: "已停止 AI 请求。", Attempts: attempts, Stage: stage}
	}
	return &ProviderError{Code: "AI_TIMEOUT", Message: "AI 服务响应超时，请稍后重试。", Retryable: parent.Err() == nil, Attempts: attempts, Stage: stage}
}

func classifyProviderHTTPError(status int, body []byte) *ProviderError {
	result := &ProviderError{Code: "AI_INVALID_REQUEST", Message: "AI 服务拒绝了请求，请检查模型和请求参数。", HTTPStatus: status}
	// Read only known machine codes. Provider prose may contain credentials,
	// submitted text or private paths and must never be logged or returned.
	var payload struct {
		Error struct {
			Code string `json:"code"`
			Type string `json:"type"`
		} `json:"error"`
	}
	_ = json.Unmarshal(body, &payload)
	for _, code := range []string{strings.ToLower(payload.Error.Code), strings.ToLower(payload.Error.Type)} {
		switch code {
		case "insufficient_quota", "quota_exceeded", "billing_hard_limit_reached", "billing_not_active", "insufficient_balance", "credit_balance_too_low", "usage_limit_exceeded":
			result.Code, result.Message = "AI_QUOTA_EXCEEDED", "AI 服务额度不足或余额已用尽，请检查服务账户。"
			return result
		case "context_length_exceeded", "max_context_length_exceeded":
			result.Code, result.Message = "AI_CONTEXT_LIMIT", "当前对话超出模型的上下文限制，请开启新对话。"
			return result
		case "invalid_api_key", "authentication_error", "permission_denied", "permission_error":
			result.Code, result.Message = "AI_AUTH_FAILED", "AI 服务鉴权失败或权限不足，请检查密钥和模型访问权限。"
			return result
		case "invalid_request_error", "invalid_argument", "unsupported_parameter", "model_not_found":
			return result
		}
	}
	switch {
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		result.Code, result.Message = "AI_AUTH_FAILED", "AI 服务鉴权失败或权限不足，请检查密钥和模型访问权限。"
	case status == http.StatusPaymentRequired:
		result.Code, result.Message = "AI_QUOTA_EXCEEDED", "AI 服务额度不足或余额已用尽，请检查服务账户。"
	case status == http.StatusRequestTimeout:
		result.Code, result.Message, result.Retryable = "AI_TIMEOUT", "AI 服务响应超时，请稍后重试。", true
	case status == http.StatusTooManyRequests:
		result.Code, result.Message, result.Retryable = "AI_RATE_LIMITED", "AI 服务暂时限流，请稍后重试。", true
	case status >= 500:
		result.Code, result.Message, result.Retryable = "AI_PROVIDER_UNAVAILABLE", "AI 服务暂时不可用，请稍后重试。", true
	}
	return result
}

func providerRetryAfter(value string, now time.Time) time.Duration {
	var delay time.Duration
	if seconds, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64); err == nil {
		if seconds >= 30 {
			return 30 * time.Second
		}
		if seconds > 0 {
			delay = time.Duration(seconds) * time.Second
		}
	} else if retryAt, err := http.ParseTime(value); err == nil {
		delay = retryAt.Sub(now)
	}
	if delay < 0 {
		return 0
	}
	if delay > 30*time.Second {
		return 30 * time.Second
	}
	return delay
}

func safeProviderRequestID(value, credential string) string {
	if value == "" || len(value) > 128 || (credential != "" && strings.Contains(value, credential)) {
		return ""
	}
	for _, char := range value {
		if !((char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') || (char >= '0' && char <= '9') || strings.ContainsRune("-_.:", char)) {
			return ""
		}
	}
	return value
}

func logProviderFailure(err *ProviderError) {
	stage := safeProviderLogStage(err.Stage)
	log.Printf("[ai_provider] code=%s request_id=%q http_status=%d stage=%s attempts=%d", err.Code, err.RequestID, err.HTTPStatus, stage, err.Attempts)
}

func safeProviderLogStage(stage string) string {
	switch stage {
	case "skill_selection", "intent", "execution", "completion":
		return stage
	}
	if strings.HasPrefix(stage, "tool.") {
		switch strings.TrimPrefix(stage, "tool.") {
		case "workspace.context", "region.search", "imagery.catalog", "imagery.scenes", "imagery.phase", "model.list", "image.list", "task.search", "task.status", "result.summary", "result.patches.query", "selection.validate", "detection.plan.create", "imagery.prepare", "detection.workflow.submit", "operation.status", "result.export.prepare", "ui.select_region", "ui.load_phase", "ui.load_phases", "ui.set_existing_aoi", "ui.fly_to_bounds", "ui.show_task", "ui.show_result", "ui.open_report":
			return stage
		}
	}
	return "unknown"
}
