package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"unicode"
)

type providerRoundTripFunc func(*http.Request) (*http.Response, error)

func (fn providerRoundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return fn(request)
}

func providerWithoutWait(serverURL string) *OpenAIProvider {
	p := NewOpenAIProvider(ProviderConfig{BaseURL: serverURL, APIKey: "secret-key", Model: "test-model", Timeout: time.Second})
	p.retry = retryTiming{wait: func(context.Context, time.Duration) error { return nil }, backoff: func(int) time.Duration { return time.Millisecond }}
	return p
}

func requireProviderError(t *testing.T, err error, code string, attempts int) *ProviderError {
	t.Helper()
	var classified *ProviderError
	if !errors.As(err, &classified) || classified.Code != code || classified.Attempts != attempts {
		t.Fatalf("error = %#v; want code=%s attempts=%d", err, code, attempts)
	}
	if !strings.ContainsFunc(classified.Message, func(char rune) bool { return unicode.Is(unicode.Han, char) }) {
		t.Fatalf("provider error is not readable Chinese: %q", classified.Message)
	}
	return classified
}

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

func TestOpenAIProviderConstrainsOnlyRoutingStages(t *testing.T) {
	for _, stage := range []string{"skill_selection", "intent", "execution"} {
		t.Run(stage, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var request map[string]any
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Error(err)
				}
				if stage == "execution" {
					if request["tool_choice"] != "auto" {
						t.Error("execution must allow text completion")
					}
				} else if !sameJSON(request["tool_choice"], map[string]any{"type": "function", "function": map[string]any{"name": "route"}}) {
					t.Error("routing tool was not required")
				}
				_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`))
			}))
			defer server.Close()
			_, err := providerWithoutWait(server.URL).Complete(context.Background(), CompletionRequest{Stage: stage, Tools: []ProviderTool{{Type: "function", Function: ProviderFunction{Name: "route"}}}})
			if err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestOpenAIProviderRetriesSameCompletionAndObservesWait(t *testing.T) {
	var requests [][]byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		requests = append(requests, body)
		if len(requests) < 3 {
			w.Header().Set("Retry-After", "120")
			w.Header().Set("X-Request-ID", "req-test-1")
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = io.WriteString(w, `{"error":{"code":"overloaded","message":"secret-key must never be shown"}}`)
			return
		}
		_, _ = io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"完成"}}]}`)
	}))
	defer server.Close()
	p := providerWithoutWait(server.URL)
	var delays []time.Duration
	p.retry.wait = func(_ context.Context, delay time.Duration) error { delays = append(delays, delay); return nil }
	var notices []RetryNotice
	ctx := withRetryObserver(context.Background(), func(notice RetryNotice) { notices = append(notices, notice) })
	response, err := p.Complete(ctx, CompletionRequest{Stage: "intent", Messages: []ProviderMessage{{Role: "user", Content: "查影像"}}})
	if err != nil || response.Message.Content != "完成" {
		t.Fatalf("response=%+v error=%v", response, err)
	}
	if len(requests) != 3 || !bytes.Equal(requests[0], requests[1]) || !bytes.Equal(requests[1], requests[2]) {
		t.Fatalf("retry changed completion request: %q", requests)
	}
	if bytes.Contains(requests[0], []byte(`"Stage"`)) || bytes.Contains(requests[0], []byte(`"stage"`)) {
		t.Fatal("internal stage leaked into provider request")
	}
	if len(delays) != 2 || delays[0] != 30*time.Second || delays[1] != 30*time.Second {
		t.Fatalf("delays = %v", delays)
	}
	if len(notices) != 2 || notices[0].Stage != "intent" || notices[0].Attempt != 2 || notices[1].Attempt != 3 || notices[0].MaxAttempts != 3 || notices[0].DelayMS != 30000 {
		t.Fatalf("notices = %+v", notices)
	}
}

