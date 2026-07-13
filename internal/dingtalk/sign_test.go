package dingtalk

import (
	"net/url"
	"testing"
)

func TestSignature(t *testing.T) {
	t.Parallel()
	const timestamp int64 = 1700000000123
	const want = "lrMq5E54T40t3FfGVjxdvoqIbXlWY6mzkZYiVoI6emk="
	if got := signature(timestamp, "SECtest"); got != want {
		t.Fatalf("signature() = %q, want %q", got, want)
	}
}

func TestSignedWebhookURL(t *testing.T) {
	t.Parallel()
	parsed, err := url.Parse(signedWebhookURL("token+value", "SECtest", 1700000000123))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := parsed.Scheme+"://"+parsed.Host+parsed.Path, webhookEndpoint; got != want {
		t.Fatalf("endpoint = %q, want %q", got, want)
	}
	query := parsed.Query()
	if query.Get("access_token") != "token+value" || query.Get("timestamp") != "1700000000123" || query.Get("sign") == "" {
		t.Fatalf("query = %#v", query)
	}
}
