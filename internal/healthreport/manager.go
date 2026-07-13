// Package healthreport schedules healthy-status notifications and persists
// their delivery times so service restarts do not produce duplicate reports.
package healthreport

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/mokexinxin/cpa-monitor/internal/monitor"
	"github.com/mokexinxin/cpa-monitor/internal/notification"
	"github.com/mokexinxin/cpa-monitor/internal/state"
)

type Sender = notification.HealthSender

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
		AccountCount:           snapshot.AccountCount,
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
