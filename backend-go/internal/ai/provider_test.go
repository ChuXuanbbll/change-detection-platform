package ai

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestOpenAIProviderUsesCompatibleEndpointWithoutLeakingCredential(t *testing.T) {
	var gotAuthorization string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuthorization = r.Header.Get("Authorization")
		var request map[string]any
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Errorf("decode request: %v", err)
		}
		if request["model"] != "test-model" {
			t.Errorf("model = %v", request["model"])
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`))
	}))
	defer server.Close()
	p := NewOpenAIProvider(ProviderConfig{BaseURL: server.URL, APIKey: "secret-key", Model: "test-model", Timeout: time.Second})
	response, err := p.Complete(context.Background(), CompletionRequest{Messages: []ProviderMessage{{Role: "user", Content: "hello"}}})
	if err != nil {
		t.Fatal(err)
	}
	if response.Message.Content != "ok" {
		t.Fatalf("content = %q", response.Message.Content)
	}
	if gotAuthorization != "Bearer secret-key" {
		t.Fatalf("authorization = %q", gotAuthorization)
	}
	if strings.Contains(response.Message.Content, "secret-key") {
		t.Fatal("provider credential leaked")
	}
}

func TestOpenAIProviderMissingConfigurationIsActionable(t *testing.T) {
	p := NewOpenAIProvider(ProviderConfig{})
	_, err := p.Complete(context.Background(), CompletionRequest{})
	if err == nil || !strings.Contains(err.Error(), "AI_API_BASE") {
		t.Fatalf("unexpected error: %v", err)
	}
}
