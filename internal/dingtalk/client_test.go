package dingtalk

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/mokexinxin/cpa-monitor/internal/notification"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) { return f(request) }

func TestClientSendsSignedMarkdown(t *testing.T) {
	t.Parallel()
	client := newTestClient(t)
	now := time.Date(2026, 7, 13, 1, 2, 3, 456000000, time.UTC)
	client.now = func() time.Time { return now }
	client.httpClient.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.Scheme+"://"+request.URL.Host+request.URL.Path != webhookEndpoint {
			t.Errorf("URL = %s", request.URL)
		}
		if request.URL.Query().Get("timestamp") != "1783904523456" || request.URL.Query().Get("sign") == "" {
			t.Errorf("query = %v", request.URL.Query())
		}
		var payload markdownPayload
		if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		if payload.MessageType != "markdown" {
			t.Errorf("payload = %#v", payload)
		}
		return response(http.StatusOK, `{"errcode":0,"errmsg":"ok"}`), nil
	})
	if err := client.SendBatch(context.Background(), testBatch(now)); err != nil {
		t.Fatal(err)
	}
}

func TestClientTreatsAPIErrorsAsFailures(t *testing.T) {
	t.Parallel()
	client := newTestClient(t)
	client.httpClient.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		return response(http.StatusOK, `{"errcode":"310000","errmsg":"keywords not in content"}`), nil
	})
	err := client.SendBatch(context.Background(), testBatch(time.Now()))
	if err == nil || !strings.Contains(err.Error(), "310000") {
		t.Fatalf("error = %v", err)
	}
}

func TestClientRateLimitCooldownSkipsHTTP(t *testing.T) {
	t.Parallel()
	client := newTestClient(t)
	now := time.Date(2026, 7, 13, 1, 2, 3, 0, time.UTC)
	client.now = func() time.Time { return now }
	calls := 0
	client.httpClient.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		calls++
		return response(http.StatusOK, `{"errcode":410100,"errmsg":"rate limited"}`), nil
	})
	if err := client.SendBatch(context.Background(), testBatch(now)); err == nil {
		t.Fatal("first SendBatch() error = nil")
	}
	if err := client.SendBatch(context.Background(), testBatch(now)); err == nil || !strings.Contains(err.Error(), "cooling down") {
		t.Fatalf("second SendBatch() error = %v", err)
	}
	if calls != 1 {
		t.Fatalf("HTTP calls = %d, want 1", calls)
	}
}

func TestClientRedactsCredentialsFromTransportError(t *testing.T) {
	t.Parallel()
	client := newTestClient(t)
	client.httpClient.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		return nil, errors.New("failed URL " + request.URL.String() + " secret-value")
	})
	err := client.SendBatch(context.Background(), testBatch(time.Now()))
	if err == nil {
		t.Fatal("SendBatch() error = nil")
	}
	for _, secret := range []string{"token-value", "secret-value"} {
		if strings.Contains(err.Error(), secret) {
			t.Fatalf("error leaked %q: %v", secret, err)
		}
	}
}

func TestClientRedactsURLEncodedCredentials(t *testing.T) {
	t.Parallel()
	client, err := New(Config{WebhookToken: "token+/=", SigningSecret: "secret+/=", Language: "zh-CN", Timeout: time.Second, MaxItems: 10})
	if err != nil {
		t.Fatal(err)
	}
	client.httpClient.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		return nil, errors.New("failed " + request.URL.RawQuery)
	})
	err = client.SendBatch(context.Background(), testBatch(time.Now()))
	if err == nil {
		t.Fatal("SendBatch() error = nil")
	}
	for _, secret := range []string{"token+/=", "secret+/=", url.QueryEscape("token+/="), url.QueryEscape("secret+/=")} {
		if strings.Contains(err.Error(), secret) {
			t.Fatalf("error leaked credential representation %q: %v", secret, err)
		}
	}
}

func TestNewRejectsUnsafeMentionAndItemCombinations(t *testing.T) {
	t.Parallel()
	base := Config{WebhookToken: "token", SigningSecret: "secret", Language: "zh-CN", Timeout: time.Second, MaxItems: 10}
	tooMany := base
	tooMany.MaxItems = 51
	if _, err := New(tooMany); err == nil {
		t.Fatal("New() accepted more than 50 items")
	}
	conflict := base
	conflict.AtAll = true
	conflict.AtMobiles = []string{"13800000000"}
	if _, err := New(conflict); err == nil {
		t.Fatal("New() accepted at_all with individual mentions")
	}
}

func newTestClient(t *testing.T) *Client {
	t.Helper()
	client, err := New(Config{WebhookToken: "token-value", SigningSecret: "secret-value", Language: "zh-CN", Timeout: time.Second, MaxItems: 10})
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func testBatch(now time.Time) notification.Batch {
	event := notification.Event{Kind: notification.Alert, Scope: "memory", Hostname: "host", Timestamp: now, Key: "resource:memory", Object: "Memory", Current: "90%", Threshold: "80%"}
	return notification.Batch{Kind: event.Kind, Scope: event.Scope, Hostname: event.Hostname, Timestamp: event.Timestamp, Events: []notification.Event{event}}
}

func response(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}
}
