package notification

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestValidateBatch(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 13, 1, 2, 3, 0, time.UTC)
	valid := Batch{Kind: Alert, Scope: "memory", Hostname: "host", Timestamp: now, Events: []Event{{
		Kind: Alert, Scope: "memory", Hostname: "host", Timestamp: now, Key: "resource:memory",
	}}}
	if err := ValidateBatch(valid); err != nil {
		t.Fatalf("valid batch: %v", err)
	}
	for name, mutate := range map[string]func(*Batch){
		"kind":       func(b *Batch) { b.Kind = "BAD" },
		"scope":      func(b *Batch) { b.Scope = "" },
		"hostname":   func(b *Batch) { b.Hostname = "" },
		"timestamp":  func(b *Batch) { b.Timestamp = time.Time{} },
		"events":     func(b *Batch) { b.Events = nil },
		"event kind": func(b *Batch) { b.Events[0].Kind = Recovery },
		"event scope": func(b *Batch) {
			b.Events[0].Scope = "disk"
		},
		"duplicate": func(b *Batch) { b.Events = append(b.Events, b.Events[0]) },
	} {
		t.Run(name, func(t *testing.T) {
			copy := valid
			copy.Events = append([]Event(nil), valid.Events...)
			mutate(&copy)
			if err := ValidateBatch(copy); err == nil {
				t.Fatal("ValidateBatch() error = nil")
			}
		})
	}
}

type alertSenderFunc func(context.Context, Batch) error

func (f alertSenderFunc) SendBatch(ctx context.Context, batch Batch) error { return f(ctx, batch) }

func TestRouterPrimaryAndFallback(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("primary down")
	primaryCalls, fallbackCalls := 0, 0
	router, err := NewRouter(
		NamedAlertSender{Name: "dingtalk", Sender: alertSenderFunc(func(context.Context, Batch) error {
			primaryCalls++
			return sentinel
		})},
		&NamedAlertSender{Name: "smtp", Sender: alertSenderFunc(func(context.Context, Batch) error {
			fallbackCalls++
			return nil
		})},
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := router.SendBatch(context.Background(), Batch{Scope: "memory"}); err != nil {
		t.Fatal(err)
	}
	if primaryCalls != 1 || fallbackCalls != 1 {
		t.Fatalf("calls = %d/%d", primaryCalls, fallbackCalls)
	}
}
