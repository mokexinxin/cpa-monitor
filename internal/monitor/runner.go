package monitor

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"reflect"
	"strings"

	"github.com/mokexinxin/cpa-monitor/internal/cliproxy"
	"github.com/mokexinxin/cpa-monitor/internal/collector"
	"github.com/mokexinxin/cpa-monitor/internal/rule"
)

// APIClient is the subset of the CLIProxyAPI client required for one cycle.
type APIClient interface {
	CheckHealth(context.Context) error
	AuthFiles(context.Context) ([]cliproxy.AuthFile, error)
}

// Reconciler applies one rule scope to alert state and delivery.
type Reconciler interface {
	Reconcile(context.Context, rule.Batch) error
}

// HealthReporter receives a snapshot after the four server scopes are
// complete, error-free, and healthy. Account monitoring is independent and
// never blocks the scheduled server-status report.
type HealthReporter interface {
	ReportHealthy(context.Context, HealthSnapshot) error
}

// HealthSnapshot contains the facts displayed in a scheduled health email.
type HealthSnapshot struct {
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
}

// AccountUsage contains the request counters exposed by CLIProxyAPI for one
// enabled account. Success and Failed are process-lifetime counters; the
// recent values are summed from the rolling recent_requests window.
type AccountUsage struct {
	Label         string
	Provider      string
	Success       int64
	Failed        int64
	RecentSuccess int64
	RecentFailed  int64
}

// Options contains Runner dependencies and already-validated configuration
// values. NewRunner still validates the numeric invariants so a Runner cannot
// silently turn an invalid threshold into misleading recovery state.
type Options struct {
	API            APIClient
	Collector      collector.HostCollector
	Reconciler     Reconciler
	HealthReporter HealthReporter
	Logger         *slog.Logger

	ServicePort            int
	MemoryPercent          float64
	DiskPercent            float64
	TotalTCPConnections    int
	ServicePortConnections int
}

// Runner executes all five monitoring scopes once in deterministic order.
type Runner struct {
	api            APIClient
	collector      collector.HostCollector
	reconciler     Reconciler
	healthReporter HealthReporter
	logger         *slog.Logger

	servicePort            int
	memoryPercent          float64
	diskPercent            float64
	totalTCPConnections    int
	servicePortConnections int
}

// NewRunner validates dependencies and constructs a cycle runner.
func NewRunner(options Options) (*Runner, error) {
	if nilInterface(options.API) {
		return nil, errors.New("monitor: nil API client")
	}
	if nilInterface(options.Collector) {
		return nil, errors.New("monitor: nil host collector")
	}
	if nilInterface(options.Reconciler) {
		return nil, errors.New("monitor: nil reconciler")
	}
	if options.HealthReporter != nil && nilInterface(options.HealthReporter) {
		return nil, errors.New("monitor: typed nil health reporter")
	}
	if options.ServicePort < 1 || options.ServicePort > 65535 {
		return nil, errors.New("monitor: service port must be between 1 and 65535")
	}
	if err := validateRunnerPercent("memory", options.MemoryPercent); err != nil {
		return nil, err
	}
	if err := validateRunnerPercent("disk", options.DiskPercent); err != nil {
		return nil, err
	}
	if options.TotalTCPConnections <= 0 {
		return nil, errors.New("monitor: total TCP connection threshold must be greater than zero")
	}
	if options.ServicePortConnections <= 0 {
		return nil, errors.New("monitor: service-port connection threshold must be greater than zero")
	}
	logger := options.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &Runner{
		api:                    options.API,
		collector:              options.Collector,
		reconciler:             options.Reconciler,
		healthReporter:         options.HealthReporter,
		logger:                 logger,
		servicePort:            options.ServicePort,
		memoryPercent:          options.MemoryPercent,
		diskPercent:            options.DiskPercent,
		totalTCPConnections:    options.TotalTCPConnections,
		servicePortConnections: options.ServicePortConnections,
	}, nil
}

func nilInterface(value any) bool {
	if value == nil {
		return true
	}
	reflected := reflect.ValueOf(value)
	switch reflected.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return reflected.IsNil()
	default:
		return false
	}
}

func validateRunnerPercent(name string, value float64) error {
	if math.IsNaN(value) || math.IsInf(value, 0) || value <= 0 || value > 100 {
		return fmt.Errorf("monitor: %s threshold must be greater than 0 and at most 100", name)
	}
	return nil
}

