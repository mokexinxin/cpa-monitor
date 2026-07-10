package monitor

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"testing"

	"github.com/mokexinxin/cpa-monitor/internal/alerter"
	"github.com/mokexinxin/cpa-monitor/internal/cliproxy"
	"github.com/mokexinxin/cpa-monitor/internal/collector"
	"github.com/mokexinxin/cpa-monitor/internal/mailer"
	"github.com/mokexinxin/cpa-monitor/internal/state"
)

func TestIntegrationAlertLifecyclePersistsAndManagementFailureDoesNotRecover(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "state", "alerts.json")
	api := &lifecycleAPI{files: unavailableAccount()}
	host := &lifecycleHost{disks: collector.DiskBatch{Complete: true}}
	sender := &lifecycleSender{}
	store := state.New(path)
	runner := newLifecycleRunner(t, api, host, sender, store)

	if err := runner.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if got := len(sender.events); got != 1 {
		t.Fatalf("first cycle events = %d", got)
	}
	if _, ok := store.Get("auth:account-1"); !ok {
		t.Fatal("auth alert was not activated")
	}
	if err := runner.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if got := len(sender.events); got != 1 {
		t.Fatalf("ongoing alert resent, events = %d", got)
	}

	// Rebuild the state/manager/runner as a process restart would. The persisted
	// active key must still suppress the ongoing condition.
	reloaded, err := state.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	runner = newLifecycleRunner(t, api, host, sender, reloaded)
	if err := runner.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if got := len(sender.events); got != 1 {
		t.Fatalf("restart resent ongoing alert, events = %d", got)
	}

	api.authErr = errors.New("management API unavailable")
	if err := runner.RunOnce(ctx); err == nil {
		t.Fatal("expected management check error")
	}
	if _, ok := reloaded.Get("auth:account-1"); !ok {
		t.Fatal("unknown management result recovered active auth alert")
	}

	api.authErr = nil
	api.files = []cliproxy.AuthFile{{AuthIndex: "account-1", Status: "active"}}
	if err := runner.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if _, ok := reloaded.Get("auth:account-1"); ok {
		t.Fatal("healthy account did not recover")
	}
	if got := len(sender.events); got != 1 {
		t.Fatalf("recovery email sent while disabled, events = %d", got)
	}

	api.files = unavailableAccount()
	if err := runner.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if got := len(sender.events); got != 2 {
		t.Fatalf("new unhealthy transition events = %d, want 2", got)
	}
}

func TestIntegrationFailedSendRemainsEligibleForRetry(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "state", "alerts.json")
	api := &lifecycleAPI{}
	host := &lifecycleHost{
		memory: collector.MemoryUsage{TotalBytes: 100, AvailableBytes: 10, UsedBytes: 90, UsedPercent: 90},
		disks:  collector.DiskBatch{Complete: true},
	}
	sender := &lifecycleSender{failRemaining: 1}
	store := state.New(path)
	runner := newLifecycleRunner(t, api, host, sender, store)

	if err := runner.RunOnce(context.Background()); err == nil {
		t.Fatal("expected first SMTP send error")
	}
	if _, ok := store.Get("resource:memory"); ok {
		t.Fatal("failed alert send activated state")
	}
	if err := runner.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if sender.attempts != 2 || len(sender.events) != 1 {
		t.Fatalf("attempts/events = %d/%d", sender.attempts, len(sender.events))
	}
	if _, ok := store.Get("resource:memory"); !ok {
		t.Fatal("successful retry did not activate state")
	}
}

func newLifecycleRunner(t *testing.T, api *lifecycleAPI, host *lifecycleHost, sender *lifecycleSender, store *state.File) *Runner {
	t.Helper()
	manager := alerter.NewManager(sender, store, "integration-host", "http://127.0.0.1:8317", false)
	runner, err := NewRunner(Options{
		API: api, Collector: host, Reconciler: manager,
		Logger:                 slog.New(slog.NewTextHandler(io.Discard, nil)),
		ServicePort:            8317,
		MemoryPercent:          80,
		DiskPercent:            80,
		TotalTCPConnections:    3000,
		ServicePortConnections: 800,
	})
	if err != nil {
		t.Fatal(err)
	}
	return runner
}

func unavailableAccount() []cliproxy.AuthFile {
	return []cliproxy.AuthFile{{AuthIndex: "account-1", Email: "user@example.com", Unavailable: true}}
}

type lifecycleAPI struct {
	files   []cliproxy.AuthFile
	authErr error
}

func (*lifecycleAPI) CheckHealth(context.Context) error { return nil }

func (a *lifecycleAPI) AuthFiles(context.Context) ([]cliproxy.AuthFile, error) {
	return a.files, a.authErr
}

type lifecycleHost struct {
	memory collector.MemoryUsage
	disks  collector.DiskBatch
	tcp    collector.TCPUsage
}

func (h *lifecycleHost) Memory(context.Context) (collector.MemoryUsage, error) {
	return h.memory, nil
}

func (h *lifecycleHost) Disks(context.Context) (collector.DiskBatch, error) {
	return h.disks, nil
}

func (h *lifecycleHost) TCP(context.Context, int) (collector.TCPUsage, error) {
	return h.tcp, nil
}

type lifecycleSender struct {
	failRemaining int
	attempts      int
	events        []mailer.Event
}

func (s *lifecycleSender) Send(_ context.Context, event mailer.Event) error {
	s.attempts++
	if s.failRemaining > 0 {
		s.failRemaining--
		return errors.New("SMTP unavailable")
	}
	s.events = append(s.events, event)
	return nil
}
