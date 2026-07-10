package cliproxy

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const testManagementKey = "management-key-that-must-stay-secret"

func TestNewValidatesInputs(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		baseURL string
		key     string
		timeout time.Duration
	}{
		{name: "malformed URL", baseURL: "://bad", key: testManagementKey, timeout: time.Second},
		{name: "relative URL", baseURL: "/local", key: testManagementKey, timeout: time.Second},
		{name: "missing host", baseURL: "http://", key: testManagementKey, timeout: time.Second},
		{name: "unsupported scheme", baseURL: "ftp://example.test", key: testManagementKey, timeout: time.Second},
		{name: "remote plaintext HTTP", baseURL: "http://example.test", key: testManagementKey, timeout: time.Second},
		{name: "URL userinfo", baseURL: "http://user:pass@example.test", key: testManagementKey, timeout: time.Second},
		{name: "empty management key", baseURL: "http://example.test", key: "", timeout: time.Second},
		{name: "header injection", baseURL: "http://example.test", key: testManagementKey + "\r\nInjected: true", timeout: time.Second},
		{name: "zero timeout", baseURL: "http://example.test", key: testManagementKey, timeout: 0},
		{name: "negative timeout", baseURL: "http://example.test", key: testManagementKey, timeout: -time.Second},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := New(tt.baseURL, tt.key, tt.timeout)
			if err == nil {
				t.Fatal("New() error = nil, want validation error")
			}
			if tt.key != "" && strings.Contains(err.Error(), tt.key) {
				t.Fatalf("New() error leaked management key: %v", err)
			}
		})
	}
}

func TestCheckHealthUsesGETAndBasePath(t *testing.T) {
	t.Parallel()

	for _, suffix := range []string{"/proxy", "/proxy/"} {
		suffix := suffix
		t.Run(suffix, func(t *testing.T) {
			t.Parallel()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet {
					t.Errorf("method = %q, want GET", r.Method)
				}
				if r.URL.Path != "/proxy/healthz" {
					t.Errorf("path = %q, want /proxy/healthz", r.URL.Path)
				}
				if got := r.Header.Get("Authorization"); got != "" {
					t.Errorf("health Authorization = %q, want empty", got)
				}
				w.WriteHeader(http.StatusOK)
				_, _ = io.WriteString(w, "not JSON and intentionally ignored")
			}))
			defer server.Close()

			client := mustClient(t, server.URL+suffix, time.Second)
			if err := client.CheckHealth(context.Background()); err != nil {
				t.Fatalf("CheckHealth() error = %v", err)
			}
		})
	}
}

func TestCheckHealthRequiresExactlyStatus200(t *testing.T) {
	t.Parallel()

	for _, status := range []int{http.StatusCreated, http.StatusNoContent, http.StatusMultipleChoices, http.StatusInternalServerError} {
		status := status
		t.Run(http.StatusText(status), func(t *testing.T) {
			t.Parallel()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(status)
			}))
			defer server.Close()

			err := mustClient(t, server.URL, time.Second).CheckHealth(context.Background())
			if err == nil {
				t.Fatalf("CheckHealth() error = nil for status %d", status)
			}
		})
	}
}

func TestCheckHealthHonorsContextAndTimeout(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer server.Close()

	t.Run("context cancellation", func(t *testing.T) {
		client := mustClient(t, server.URL, time.Second)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		err := client.CheckHealth(ctx)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("CheckHealth() error = %v, want context.Canceled", err)
		}
	})

	t.Run("client timeout", func(t *testing.T) {
		client := mustClient(t, server.URL, 20*time.Millisecond)
		err := client.CheckHealth(context.Background())
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("CheckHealth() error = %v, want context deadline", err)
		}
	})
}