// RunOnce checks health, memory, disks, TCP, and accounts in that order. A
// scope failure is reconciled as incomplete and does not block later scopes.
// Outer context cancellation is terminal and never becomes a health-down
// condition.
func (r *Runner) RunOnce(ctx context.Context) error {
	if ctx == nil {
		return errors.New("monitor: nil context")
	}
	if r == nil || r.api == nil || r.collector == nil || r.reconciler == nil {
		return errors.New("monitor: runner is not initialized")
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	var runErrors []error

	healthErr := r.api.CheckHealth(ctx)
	if err := ctx.Err(); err != nil {
		return joinContextError(runErrors, err)
	}
	healthBatch := rule.Health(healthErr)
	if healthErr != nil {
		// Do not log the underlying HTTP error here. It is retained in the
		// condition for delivery, while the log message remains safe even if a
		// custom APIClient returns an error containing a credential.
		r.logger.WarnContext(ctx, "monitor condition detected",
			"check", "health",
			"scope", rule.ScopeHealth,
		)
	}
	r.reconcile(ctx, "health", healthBatch, &runErrors)
	if err := ctx.Err(); err != nil {
		return joinContextError(runErrors, err)
	}

	memory, memoryErr := r.collector.Memory(ctx)
	if err := ctx.Err(); err != nil {
		return joinContextError(runErrors, err)
	}
	var memoryBatch rule.Batch
	if memoryErr != nil {
		memoryBatch = incompleteBatch(rule.ScopeMemory, memoryErr)
		r.recordRuntimeError(ctx, "memory", rule.ScopeMemory, memoryErr, &runErrors)
	} else {
		memoryBatch = rule.Memory(memory, r.memoryPercent)
	}
	r.reconcile(ctx, "memory", memoryBatch, &runErrors)
	if err := ctx.Err(); err != nil {
		return joinContextError(runErrors, err)
	}

	disks, diskErr := r.collector.Disks(ctx)
	if err := ctx.Err(); err != nil {
		return joinContextError(runErrors, err)
	}
	if diskErr != nil {
		// A collector error always makes the scope incomplete, even if an
		// injected implementation accidentally returns Complete=true. Preserve
		// successful mount facts so they can still produce new alerts.
		disks.Complete = false
		if len(disks.Errors) == 0 {
			disks.Errors = []collector.DiskError{{Err: diskErr}}
		}
	}
	diskBatch := rule.Disks(disks, r.diskPercent)
	if diskErr != nil {
		r.recordRuntimeError(ctx, "disk", rule.ScopeDisk, diskErr, &runErrors)
	} else if entryErr := diskBatch.Err(); entryErr != nil {
		// Be defensive about injected collectors that report partial errors in
		// the batch but forget to return the joined error separately.
		r.recordRuntimeError(ctx, "disk", rule.ScopeDisk, entryErr, &runErrors)
	}
	r.reconcile(ctx, "disk", diskBatch, &runErrors)
	if err := ctx.Err(); err != nil {
		return joinContextError(runErrors, err)
	}

	tcp, tcpErr := r.collector.TCP(ctx, r.servicePort)
	if err := ctx.Err(); err != nil {
		return joinContextError(runErrors, err)
	}
	var tcpBatch rule.Batch
	if tcpErr != nil {
		tcpBatch = incompleteBatch(rule.ScopeNetwork, tcpErr)
		r.recordRuntimeError(ctx, "tcp", rule.ScopeNetwork, tcpErr, &runErrors)
	} else {
		tcpBatch = rule.TCP(tcp, r.servicePort, r.totalTCPConnections, r.servicePortConnections)
	}
	r.reconcile(ctx, "tcp", tcpBatch, &runErrors)
	if err := ctx.Err(); err != nil {
		return joinContextError(runErrors, err)
	}
	serverRunErrorCount := len(runErrors)

	files, authErr := r.api.AuthFiles(ctx)
	if err := ctx.Err(); err != nil {
		return joinContextError(runErrors, err)
	}
	var authBatch rule.Batch
	if authErr != nil {
		authBatch = incompleteBatch(rule.ScopeAuth, authErr)
		r.recordRuntimeError(ctx, "auth", rule.ScopeAuth, authErr, &runErrors)
	} else {
		authBatch = rule.Auth(files)
		if entryErr := authBatch.Err(); entryErr != nil {
			r.recordRuntimeError(ctx, "auth", rule.ScopeAuth, entryErr, &runErrors)
		}
	}
	r.reconcile(ctx, "auth", authBatch, &runErrors)
	if err := ctx.Err(); err != nil {
		return joinContextError(runErrors, err)
	}

	if serverRunErrorCount == 0 && batchesHealthy(healthBatch, memoryBatch, diskBatch, tcpBatch) && r.healthReporter != nil {
		highestDisk := 0.0
		for _, disk := range disks.Disks {
			if disk.UsedPercent > highestDisk {
				highestDisk = disk.UsedPercent
			}
		}
		accountUsages := []AccountUsage(nil)
		accountCount := 0
		if authErr == nil {
			accountUsages = enabledAccountUsages(files)
			accountCount = len(files)
		}
		snapshot := HealthSnapshot{
			MemoryUsedPercent:      memory.UsedPercent,
			MemoryThreshold:        r.memoryPercent,
			HighestDiskUsedPercent: highestDisk,
			DiskMountCount:         len(disks.Disks),
			DiskThreshold:          r.diskPercent,
			TotalTCPConnections:    tcp.TotalConnections,
			TotalTCPThreshold:      r.totalTCPConnections,
			ServicePort:            r.servicePort,
			ServicePortConnections: tcp.ServicePortConnections,
			ServicePortThreshold:   r.servicePortConnections,
			AccountUsageAvailable:  authErr == nil,
			AccountCount:           accountCount,
			EnabledAccountCount:    len(accountUsages),
			AccountUsages:          accountUsages,
		}
		if err := r.healthReporter.ReportHealthy(ctx, snapshot); err != nil {
			runErrors = append(runErrors, fmt.Errorf("monitor healthy report: %w", err))
			r.logger.ErrorContext(ctx, "healthy report failed", "error", err)
		}
	}

	return errors.Join(runErrors...)
}

func enabledAccountUsages(files []cliproxy.AuthFile) []AccountUsage {
	usages := make([]AccountUsage, 0, len(files))
	for i, file := range files {
		if file.Disabled {
			continue
		}
		recentSuccess, recentFailed := int64(0), int64(0)
		for _, bucket := range file.RecentRequests {
			recentSuccess += bucket.Success
			recentFailed += bucket.Failed
		}
		usages = append(usages, AccountUsage{
			Label:         accountLabel(file, i+1),
			Provider:      strings.TrimSpace(file.Provider),
			Success:       file.Success,
			Failed:        file.Failed,
			RecentSuccess: recentSuccess,
			RecentFailed:  recentFailed,
		})
	}
	return usages
}

func accountLabel(file cliproxy.AuthFile, position int) string {
	for _, candidate := range []string{file.Email, file.Account, file.Name, file.AuthIndex} {
		if value := strings.TrimSpace(candidate); value != "" {
			return value
		}
	}
	return fmt.Sprintf("#%d", position)
}

func batchesHealthy(batches ...rule.Batch) bool {
	for _, batch := range batches {
		if !batch.Complete || len(batch.Conditions) != 0 || batch.Err() != nil {
			return false
		}
	}
	return true
}

func incompleteBatch(scope string, err error) rule.Batch {
	batch := rule.Batch{Scope: scope, Complete: false}
	if err != nil {
		batch.Errors = []error{err}
	}
	return batch
}

func (r *Runner) reconcile(ctx context.Context, check string, batch rule.Batch, runErrors *[]error) {
	if err := r.reconciler.Reconcile(ctx, batch); err != nil {
		wrapped := fmt.Errorf("monitor reconcile %s scope: %w", batch.Scope, err)
		*runErrors = append(*runErrors, wrapped)
		if ctx.Err() != nil {
			return
		}
		r.logger.ErrorContext(ctx, "monitor reconciliation failed",
			"check", check,
			"scope", batch.Scope,
			"error", err,
		)
	}
}

func (r *Runner) recordRuntimeError(ctx context.Context, check, scope string, err error, runErrors *[]error) {
	wrapped := fmt.Errorf("monitor %s check: %w", check, err)
	*runErrors = append(*runErrors, wrapped)
	r.logger.ErrorContext(ctx, "monitor check failed",
		"check", check,
		"scope", scope,
		"error", err,
	)
}

func joinContextError(existing []error, contextErr error) error {
	all := make([]error, 0, len(existing)+1)
	all = append(all, existing...)
	all = append(all, contextErr)
	return errors.Join(all...)
}
