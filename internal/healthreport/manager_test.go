package healthreport

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/mokexinxin/cpa-monitor/internal/cliproxy"
	"github.com/mokexinxin/cpa-monitor/internal/monitor"
	"github.com/mokexinxin/cpa-monitor/internal/notification"
	"github.com/mokexinxin/cpa-monitor/internal/state"
)

func TestManagerSendsFirstHealthyCycleThenAtIntervalAcrossRestart(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "alerts.json")
	store, err := state.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	sender := &recordingSender{}
	start := time.Date(2026, 7, 11, 5, 0, 0, 0, time.UTC)
	manager := newTestManager(t, sender, store, start)

	if err := manager.ReportHealthy(context.Background(), testSnapshot()); err != nil {
		t.Fatal(err)
	}
	if len(sender.reports) != 1 {
		t.Fatalf("reports = %d, want immediate first report", len(sender.reports))
	}
	if got := sender.reports[0]; got.Hostname != "monitor-01" || got.MemoryUsedPercent != 42.5 || !got.NextScheduledAt.Equal(start.Add(24*time.Hour)) || !got.AccountUsageAvailable || got.EnabledAccountCount != 2 || len(got.AccountUsages) != 2 || got.AccountUsages[0].Label != "one@example.test" || got.AccountUsages[0].RecentSuccess != 2 || !got.AccountUsages[0].QuotaAvailable || len(got.AccountUsages[0].QuotaWindows) != 2 || got.AccountUsages[0].QuotaWindows[1].Kind != "weekly" {
		t.Fatalf("report = %#v", got)
	}
	if calls := manager.quotaFetcher.(*recordingQuotaFetcher).calls; len(calls) != 1 || calls[0] != "auth-one/account-one" {
		t.Fatalf("quota calls = %#v", calls)
	}

	manager.now = func() time.Time { return start.Add(23 * time.Hour) }
	if err := manager.ReportHealthy(context.Background(), testSnapshot()); err != nil {
		t.Fatal(err)
	}
	if len(sender.reports) != 1 {
		t.Fatalf("reports before interval = %d, want 1", len(sender.reports))
	}
	if calls := manager.quotaFetcher.(*recordingQuotaFetcher).calls; len(calls) != 1 {
		t.Fatalf("quota calls before interval = %#v, want no additional lookup", calls)
	}

	reloaded, err := state.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	restarted := newTestManager(t, sender, reloaded, start.Add(24*time.Hour))
	if err := restarted.ReportHealthy(context.Background(), testSnapshot()); err != nil {
		t.Fatal(err)
	}
	if len(sender.reports) != 2 {
		t.Fatalf("reports after interval and restart = %d, want 2", len(sender.reports))
	}
}

func TestManagerQuotaFailureDoesNotBlockServerReport(t *testing.T) {
	t.Parallel()
	sender := &recordingSender{}
	store := state.New(filepath.Join(t.TempDir(), "alerts.json"))
	manager := newTestManager(t, sender, store, time.Date(2026, 7, 14, 5, 0, 0, 0, time.UTC))
	manager.quotaFetcher = &recordingQuotaFetcher{err: errors.New("usage endpoint unavailable")}

	if err := manager.ReportHealthy(context.Background(), testSnapshot()); err != nil {
		t.Fatal(err)
	}
	if len(sender.reports) != 1 {
		t.Fatalf("reports = %d, want 1", len(sender.reports))
	}
	usage := sender.reports[0].AccountUsages[0]
	if !usage.QuotaSupported || usage.QuotaAvailable || len(usage.QuotaWindows) != 0 {
		t.Fatalf("Codex usage after quota failure = %#v", usage)
	}
}

