// Package notification defines transport-neutral monitoring messages.
package notification

import (
	"context"
	"errors"
	"fmt"
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
	AccountCount           int
	EnabledAccountCount    int
	AccountUsages          []AccountUsage
}

// AccountUsage is the transport-neutral request-usage summary for one enabled
// CLIProxyAPI account.
type AccountUsage struct {
	Label         string
	Provider      string
	Success       int64
	Failed        int64
	RecentSuccess int64
	RecentFailed  int64
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