func TestOpenAIProviderClassifiesHTTPFailuresAndBoundsRetries(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		code   string
		retry  bool
	}{
		{"rate limit", 429, `{"error":{"code":"rate_limit_exceeded"}}`, "AI_RATE_LIMITED", true},
		{"quota", 429, `{"error":{"code":"insufficient_quota","type":"invalid_request_error"}}`, "AI_QUOTA_EXCEEDED", false},
		{"authentication", 401, `{"error":{"message":"Authorization: Bearer secret-key"}}`, "AI_AUTH_FAILED", false},
		{"permission", 403, `{}`, "AI_AUTH_FAILED", false},
		{"payment", 402, `{}`, "AI_QUOTA_EXCEEDED", false},
		{"invalid parameters", 400, `{"error":{"code":"invalid_request_error"}}`, "AI_INVALID_REQUEST", false},
		{"context limit", 400, `{"error":{"code":"context_length_exceeded"}}`, "AI_CONTEXT_LIMIT", false},
		{"request timeout", 408, `{}`, "AI_TIMEOUT", true},
		{"upstream", 502, `{}`, "AI_PROVIDER_UNAVAILABLE", true},
		{"upstream unavailable", 503, `{}`, "AI_PROVIDER_UNAVAILABLE", true},
		{"gateway timeout", 504, `{}`, "AI_PROVIDER_UNAVAILABLE", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				calls++
				w.Header().Set("X-Request-ID", "req-unique-123")
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			}))
			defer server.Close()
			p := providerWithoutWait(server.URL)
			_, err := p.Complete(context.Background(), CompletionRequest{Stage: "execution"})
			wantAttempts := 1
			if tc.retry {
				wantAttempts = 3
			}
			classified := requireProviderError(t, err, tc.code, wantAttempts)
			if calls != wantAttempts || classified.Retryable != tc.retry || classified.Stage != "execution" || classified.HTTPStatus != tc.status || classified.RequestID != "req-unique-123" {
				t.Fatalf("calls=%d classified=%+v", calls, classified)
			}
			if strings.Contains(err.Error(), "secret-key") || strings.Contains(err.Error(), "Bearer") {
				t.Fatalf("diagnostics leaked: %v", err)
			}
		})
	}
}

func TestOpenAIProviderRejectsMalformedResponsesWithoutRetry(t *testing.T) {
	for _, body := range []string{`not json secret-key`, `{}`, `{"choices":[]}`, `{"choices":[{"message":{"role":"assistant"}}]}`, `{"choices":[{"message":{"role":"user","content":"wrong"}}]}`, `{"choices":[{"message":{"tool_calls":[{"function":{"name":"x"}}]}}]}`, `{"choices":[{"message":{"tool_calls":[{"id":"duplicate","function":{"name":"first"}},{"id":"duplicate","function":{"name":"second"}}]}}]}`} {
		t.Run(body, func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { calls++; _, _ = io.WriteString(w, body) }))
			defer server.Close()
			_, err := providerWithoutWait(server.URL).Complete(context.Background(), CompletionRequest{})
			requireProviderError(t, err, "AI_INVALID_RESPONSE", 1)
			if calls != 1 {
				t.Fatalf("calls=%d", calls)
			}
		})
	}
}

func TestOpenAIProviderTransportFailureAndDiagnosticsAreSafe(t *testing.T) {
	p := providerWithoutWait("https://provider.invalid/v1")
	calls := 0
	p.client.Transport = providerRoundTripFunc(func(*http.Request) (*http.Response, error) {
		calls++
		return nil, errors.New("network failed Authorization: Bearer secret-key at C:\\private\\provider.json")
	})
	var logs bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&logs)
	defer log.SetOutput(previous)
	_, err := p.Complete(context.Background(), CompletionRequest{})
	requireProviderError(t, err, "AI_NETWORK_ERROR", 3)
	if calls != 3 || strings.Count(logs.String(), "[ai_provider]") != 3 || !strings.Contains(logs.String(), "stage=completion attempts=3") {
		t.Fatalf("calls=%d logs=%q", calls, logs.String())
	}
	for _, secret := range []string{"secret-key", "Bearer", "private", "provider.invalid"} {
		if strings.Contains(err.Error(), secret) || strings.Contains(logs.String(), secret) {
			t.Fatalf("unsafe diagnostic %q", secret)
		}
	}
}

func TestOpenAIProviderCancellationDuringBackoffStopsRequests(t *testing.T) {
	p := providerWithoutWait("https://provider.invalid/v1")
	calls := 0
	p.client.Transport = providerRoundTripFunc(func(*http.Request) (*http.Response, error) {
		calls++
		return nil, errors.New("temporary connection failure")
	})
	ctx, cancel := context.WithCancel(context.Background())
	ctx = withRetryObserver(ctx, func(RetryNotice) { cancel() })
	_, err := p.Complete(ctx, CompletionRequest{Stage: "intent"})
	classified := requireProviderError(t, err, "AI_CANCELLED", 1)
	if calls != 1 || classified.Retryable || !errors.Is(err, context.Canceled) {
		t.Fatalf("calls=%d error=%+v", calls, classified)
	}
}

