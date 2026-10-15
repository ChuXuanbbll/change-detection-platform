package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
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
}

func NewOpenAIProvider(config ProviderConfig) *OpenAIProvider {
	timeout := config.Timeout
	if timeout <= 0 {
		timeout = 45 * time.Second
	}
	return &OpenAIProvider{config: config, client: &http.Client{Timeout: timeout}}
}

func (p *OpenAIProvider) Complete(ctx context.Context, request CompletionRequest) (CompletionResponse, error) {
	if !p.config.Configured() {
		return CompletionResponse{}, fmt.Errorf("AI provider is not configured; set AI_API_BASE, AI_API_KEY, and AI_MODEL")
	}
	payload := map[string]any{"model": p.config.Model, "messages": request.Messages}
	if len(request.Tools) > 0 {
		payload["tools"] = request.Tools
		payload["tool_choice"] = "auto"
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return CompletionResponse{}, fmt.Errorf("encode provider request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.config.BaseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return CompletionResponse{}, fmt.Errorf("create provider request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+p.config.APIKey)
	resp, err := p.client.Do(req)
	if err != nil {
		return CompletionResponse{}, fmt.Errorf("provider request failed: %w", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return CompletionResponse{}, fmt.Errorf("read provider response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return CompletionResponse{}, fmt.Errorf("provider returned HTTP %d", resp.StatusCode)
	}
	var decoded struct {
		Choices []struct {
			Message ProviderMessage `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(data, &decoded); err != nil {
		return CompletionResponse{}, fmt.Errorf("decode provider response: %w", err)
	}
	if len(decoded.Choices) == 0 {
		return CompletionResponse{}, fmt.Errorf("provider returned no choices")
	}
	return CompletionResponse{Message: decoded.Choices[0].Message}, nil
}