func TestAuthFilesSendsBearerAndDecodesWireContract(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("method = %q, want GET", r.Method)
		}
		if r.URL.Path != "/prefix/v0/management/auth-files" {
			t.Errorf("path = %q, want management endpoint", r.URL.Path)
		}
		if got, want := r.Header.Get("Authorization"), "Bearer "+testManagementKey; got != want {
			t.Errorf("Authorization = %q, want %q", got, want)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{
			"files": [
				{
					"auth_index": "auth-one",
					"name": "one.json",
					"type": "claude",
					"provider": "anthropic",
					"email": "one@example.test",
					"account": "team-one",
					"status": "active",
					"status_message": "ready",
					"disabled": true,
					"unavailable": true,
					"success": 12,
					"failed": 3,
					"recent_requests": [{"time":"2026-07-10T00:00:00Z","success":2,"failed":1}],
					"future_field": {"is": "ignored"}
				},
				{"auth_index": 123456789012345678901234567890, "name": "two.json"}
			],
			"future_top_level": true
		}`)
	}))
	defer server.Close()

	files, err := mustClient(t, server.URL+"/prefix/", time.Second).AuthFiles(context.Background())
	if err != nil {
		t.Fatalf("AuthFiles() error = %v", err)
	}
	if len(files) != 2 {
		t.Fatalf("len(AuthFiles()) = %d, want 2", len(files))
	}
	want := AuthFile{
		AuthIndex:     "auth-one",
		Name:          "one.json",
		Type:          "claude",
		Provider:      "anthropic",
		Email:         "one@example.test",
		Account:       "team-one",
		Status:        "active",
		StatusMessage: "ready",
		Disabled:      true,
		Unavailable:   true,
		Success:       12,
		Failed:        3,
	}
	if got := files[0]; got.AuthIndex != want.AuthIndex || got.Name != want.Name || got.Type != want.Type ||
		got.Provider != want.Provider || got.Email != want.Email || got.Account != want.Account ||
		got.Status != want.Status || got.StatusMessage != want.StatusMessage || got.Disabled != want.Disabled ||
		got.Unavailable != want.Unavailable || got.Success != want.Success || got.Failed != want.Failed {
		t.Fatalf("first AuthFile = %#v, want fields %#v", got, want)
	}
	if got := files[0].RecentRequests; len(got) != 1 || got[0].Time != "2026-07-10T00:00:00Z" || got[0].Success != 2 || got[0].Failed != 1 {
		t.Fatalf("RecentRequests = %#v, want decoded bucket", got)
	}
	if got, want := files[1].AuthIndex, "123456789012345678901234567890"; got != want {
		t.Fatalf("numeric AuthIndex = %q, want %q", got, want)
	}
}

func TestAuthFilesAcceptsEmptyFilesArray(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"files":[]}`)
	}))
	defer server.Close()

	files, err := mustClient(t, server.URL, time.Second).AuthFiles(context.Background())
	if err != nil {
		t.Fatalf("AuthFiles() error = %v", err)
	}
	if files == nil || len(files) != 0 {
		t.Fatalf("AuthFiles() = %#v, want non-nil empty slice", files)
	}
}

func TestAuthFilesLeavesMissingAuthIndexForEntryValidation(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"files":[{"name":"missing-index.json"}]}`)
	}))
	defer server.Close()

	files, err := mustClient(t, server.URL, time.Second).AuthFiles(context.Background())
	if err != nil {
		t.Fatalf("AuthFiles() error = %v", err)
	}
	if len(files) != 1 || files[0].AuthIndex != "" {
		t.Fatalf("AuthFiles() = %#v, want one entry with empty AuthIndex", files)
	}
}

func TestAuthFilesTimeoutIncludesResponseBodyRead(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		<-r.Context().Done()
	}))
	defer server.Close()

	_, err := mustClient(t, server.URL, 20*time.Millisecond).AuthFiles(context.Background())
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("AuthFiles() error = %v, want context deadline", err)
	}
}

func TestAuthFilesRejectsInvalidResponses(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		status int
		body   string
	}{
		{name: "non-200", status: http.StatusUnauthorized, body: testManagementKey},
		{name: "malformed JSON", status: http.StatusOK, body: `{"files":[`},
		{name: "missing files", status: http.StatusOK, body: `{}`},
		{name: "null files", status: http.StatusOK, body: `{"files":null}`},
		{name: "object files", status: http.StatusOK, body: `{"files":{}}`},
		{name: "string files", status: http.StatusOK, body: `{"files":"wrong"}`},
		{name: "boolean auth index", status: http.StatusOK, body: `{"files":[{"auth_index":true}]}`},
		{name: "fractional auth index", status: http.StatusOK, body: `{"files":[{"auth_index":1.5}]}`},
		{name: "trailing JSON", status: http.StatusOK, body: `{"files":[]} {}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tt.status)
				_, _ = io.WriteString(w, tt.body)
			}))
			defer server.Close()

			_, err := mustClient(t, server.URL, time.Second).AuthFiles(context.Background())
			if err == nil {
				t.Fatal("AuthFiles() error = nil, want response error")
			}
			if strings.Contains(err.Error(), testManagementKey) {
				t.Fatalf("AuthFiles() error leaked management key: %v", err)
			}
		})
	}
}

