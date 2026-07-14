// Package healthreport schedules healthy-status notifications and persists
// their delivery times so service restarts do not produce duplicate reports.
package healthreport

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/mokexinxin/cpa-monitor/internal/cliproxy"
	"github.com/mokexinxin/cpa-monitor/internal/monitor"
	"github.com/mokexinxin/cpa-monitor/internal/notification"
	"github.com/mokexinxin/cpa-monitor/internal/state"
)

type Sender = notification.HealthSender

// QuotaFetcher retrieves provider plan limits only when a scheduled report is
// due, avoiding one upstream quota request per monitor cycle.
type QuotaFetcher interface {
	CodexQuota(context.Context, string, string) (cliproxy.CodexQuota, error)
}

type Store interface {
	HealthReport() state.HealthReportState
	SetHealthReport(state.HealthReportState) error
	Save() error
}

type Options struct {
	Enabled       bool
	Interval      time.Duration
	RetryInterval time.Duration
	Hostname      string
	BaseURL       string
	Logger        *slog.Logger
	QuotaFetcher  QuotaFetcher
}

type Manager struct {
	mu            sync.Mutex
	sender        Sender
	store         Store
	enabled       bool
	interval      time.Duration
	retryInterval time.Duration
	hostname      string
	baseURL       string
	logger        *slog.Logger
	quotaFetcher  QuotaFetcher
	now           func() time.Time
}

func New(sender Sender, store Store, options Options) (*Manager, error) {
	if sender == nil {
		return nil, errors.New("health report: nil sender")
	}
	if store == nil {
		return nil, errors.New("health report: nil state store")
	}
	if options.Interval <= 0 {
		return nil, errors.New("health report: interval must be greater than zero")
	}
	if options.RetryInterval <= 0 {
		return nil, errors.New("health report: retry interval must be greater than zero")
	}
	if options.Hostname == "" {
		return nil, errors.New("health report: hostname must not be empty")
	}
	if options.BaseURL == "" {
		return nil, errors.New("health report: base URL must not be empty")
	}
	logger := options.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &Manager{
		sender:        sender,
		store:         store,
		enabled:       options.Enabled,
		interval:      options.Interval,
		retryInterval: options.RetryInterval,
		hostname:      options.Hostname,
		baseURL:       options.BaseURL,
		logger:        logger,
		quotaFetcher:  options.QuotaFetcher,
		now:           time.Now,
	}, nil
}

// ReportHealthy sends immediately on the first healthy cycle, then at the
// configured interval. A failed delivery is retried only after RetryInterval.
func (m *Manager) ReportHealthy(ctx context.Context, snapshot monitor.HealthSnapshot) error {
	if !m.enabled {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	now := m.now().UTC()
	current := m.store.HealthReport()
	if !due(current, now, m.interval, m.retryInterval) {
		return nil
	}

	report := notification.HealthReport{
		Hostname:               m.hostname,
		Timestamp:              now,
		NextScheduledAt:        now.Add(m.interval),
		BaseURL:                m.baseURL,
		MemoryUsedPercent:      snapshot.MemoryUsedPercent,
		MemoryThreshold:        snapshot.MemoryThreshold,
		HighestDiskUsedPercent: snapshot.HighestDiskUsedPercent,
		DiskMountCount:         snapshot.DiskMountCount,
		DiskThreshold:          snapshot.DiskThreshold,
		TotalTCPConnections:    snapshot.TotalTCPConnections,
		TotalTCPThreshold:      snapshot.TotalTCPThreshold,
		ServicePort:            snapshot.ServicePort,
		ServicePortConnections: snapshot.ServicePortConnections,
		ServicePortThreshold:   snapshot.ServicePortThreshold,
		AccountUsageAvailable:  snapshot.AccountUsageAvailable,
		AccountCount:           snapshot.AccountCount,
		EnabledAccountCount:    snapshot.EnabledAccountCount,
		AccountUsages:          make([]notification.AccountUsage, len(snapshot.AccountUsages)),
	}
	for i, usage := range snapshot.AccountUsages {
		report.AccountUsages[i] = notification.AccountUsage{
			Label:         usage.Label,
			Provider:      usage.Provider,
			Success:       usage.Success,
			Failed:        usage.Failed,
			RecentSuccess: usage.RecentSuccess,
			RecentFailed:  usage.RecentFailed,
		}
		m.addQuota(ctx, usage, &report.AccountUsages[i])
	}
	sendErr := m.sender.SendHealth(ctx, report)
	current.LastAttemptAt = now
	if sendErr == nil {
		current.LastSentAt = now
	}
	if err := m.store.SetHealthReport(current); err != nil {
		return errors.Join(sendErr, fmt.Errorf("update health report state: %w", err))
	}
	if err := m.store.Save(); err != nil {
		return errors.Join(sendErr, fmt.Errorf("save health report state: %w", err))
	}
	if sendErr == nil {
		m.logger.InfoContext(ctx, "healthy report sent", "next_scheduled_at", report.NextScheduledAt.UTC().Format(time.RFC3339))
	}
	return sendErr
}

func (m *Manager) addQuota(ctx context.Context, source monitor.AccountUsage, target *notification.AccountUsage) {
	if target == nil || !strings.EqualFold(strings.TrimSpace(source.Provider), "codex") {
		return
	}
	target.QuotaSupported = true
	if m.quotaFetcher == nil {
		m.logger.WarnContext(ctx, "account quota unavailable", "provider", "codex", "account", source.Label, "reason", "quota fetcher is not configured")
		return
	}
	quota, err := m.quotaFetcher.CodexQuota(ctx, source.AuthIndex, source.AccountID)
	if err != nil {
		m.logger.WarnContext(ctx, "account quota unavailable", "provider", "codex", "account", source.Label, "error", err)
		return
	}
	candidate := *target
	candidate.PlanType = quota.PlanType
	candidate.QuotaAvailable = true
	candidate.QuotaWindows = make([]notification.QuotaWindow, len(quota.Windows))
	for i, window := range quota.Windows {
		candidate.QuotaWindows[i] = notification.QuotaWindow{
			Kind:        string(window.Kind),
			UsedPercent: cloneFloat64(window.UsedPercent),
			ResetAt:     window.ResetAt,
			ResetAfter:  window.ResetAfter,
		}
	}
	if err := notification.ValidateAccountUsage(candidate); err != nil {
		m.logger.WarnContext(ctx, "account quota unavailable", "provider", "codex", "account", source.Label, "error", err)
		return
	}
	*target = candidate
}

func cloneFloat64(value *float64) *float64 {
	if value == nil {
		return nil
	}
	cloned := *value
	return &cloned
}

func due(current state.HealthReportState, now time.Time, interval, retryInterval time.Duration) bool {
	if current.LastSentAt.IsZero() {
		return current.LastAttemptAt.IsZero() || !now.Before(current.LastAttemptAt.Add(retryInterval))
	}
	if now.Before(current.LastSentAt.Add(interval)) {
		return false
	}
	if current.LastAttemptAt.After(current.LastSentAt) && now.Before(current.LastAttemptAt.Add(retryInterval)) {
		return false
	}
	return true
}