func TestManagerBacksOffAfterSendFailure(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("SMTP unavailable")
	sender := &recordingSender{err: sentinel}
	store := state.New(filepath.Join(t.TempDir(), "alerts.json"))
	start := time.Date(2026, 7, 11, 5, 0, 0, 0, time.UTC)
	manager := newTestManager(t, sender, store, start)

	if err := manager.ReportHealthy(context.Background(), testSnapshot()); !errors.Is(err, sentinel) {
		t.Fatalf("first error = %v", err)
	}
	manager.now = func() time.Time { return start.Add(14 * time.Minute) }
	if err := manager.ReportHealthy(context.Background(), testSnapshot()); err != nil {
		t.Fatalf("backoff call error = %v", err)
	}
	if len(sender.reports) != 1 {
		t.Fatalf("reports during backoff = %d, want 1", len(sender.reports))
	}

	sender.err = nil
	manager.now = func() time.Time { return start.Add(15 * time.Minute) }
	if err := manager.ReportHealthy(context.Background(), testSnapshot()); err != nil {
		t.Fatal(err)
	}
	if len(sender.reports) != 2 {
		t.Fatalf("reports after retry interval = %d, want 2", len(sender.reports))
	}
	got := store.HealthReport()
	if !got.LastSentAt.Equal(start.Add(15*time.Minute)) || !got.LastAttemptAt.Equal(got.LastSentAt) {
		t.Fatalf("state = %#v", got)
	}
}

func TestDisabledManagerDoesNothing(t *testing.T) {
	t.Parallel()
	sender := &recordingSender{}
	store := state.New(filepath.Join(t.TempDir(), "alerts.json"))
	manager := newTestManager(t, sender, store, time.Now())
	manager.enabled = false
	if err := manager.ReportHealthy(context.Background(), testSnapshot()); err != nil {
		t.Fatal(err)
	}
	if len(sender.reports) != 0 {
		t.Fatalf("reports = %d", len(sender.reports))
	}
}

type recordingSender struct {
	reports []notification.HealthReport
	err     error
}

func (s *recordingSender) SendHealth(_ context.Context, report notification.HealthReport) error {
	s.reports = append(s.reports, report)
	return s.err
}

func newTestManager(t *testing.T, sender Sender, store Store, now time.Time) *Manager {
	t.Helper()
	manager, err := New(sender, store, Options{
		Enabled:       true,
		Interval:      24 * time.Hour,
		RetryInterval: 15 * time.Minute,
		Hostname:      "monitor-01",
		BaseURL:       "http://127.0.0.1:8317",
		QuotaFetcher:  &recordingQuotaFetcher{},
	})
	if err != nil {
		t.Fatal(err)
	}
	manager.now = func() time.Time { return now }
	return manager
}

func testSnapshot() monitor.HealthSnapshot {
	return monitor.HealthSnapshot{
		MemoryUsedPercent:      42.5,
		MemoryThreshold:        80,
		HighestDiskUsedPercent: 51.2,
		DiskMountCount:         2,
		DiskThreshold:          80,
		TotalTCPConnections:    19,
		TotalTCPThreshold:      3000,
		ServicePort:            8317,
		ServicePortConnections: 11,
		ServicePortThreshold:   800,
		AccountUsageAvailable:  true,
		AccountCount:           3,
		EnabledAccountCount:    2,
		AccountUsages: []monitor.AccountUsage{
			{AuthIndex: "auth-one", AccountID: "account-one", Label: "one@example.test", Provider: "codex", Success: 12, Failed: 1, RecentSuccess: 2},
			{Label: "team-two", Provider: "claude", Success: 4, RecentFailed: 1},
		},
	}
}

type recordingQuotaFetcher struct {
	calls []string
	err   error
}

func (f *recordingQuotaFetcher) CodexQuota(_ context.Context, authIndex, accountID string) (cliproxy.CodexQuota, error) {
	f.calls = append(f.calls, authIndex+"/"+accountID)
	if f.err != nil {
		return cliproxy.CodexQuota{}, f.err
	}
	fiveHour, weekly := 12.5, 47.25
	return cliproxy.CodexQuota{
		PlanType: "plus",
		Windows: []cliproxy.QuotaWindow{
			{Kind: cliproxy.QuotaWindowFiveHour, UsedPercent: &fiveHour, ResetAfter: 10 * time.Minute},
			{Kind: cliproxy.QuotaWindowWeekly, UsedPercent: &weekly, ResetAt: time.Unix(1784000000, 0).UTC()},
		},
	}, nil
}