func TestOpenAIProviderCapsWholeCompletionBudget(t *testing.T) {
	p := providerWithoutWait("https://provider.invalid/v1")
	p.config.Timeout = 2 * time.Second
	p.client.Timeout = 0 // Inspect the outer budget independently of http.Client's per-attempt cap.
	p.client.Transport = providerRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		deadline, ok := r.Context().Deadline()
		if remaining := time.Until(deadline); !ok || remaining < 35*time.Second || remaining > 36*time.Second {
			t.Errorf("completion budget=%s exists=%v", remaining, ok)
		}
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`))}, nil
	})
	if _, err := p.Complete(context.Background(), CompletionRequest{}); err != nil {
		t.Fatal(err)
	}
}

func TestOpenAIProviderUsesBoundedBudgetAndIndependentAttemptTimeouts(t *testing.T) {
	p := providerWithoutWait("https://provider.invalid/v1")
	p.config.Timeout = 5 * time.Millisecond
	p.client.Timeout = p.config.Timeout
	calls := 0
	var budgets []time.Duration
	p.client.Transport = providerRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		deadline, ok := r.Context().Deadline()
		if !ok {
			t.Error("missing request deadline")
		}
		budgets = append(budgets, time.Until(deadline))
		<-r.Context().Done()
		return nil, r.Context().Err()
	})
	_, err := p.Complete(context.Background(), CompletionRequest{})
	requireProviderError(t, err, "AI_TIMEOUT", 3)
	if calls != 3 {
		t.Fatalf("calls=%d", calls)
	}
	for _, budget := range budgets {
		if budget <= 0 || budget > 10*time.Millisecond {
			t.Fatalf("attempt deadline=%s", budget)
		}
	}
	// A caller deadline is an upper bound and must stop all retry attempts.
	ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancel()
	calls = 0
	_, err = p.Complete(ctx, CompletionRequest{})
	classified := requireProviderError(t, err, "AI_TIMEOUT", 1)
	if classified.Retryable || calls != 1 {
		t.Fatalf("calls=%d error=%+v", calls, classified)
	}
}

func TestProviderRetryAfterAndRequestIDSanitization(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	for _, tc := range []struct {
		value string
		want  time.Duration
	}{
		{"5", 5 * time.Second}, {"300", 30 * time.Second}, {"-1", 0}, {"bad", 0},
		{now.Add(10 * time.Second).Format(http.TimeFormat), 10 * time.Second},
		{now.Add(time.Minute).Format(http.TimeFormat), 30 * time.Second},
	} {
		if got := providerRetryAfter(tc.value, now); got != tc.want {
			t.Errorf("Retry-After %q=%s want %s", tc.value, got, tc.want)
		}
	}
	for _, value := range []string{"secret-key", "req\nsecret", "req/unsafe", strings.Repeat("a", 129)} {
		if got := safeProviderRequestID(value, "secret-key"); got != "" {
			t.Errorf("unsafe ID=%q", got)
		}
	}
	if got := safeProviderRequestID("req-test_123", "secret-key"); got != "req-test_123" {
		t.Fatalf("safe ID=%q", got)
	}
}

func TestOpenAIProviderNeverLogsProviderBodyOrCredentialRequestID(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Request-ID", "secret-key")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":{"code":"insufficient_quota","message":"secret-key /private/credentials.json raw-provider-detail"}}`)
	}))
	defer server.Close()
	var logs bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&logs)
	defer log.SetOutput(previous)
	_, err := providerWithoutWait(server.URL).Complete(context.Background(), CompletionRequest{})
	classified := requireProviderError(t, err, "AI_QUOTA_EXCEEDED", 1)
	encoded, _ := json.Marshal(classified)
	for _, detail := range []string{"secret-key", "private", "credentials.json", "raw-provider-detail"} {
		if strings.Contains(logs.String(), detail) || bytes.Contains(encoded, []byte(detail)) {
			t.Fatalf("provider diagnostic leaked %q", detail)
		}
	}
	if classified.RequestID != "" {
		t.Fatalf("request ID leaked: %q", classified.RequestID)
	}
}

func TestOpenAIProviderInvalidAddressDoesNotRetryTransport(t *testing.T) {
	for _, address := range []string{"file:///private/config", "missing-host", "https://"} {
		p := providerWithoutWait(address)
		calls := 0
		p.client.Transport = providerRoundTripFunc(func(*http.Request) (*http.Response, error) {
			calls++
			return nil, errors.New("should not reach transport")
		})
		_, err := p.Complete(context.Background(), CompletionRequest{})
		requireProviderError(t, err, "AI_INVALID_REQUEST", 1)
		if calls != 0 {
			t.Fatalf("invalid URL reached transport: %q", address)
		}
	}
}

func TestProviderFailureLogWhitelistsStage(t *testing.T) {
	var logs bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&logs)
	defer log.SetOutput(previous)
	logProviderFailure(&ProviderError{Code: "AI_NETWORK_ERROR", Stage: "intent\nsecret-key", Attempts: 2})
	if strings.Contains(logs.String(), "secret-key") || !strings.Contains(logs.String(), "stage=unknown attempts=2") {
		t.Fatalf("unsafe stage log=%q", logs.String())
	}
	if safeProviderLogStage("tool.workspace.context") != "tool.workspace.context" || safeProviderLogStage("tool.secret-key") != "unknown" {
		t.Fatal("tool stage allowlist failed")
	}
}
