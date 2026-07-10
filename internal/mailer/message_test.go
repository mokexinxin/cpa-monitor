package mailer

import (
	"bytes"
	"io"
	"mime"
	"mime/quotedprintable"
	"net/mail"
	"strings"
	"testing"
	"time"
)

func TestBuildMessageCreatesRFCMessage(t *testing.T) {
	t.Parallel()

	timestamp := time.Date(2026, time.July, 9, 3, 4, 5, 0, time.FixedZone("CST", 8*60*60))
	event := Event{
		Kind:      Alert,
		Object:    "账户 codex-user@example.com 配额不可用",
		Hostname:  "monitor-01",
		Timestamp: timestamp,
		Key:       "auth:account-7",
		Current:   "unavailable",
		Threshold: "active",
		Details:   "provider=codex\nstatus_message=额度已耗尽",
		BaseURL:   "http://127.0.0.1:8317",
	}

	raw, err := BuildMessage(
		"CPA 监控 <monitor@example.com>",
		[]string{"Admin One <one@example.com>", "two@example.com"},
		event,
	)
	if err != nil {
		t.Fatalf("BuildMessage() error = %v", err)
	}
	assertOnlyCRLF(t, raw)
	if !bytes.HasSuffix(raw, []byte("\r\n")) {
		t.Fatal("message does not end with CRLF")
	}

	msg, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("mail.ReadMessage() error = %v\n%s", err, raw)
	}
	if got := msg.Header.Get("From"); !strings.Contains(got, "monitor@example.com") {
		t.Fatalf("From = %q, want monitor@example.com", got)
	}
	if got := msg.Header.Get("To"); !strings.Contains(got, "one@example.com") || !strings.Contains(got, "two@example.com") {
		t.Fatalf("To = %q, want both recipients", got)
	}
	if got := msg.Header.Get("Message-ID"); !strings.HasPrefix(got, "<") || !strings.HasSuffix(got, "@cpa-monitor.local>") {
		t.Fatalf("Message-ID = %q, want cpa-monitor.local message id", got)
	}
	parsedDate, err := mail.ParseDate(msg.Header.Get("Date"))
	if err != nil {
		t.Fatalf("Date %q is invalid: %v", msg.Header.Get("Date"), err)
	}
	_, offset := parsedDate.Zone()
	if !parsedDate.Equal(timestamp) || offset != 0 {
		t.Fatalf("Date = %v, want timestamp represented in UTC", parsedDate)
	}
	if got, want := msg.Header.Get("MIME-Version"), "1.0"; got != want {
		t.Fatalf("MIME-Version = %q, want %q", got, want)
	}
	if got := msg.Header.Get("Content-Type"); !strings.EqualFold(got, "text/plain; charset=UTF-8") {
		t.Fatalf("Content-Type = %q, want UTF-8 text/plain", got)
	}
	if got, want := msg.Header.Get("Content-Transfer-Encoding"), "quoted-printable"; !strings.EqualFold(got, want) {
		t.Fatalf("Content-Transfer-Encoding = %q, want %q", got, want)
	}

	decodedSubject, err := new(mime.WordDecoder).DecodeHeader(msg.Header.Get("Subject"))
	if err != nil {
		t.Fatalf("decode Subject: %v", err)
	}
	if got, want := decodedSubject, "[CPA Monitor] ALERT "+event.Object; got != want {
		t.Fatalf("Subject = %q, want %q", got, want)
	}
	if !strings.Contains(string(raw), "=?utf-8?") && !strings.Contains(string(raw), "=?UTF-8?") {
		t.Fatalf("raw UTF-8 Subject is not encoded: %s", raw)
	}

	body, err := io.ReadAll(quotedprintable.NewReader(msg.Body))
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	for _, want := range []string{
		"Event: ALERT",
		"Host: monitor-01",
		"Timestamp: 2026-07-08T19:04:05Z",
		"Alert key: auth:account-7",
		"Current: unavailable",
		"Threshold: active",
		"Details:\r\nprovider=codex\r\nstatus_message=额度已耗尽",
		"CLIProxyAPI base URL: http://127.0.0.1:8317",
	} {
		if !bytes.Contains(body, []byte(want)) {
			t.Errorf("body does not contain %q:\n%s", want, body)
		}
	}
}

func TestBuildMessageRecoverySubject(t *testing.T) {
	t.Parallel()

	event := validEvent()
	event.Kind = Recovery
	event.Object = "disk / recovered"
	raw, err := BuildMessage("monitor@example.com", []string{"admin@example.com"}, event)
	if err != nil {
		t.Fatalf("BuildMessage() error = %v", err)
	}
	msg, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("mail.ReadMessage() error = %v", err)
	}
	if got, want := msg.Header.Get("Subject"), "[CPA Monitor] RECOVERY disk / recovered"; got != want {
		t.Fatalf("Subject = %q, want %q", got, want)
	}
}

func TestBuildMessageRejectsHeaderInjection(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		from  string
		to    []string
		event Event
	}{
		{name: "from", from: "monitor@example.com\r\nBcc: stolen@example.com", to: []string{"admin@example.com"}, event: validEvent()},
		{name: "recipient", from: "monitor@example.com", to: []string{"admin@example.com\nBcc: stolen@example.com"}, event: validEvent()},
		{name: "subject object", from: "monitor@example.com", to: []string{"admin@example.com"}, event: func() Event {
			e := validEvent()
			e.Object = "memory\r\nBcc: stolen@example.com"
			return e
		}()},
		{name: "subject tab", from: "monitor@example.com", to: []string{"admin@example.com"}, event: func() Event {
			e := validEvent()
			e.Object = "memory\tusage"
			return e
		}()},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if _, err := BuildMessage(tt.from, tt.to, tt.event); err == nil {
				t.Fatal("BuildMessage() error = nil, want header injection error")
			}
		})
	}
}

func TestBuildMessageValidatesEvent(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		mutate func(*Event)
	}{
		{name: "kind", mutate: func(e *Event) { e.Kind = Kind("UNKNOWN") }},
		{name: "object", mutate: func(e *Event) { e.Object = "" }},
		{name: "hostname", mutate: func(e *Event) { e.Hostname = "" }},
		{name: "timestamp", mutate: func(e *Event) { e.Timestamp = time.Time{} }},
		{name: "key", mutate: func(e *Event) { e.Key = "" }},
		{name: "base URL", mutate: func(e *Event) { e.BaseURL = "" }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			event := validEvent()
			tt.mutate(&event)
			if _, err := BuildMessage("monitor@example.com", []string{"admin@example.com"}, event); err == nil {
				t.Fatal("BuildMessage() error = nil, want validation error")
			}
		})
	}
}

func validEvent() Event {
	return Event{
		Kind:      Alert,
		Object:    "memory usage 84.2% on host",
		Hostname:  "monitor-01",
		Timestamp: time.Date(2026, time.July, 9, 3, 4, 5, 0, time.UTC),
		Key:       "resource:memory",
		Current:   "84.2%",
		Threshold: "80.0%",
		Details:   "used=842 MiB total=1000 MiB",
		BaseURL:   "http://127.0.0.1:8317",
	}
}

func assertOnlyCRLF(t *testing.T, data []byte) {
	t.Helper()
	for i, b := range data {
		switch b {
		case '\n':
			if i == 0 || data[i-1] != '\r' {
				t.Fatalf("message contains bare LF at byte %d", i)
			}
		case '\r':
			if i+1 >= len(data) || data[i+1] != '\n' {
				t.Fatalf("message contains bare CR at byte %d", i)
			}
		}
	}
}