func TestAuthFilesDoesNotFollowRedirects(t *testing.T) {
	t.Parallel()

	var targetRequests atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		targetRequests.Add(1)
		if got := r.Header.Get("Authorization"); got != "" {
			t.Errorf("redirect target received Authorization %q", got)
		}
		_, _ = io.WriteString(w, `{"files":[]}`)
	}))
	defer target.Close()

	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer source.Close()

	_, err := mustClient(t, source.URL, time.Second).AuthFiles(context.Background())
	if err == nil {
		t.Fatal("AuthFiles() error = nil, want redirect rejection")
	}
	if got := targetRequests.Load(); got != 0 {
		t.Fatalf("redirect target request count = %d, want 0", got)
	}
	if strings.Contains(err.Error(), testManagementKey) {
		t.Fatalf("AuthFiles() error leaked management key: %v", err)
	}
}

func TestAuthFilesEnforcesBodyLimit(t *testing.T) {
	t.Parallel()

	prefix := `{"files":[]}`
	tests := []struct {
		name    string
		body    string
		wantErr bool
	}{
		{name: "exactly limit", body: prefix + strings.Repeat(" ", maxAuthFilesResponseBytes-len(prefix))},
		{name: "over limit", body: prefix + strings.Repeat(" ", maxAuthFilesResponseBytes-len(prefix)+1), wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = io.WriteString(w, tt.body)
			}))
			defer server.Close()

			_, err := mustClient(t, server.URL, 5*time.Second).AuthFiles(context.Background())
			if (err != nil) != tt.wantErr {
				t.Fatalf("AuthFiles() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestResponseBodiesAreAlwaysClosed(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		status     int
		body       string
		callHealth bool
	}{
		{name: "health success", status: http.StatusOK, body: "ignored", callHealth: true},
		{name: "health status error", status: http.StatusBadGateway, body: "ignored", callHealth: true},
		{name: "management success", status: http.StatusOK, body: `{"files":[]}`},
		{name: "management status error", status: http.StatusUnauthorized, body: "ignored"},
		{name: "management JSON error", status: http.StatusOK, body: `{"files":`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			body := &trackingBody{Reader: strings.NewReader(tt.body)}
			client := mustClient(t, "https://example.test", time.Second)
			client.httpClient.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
				return &http.Response{
					StatusCode: tt.status,
					Header:     make(http.Header),
					Body:       body,
				}, nil
			})
			if tt.callHealth {
				_ = client.CheckHealth(context.Background())
			} else {
				_, _ = client.AuthFiles(context.Background())
			}
			if !body.closed.Load() {
				t.Fatal("response body was not closed")
			}
		})
	}
}

func TestErrorsRedactManagementKeyAndPreserveCause(t *testing.T) {
	t.Parallel()

	cause := errors.New("transport accidentally included " + testManagementKey)
	client := mustClient(t, "https://example.test", time.Second)
	client.httpClient.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, cause
	})

	_, err := client.AuthFiles(context.Background())
	if err == nil {
		t.Fatal("AuthFiles() error = nil, want transport error")
	}
	if strings.Contains(err.Error(), testManagementKey) {
		t.Fatalf("AuthFiles() error leaked management key: %v", err)
	}
	if !errors.Is(err, cause) {
		t.Fatalf("errors.Is(%v, cause) = false", err)
	}
}

func mustClient(t *testing.T, baseURL string, timeout time.Duration) *Client {
	t.Helper()
	client, err := New(baseURL, testManagementKey, timeout)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	return client
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

type trackingBody struct {
	io.Reader
	closed atomic.Bool
}

func (b *trackingBody) Close() error {
	b.closed.Store(true)
	return nil
}
