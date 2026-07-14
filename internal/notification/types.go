// Package notification defines transport-neutral monitoring messages.
package notification

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"
)

// Kind identifies whether a batch announces newly active alerts or recoveries.
type Kind string

const (
	Alert    Kind = "ALERT"
	Recovery Kind = "RECOVERY"
)

// Event contains transport-independent alert content.
type Event struct {
	Kind      Kind
	Scope     string
	Object    string
	Hostname  string
	Timestamp time.Time
	Key       string
	Current   string
	Threshold string
	Details   string
	BaseURL   string
}

// Batch groups events of one kind and monitoring scope into one delivery.
type Batch struct {
	Kind      Kind
	Scope     string
	Hostname  string
	Timestamp time.Time
	Events    []Event
}

// HealthReport is the complete data model for a healthy-status notification.
type HealthReport struct {
	Hostname               string
	Timestamp              time.Time
	NextScheduledAt        time.Time
	BaseURL                string
	MemoryUsedPercent      float64
	MemoryThreshold        float64
	HighestDiskUsedPercent float64
	DiskMountCount         int
	DiskThreshold          float64
	TotalTCPConnections    int
	TotalTCPThreshold      int
	ServicePort            int
	ServicePortConnections int
	ServicePortThreshold   int
	AccountUsageAvailable  bool
	AccountCount           int
	EnabledAccountCount    int
	AccountUsages          []AccountUsage
	VersionCheckAvailable  bool
	CurrentVersion         string
	LatestVersion          string
	VersionComparable      bool
	UpdateAvailable        bool
	ReleaseURL             string
}

// AccountUsage is the transport-neutral request-usage summary for one enabled
// CLIProxyAPI account.
type AccountUsage struct {
	Label          string
	Provider       string
	PlanType       string
	QuotaSupported bool
	QuotaAvailable bool
	QuotaWindows   []QuotaWindow
	Success        int64
	Failed         int64
	RecentSuccess  int64
	RecentFailed   int64
}

// QuotaWindow is a provider plan-limit window displayed in an account usage
// section. UsedPercent may be absent when the provider only reports reset
// state.
type QuotaWindow struct {
	Kind        string
	UsedPercent *float64
	ResetAt     time.Time
	ResetAfter  time.Duration
}

// ValidateAccountUsage checks the shared account and quota invariants before
// a transport renders untrusted management/provider data.
func ValidateAccountUsage(usage AccountUsage) error {
	if strings.TrimSpace(usage.Label) == "" || usage.Success < 0 || usage.Failed < 0 || usage.RecentSuccess < 0 || usage.RecentFailed < 0 {
		return errors.New("account usage identity and counters are invalid")
	}
	if !usage.QuotaSupported && (usage.QuotaAvailable || strings.TrimSpace(usage.PlanType) != "" || len(usage.QuotaWindows) != 0) {
		return errors.New("unsupported account quota contains provider data")
	}
	if usage.QuotaAvailable && (!usage.QuotaSupported || len(usage.QuotaWindows) == 0) {
		return errors.New("available account quota requires at least one window")
	}
	if !usage.QuotaAvailable && len(usage.QuotaWindows) != 0 {
		return errors.New("unavailable account quota contains windows")
	}
	for _, window := range usage.QuotaWindows {
		if strings.TrimSpace(window.Kind) == "" || window.ResetAfter < 0 {
			return errors.New("account quota window kind or reset duration is invalid")
		}
		if window.UsedPercent != nil {
			value := *window.UsedPercent
			if math.IsNaN(value) || math.IsInf(value, 0) || value < 0 || value > 100 {
				return errors.New("account quota used percent must be between 0 and 100")
			}
		}
	}
	return nil
}

// ValidateVersionInfo checks the transport-neutral CLIProxyAPI update block.
func ValidateVersionInfo(report HealthReport) error {
	if strings.TrimSpace(report.ReleaseURL) == "" {
		return errors.New("CLIProxyAPI release URL is required")
	}
	if !report.VersionCheckAvailable {
		if report.VersionComparable || report.UpdateAvailable || strings.TrimSpace(report.CurrentVersion) != "" || strings.TrimSpace(report.LatestVersion) != "" {
			return errors.New("unavailable CLIProxyAPI version check contains version data")
		}
		return nil
	}
	if strings.TrimSpace(report.CurrentVersion) == "" || strings.TrimSpace(report.LatestVersion) == "" {
		return errors.New("available CLIProxyAPI version check requires current and latest versions")
	}
	if report.UpdateAvailable && !report.VersionComparable {
		return errors.New("CLIProxyAPI update status requires a comparable version")
	}
	return nil
}

// AlertSender sends one already-aggregated alert or recovery batch.
type AlertSender interface {
	SendBatch(context.Context, Batch) error
}

// HealthSender sends one scheduled healthy-status report.
type HealthSender interface {
	SendHealth(context.Context, HealthReport) error
}

// ValidateBatch checks transport-neutral invariants before a sender performs
// external I/O.
func ValidateBatch(batch Batch) error {
	if batch.Kind != Alert && batch.Kind != Recovery {
		return fmt.Errorf("notification batch kind %q is invalid", batch.Kind)
	}
	if strings.TrimSpace(batch.Scope) == "" {
		return errors.New("notification batch scope is required")
	}
	if strings.TrimSpace(batch.Hostname) == "" {
		return errors.New("notification batch hostname is required")
	}
	if batch.Timestamp.IsZero() {
		return errors.New("notification batch timestamp is required")
	}
	if len(batch.Events) == 0 {
		return errors.New("notification batch must contain at least one event")
	}
	seen := make(map[string]struct{}, len(batch.Events))
	for i, event := range batch.Events {
		if event.Kind != batch.Kind {
			return fmt.Errorf("notification event %d kind does not match batch", i+1)
		}
		if event.Scope != batch.Scope {
			return fmt.Errorf("notification event %d scope does not match batch", i+1)
		}
		if event.Hostname != batch.Hostname {
			return fmt.Errorf("notification event %d hostname does not match batch", i+1)
		}
		if !event.Timestamp.Equal(batch.Timestamp) {
			return fmt.Errorf("notification event %d timestamp does not match batch", i+1)
		}
		if strings.TrimSpace(event.Key) == "" {
			return fmt.Errorf("notification event %d key is required", i+1)
		}
		if _, duplicate := seen[event.Key]; duplicate {
			return fmt.Errorf("notification batch contains duplicate key %q", event.Key)
		}
		seen[event.Key] = struct{}{}
	}
	return nil
}
