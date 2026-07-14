package monitor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/mokexinxin/cpa-monitor/internal/cliproxy"
	"github.com/mokexinxin/cpa-monitor/internal/collector"
	"github.com/mokexinxin/cpa-monitor/internal/rule"
)

func TestRunnerRunsEveryScopeOnceInOrder(t *testing.T) {
	t.Parallel()

	events := &eventLog{}
	api := &fakeAPI{
		events: events,
		files:  []cliproxy.AuthFile{{AuthIndex: "account", Unavailable: true}},
	}
	host := &fakeHostCollector{
		events: events,
		memory: collector.MemoryUsage{
			TotalBytes:  100,
			UsedBytes:   90,
			UsedPercent: 90,
		},
		disks: collector.DiskBatch{Complete: true, Disks: []collector.DiskUsage{{MountPoint: "/", UsedPercent: 90}}},
		tcp:   collector.TCPUsage{TotalConnections: 20, ServicePortConnections: 10},
	}
	reconciler := &recordingReconciler{events: events}
	runner := newTestRunner(t, Options{API: api, Collector: host, Reconciler: reconciler})

	if err := runner.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce() error = %v", err)
	}
	wantEvents := []string{
		"api.health", "reconcile.health",
		"collector.memory", "reconcile.memory",
		"collector.disk", "reconcile.disk",
		"collector.tcp:8317", "reconcile.network",
		"api.auth", "reconcile.auth",
	}
	if got := events.snapshot(); !reflect.DeepEqual(got, wantEvents) {
		t.Fatalf("events = %#v, want %#v", got, wantEvents)
	}
	if got, want := batchKeys(reconciler.snapshot()), [][]string{
		{}, {"resource:memory"}, {"resource:disk:/"},
		{"network:service_port:8317", "network:total_tcp"}, {"auth:account"},
	}; !reflect.DeepEqual(got, want) {
		t.Errorf("batch keys = %#v, want %#v", got, want)
	}
}

func TestRunnerContinuesAfterIndependentFailuresAndJoinsErrors(t *testing.T) {
	t.Parallel()

	healthDown := errors.New("health connection refused")
	memoryErr := errors.New("memory failed")
	diskErr := errors.New("one disk failed")
	tcpErr := errors.New("TCP failed")
	authErr := errors.New("management failed")
	reconcileHealthErr := errors.New("health mail failed")
	reconcileNetworkErr := errors.New("network state failed")
	events := &eventLog{}
	api := &fakeAPI{events: events, healthErr: healthDown, authErr: authErr}
	host := &fakeHostCollector{
		events:    events,
		memoryErr: memoryErr,
		disks: collector.DiskBatch{
			Complete: false,
			Disks:    []collector.DiskUsage{{MountPoint: "/known", UsedPercent: 95}},
			Errors:   []collector.DiskError{{MountPoint: "/missing", Err: diskErr}},
		},
		diskErr: diskErr,
		tcpErr:  tcpErr,
	}
	reconciler := &recordingReconciler{
		events: events,
		errorsByScope: map[string]error{
			rule.ScopeHealth:  reconcileHealthErr,
			rule.ScopeNetwork: reconcileNetworkErr,
		},
	}
	runner := newTestRunner(t, Options{API: api, Collector: host, Reconciler: reconciler})

	err := runner.RunOnce(context.Background())
	for _, want := range []error{memoryErr, diskErr, tcpErr, authErr, reconcileHealthErr, reconcileNetworkErr} {
		if !errors.Is(err, want) {
			t.Errorf("RunOnce() error = %v, want errors.Is(%v)", err, want)
		}
	}
	if errors.Is(err, healthDown) {
		t.Errorf("health down was returned as runner failure: %v", err)
	}

	batches := reconciler.snapshot()
	if got, want := len(batches), 5; got != want {
		t.Fatalf("reconciled batches = %d, want %d", got, want)
	}
	assertRunnerBatch(t, batches[0], rule.ScopeHealth, true, []string{"health:cliproxy_down"})
	assertRunnerBatch(t, batches[1], rule.ScopeMemory, false, nil)
	assertRunnerBatch(t, batches[2], rule.ScopeDisk, false, []string{"resource:disk:/known"})
	assertRunnerBatch(t, batches[3], rule.ScopeNetwork, false, nil)
	assertRunnerBatch(t, batches[4], rule.ScopeAuth, false, nil)
	if got := events.snapshot(); len(got) != 10 {
		t.Errorf("events = %#v, want every operation and reconciliation", got)
	}
}

func TestRunnerHealthFailureIsCompleteDownAndNotRuntimeError(t *testing.T) {
	t.Parallel()

	healthErr := context.DeadlineExceeded // internal HTTP timeout, not outer context
	reconciler := &recordingReconciler{}
	runner := newTestRunner(t, Options{
		API:        &fakeAPI{healthErr: healthErr},
		Collector:  &fakeHostCollector{disks: collector.DiskBatch{Complete: true}},
		Reconciler: reconciler,
	})
	if err := runner.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce() error = %v, health down must not fail cycle", err)
	}
	batches := reconciler.snapshot()
	assertRunnerBatch(t, batches[0], rule.ScopeHealth, true, []string{"health:cliproxy_down"})
	if got := batches[0].Conditions[0].Details["error"]; got != healthErr.Error() {
		t.Errorf("health error detail = %q", got)
	}
}

func TestRunnerReportsOnlyCompleteAllHealthySnapshot(t *testing.T) {
	t.Parallel()
	reporter := &recordingHealthReporter{}
	runner := newTestRunner(t, Options{
		API: &fakeAPI{files: []cliproxy.AuthFile{
			{AuthIndex: "a", Email: "a@example.test", Provider: "codex", IDToken: json.RawMessage(`{"chatgpt_account_id":"account-a"}`), Success: 12, Failed: 3, RecentRequests: []cliproxy.RecentRequest{{Success: 2}, {Success: 1, Failed: 1}}},
			{AuthIndex: "b", Email: "disabled@example.test", Disabled: true, Success: 99},
			{AuthIndex: "c", Account: "team-c", Provider: "claude", Success: 4, RecentRequests: []cliproxy.RecentRequest{{Failed: 2}}},
		}},
		Collector: &fakeHostCollector{
			memory: collector.MemoryUsage{UsedPercent: 42.5},
			disks: collector.DiskBatch{Complete: true, Disks: []collector.DiskUsage{
				{MountPoint: "/", UsedPercent: 51.2}, {MountPoint: "/data", UsedPercent: 20},
			}},
			tcp: collector.TCPUsage{TotalConnections: 4, ServicePortConnections: 2},
		},
		HealthReporter: reporter,
	})
	if err := runner.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(reporter.snapshots) != 1 {
		t.Fatalf("snapshots = %d, want 1", len(reporter.snapshots))
	}
	got := reporter.snapshots[0]
	if got.MemoryUsedPercent != 42.5 || got.HighestDiskUsedPercent != 51.2 || got.DiskMountCount != 2 || got.TotalTCPConnections != 4 || got.ServicePortConnections != 2 || !got.AccountUsageAvailable || got.AccountCount != 3 || got.EnabledAccountCount != 2 {
		t.Fatalf("snapshot = %#v", got)
	}
	wantUsages := []AccountUsage{
		{AuthIndex: "a", AccountID: "account-a", Label: "a@example.test", Provider: "codex", Success: 12, Failed: 3, RecentSuccess: 3, RecentFailed: 1},
		{AuthIndex: "c", Label: "team-c", Provider: "claude", Success: 4, RecentFailed: 2},
	}
	if !reflect.DeepEqual(got.AccountUsages, wantUsages) {
		t.Fatalf("account usages = %#v, want %#v", got.AccountUsages, wantUsages)
	}
}

func TestRunnerAccountProblemsDoNotSuppressServerStatusReport(t *testing.T) {
	t.Parallel()

	t.Run("unhealthy account", func(t *testing.T) {
		reporter := &recordingHealthReporter{}
		runner := newTestRunner(t, Options{
			API: &fakeAPI{files: []cliproxy.AuthFile{{
				AuthIndex: "account-1", Email: "user@example.test", Status: "error", StatusMessage: "quota exhausted",
			}}},
			HealthReporter: reporter,
		})
		if err := runner.RunOnce(context.Background()); err != nil {
			t.Fatal(err)
		}
		if len(reporter.snapshots) != 1 || !reporter.snapshots[0].AccountUsageAvailable || reporter.snapshots[0].EnabledAccountCount != 1 {
			t.Fatalf("snapshots = %#v", reporter.snapshots)
		}
	})

	t.Run("account API failure", func(t *testing.T) {
		reporter := &recordingHealthReporter{}
		authErr := errors.New("management unavailable")
		runner := newTestRunner(t, Options{API: &fakeAPI{authErr: authErr}, HealthReporter: reporter})
		if err := runner.RunOnce(context.Background()); !errors.Is(err, authErr) {
			t.Fatalf("RunOnce() error = %v, want auth error", err)
		}
		if len(reporter.snapshots) != 1 || reporter.snapshots[0].AccountUsageAvailable || reporter.snapshots[0].AccountCount != 0 || len(reporter.snapshots[0].AccountUsages) != 0 {
			t.Fatalf("snapshots = %#v", reporter.snapshots)
		}
	})

	t.Run("account alert delivery failure", func(t *testing.T) {
		reporter := &recordingHealthReporter{}
		reconcileErr := errors.New("account alert failed")
		runner := newTestRunner(t, Options{
			Reconciler:     &recordingReconciler{errorsByScope: map[string]error{rule.ScopeAuth: reconcileErr}},
			HealthReporter: reporter,
		})
		if err := runner.RunOnce(context.Background()); !errors.Is(err, reconcileErr) {
			t.Fatalf("RunOnce() error = %v, want reconcile error", err)
		}
		if len(reporter.snapshots) != 1 {
			t.Fatalf("snapshots = %#v", reporter.snapshots)
		}
	})
}

func TestRunnerSuppressesHealthyReportForConditionOrIncompleteScope(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name string
		api  *fakeAPI
		host *fakeHostCollector
	}{
		{name: "active condition", api: &fakeAPI{healthErr: errors.New("down")}, host: &fakeHostCollector{disks: collector.DiskBatch{Complete: true}}},
		{name: "incomplete scope", api: &fakeAPI{}, host: &fakeHostCollector{memoryErr: errors.New("unknown"), disks: collector.DiskBatch{Complete: true}}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			reporter := &recordingHealthReporter{}
			runner := newTestRunner(t, Options{API: tt.api, Collector: tt.host, HealthReporter: reporter})
			_ = runner.RunOnce(context.Background())
			if len(reporter.snapshots) != 0 {
				t.Fatalf("snapshots = %d, want 0", len(reporter.snapshots))
			}
		})
	}
}

func TestRunnerReturnsHealthyReportDeliveryError(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("healthy SMTP failed")
	reporter := &recordingHealthReporter{err: sentinel}
	runner := newTestRunner(t, Options{HealthReporter: reporter})
	err := runner.RunOnce(context.Background())
	if !errors.Is(err, sentinel) {
		t.Fatalf("RunOnce() error = %v", err)
	}
}

func TestRunnerDiskErrorForcesIncompleteWhilePreservingKnownMounts(t *testing.T) {
	t.Parallel()

	diskErr := errors.New("injected disk failure")
	reconciler := &recordingReconciler{}
	runner := newTestRunner(t, Options{
		API: &fakeAPI{},
		Collector: &fakeHostCollector{
			// The deliberately inconsistent Complete=true exercises Runner's
			// defensive boundary around injected collectors.
			disks:   collector.DiskBatch{Complete: true, Disks: []collector.DiskUsage{{MountPoint: "/known", UsedPercent: 90}}},
			diskErr: diskErr,
		},
		Reconciler: reconciler,
	})
	err := runner.RunOnce(context.Background())
	if !errors.Is(err, diskErr) {
		t.Fatalf("RunOnce() error = %v, want disk error", err)
	}
	diskBatch := reconciler.snapshot()[2]
	assertRunnerBatch(t, diskBatch, rule.ScopeDisk, false, []string{"resource:disk:/known"})
	if !errors.Is(diskBatch.Err(), diskErr) {
		t.Errorf("disk Batch.Err() = %v, want injected error", diskBatch.Err())
	}
}

func TestRunnerOuterCancellationDoesNotCreateHealthDown(t *testing.T) {
	t.Parallel()

	t.Run("before cycle", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		api := &fakeAPI{}
		reconciler := &recordingReconciler{}
		runner := newTestRunner(t, Options{
			API:        api,
			Collector:  &fakeHostCollector{disks: collector.DiskBatch{Complete: true}},
			Reconciler: reconciler,
		})
		if err := runner.RunOnce(ctx); !errors.Is(err, context.Canceled) {
			t.Fatalf("RunOnce() error = %v, want context.Canceled", err)
		}
		if api.healthCalls != 0 || len(reconciler.snapshot()) != 0 {
			t.Fatalf("health calls/batches = %d/%d, want 0/0", api.healthCalls, len(reconciler.snapshot()))
		}
	})

	t.Run("during health", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		api := &fakeAPI{onHealth: func(context.Context) error {
			cancel()
			return context.Canceled
		}}
		reconciler := &recordingReconciler{}
		runner := newTestRunner(t, Options{
			API:        api,
			Collector:  &fakeHostCollector{disks: collector.DiskBatch{Complete: true}},
			Reconciler: reconciler,
		})
		if err := runner.RunOnce(ctx); !errors.Is(err, context.Canceled) {
			t.Fatalf("RunOnce() error = %v, want context.Canceled", err)
		}
		if got := len(reconciler.snapshot()); got != 0 {
			t.Fatalf("reconciled batches = %d, want no health-down batch", got)
		}
	})
}

func TestRunnerStopsImmediatelyWhenLaterCheckCancelsContext(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	events := &eventLog{}
	host := &fakeHostCollector{
		events: events,
		onMemory: func(context.Context) (collector.MemoryUsage, error) {
			cancel()
			return collector.MemoryUsage{}, context.Canceled
		},
		disks: collector.DiskBatch{Complete: true},
	}
	reconciler := &recordingReconciler{events: events}
	runner := newTestRunner(t, Options{API: &fakeAPI{events: events}, Collector: host, Reconciler: reconciler})
	if err := runner.RunOnce(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("RunOnce() error = %v, want context.Canceled", err)
	}
	if got, want := events.snapshot(), []string{"api.health", "reconcile.health", "collector.memory"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("events = %#v, want immediate stop %#v", got, want)
	}
	if batches := reconciler.snapshot(); len(batches) != 1 || batches[0].Scope != rule.ScopeHealth {
		t.Fatalf("batches = %#v, want only health", batches)
	}
}

func TestRunnerPreservesReconcileErrorWhenContextIsCanceled(t *testing.T) {
	t.Parallel()

	sentinel := errors.New("state save failed")
	ctx, cancel := context.WithCancel(context.Background())
	runner := newTestRunner(t, Options{
		Reconciler: reconcilerFunc(func(context.Context, rule.Batch) error {
			cancel()
			return errors.Join(sentinel, context.Canceled)
		}),
	})
	err := runner.RunOnce(ctx)
	if !errors.Is(err, sentinel) || !errors.Is(err, context.Canceled) {
		t.Fatalf("RunOnce() error = %v, want state and context errors", err)
	}
}

func TestRunnerAuthEntryErrorsAreReturnedAfterReconcilingKnownEntries(t *testing.T) {
	t.Parallel()

	reconciler := &recordingReconciler{}
	runner := newTestRunner(t, Options{
		API: &fakeAPI{files: []cliproxy.AuthFile{
			{AuthIndex: "", Unavailable: true},
			{AuthIndex: "known", Unavailable: true},
		}},
		Collector:  &fakeHostCollector{disks: collector.DiskBatch{Complete: true}},
		Reconciler: reconciler,
	})
	err := runner.RunOnce(context.Background())
	if !errors.Is(err, rule.ErrMissingAuthIndex) {
		t.Fatalf("RunOnce() error = %v, want missing auth index", err)
	}
	authBatch := reconciler.snapshot()[4]
	assertRunnerBatch(t, authBatch, rule.ScopeAuth, false, []string{"auth:known"})
}

func TestRunnerLogsCheckAndScopeWithoutConfigurationSecret(t *testing.T) {
	t.Parallel()

	const managementSecret = "super-secret-management-key"
	var output bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&output, nil))
	runner := newTestRunner(t, Options{
		API:        &fakeAPI{managementSecret: managementSecret, authErr: errors.New("request failed")},
		Collector:  &fakeHostCollector{memoryErr: errors.New("read failed"), disks: collector.DiskBatch{Complete: true}},
		Reconciler: &recordingReconciler{},
		Logger:     logger,
	})
	_ = runner.RunOnce(context.Background())
	logs := output.String()
	for _, want := range []string{"monitor check failed", "check=memory", "scope=memory", "check=auth", "scope=auth"} {
		if !strings.Contains(logs, want) {
			t.Errorf("logs = %q, want %q", logs, want)
		}
	}
	if strings.Contains(logs, managementSecret) {
		t.Fatalf("logs leaked management secret: %q", logs)
	}
}

func TestNewRunnerValidatesOptions(t *testing.T) {
	t.Parallel()

	valid := defaultRunnerOptions()
	tests := []struct {
		name   string
		mutate func(*Options)
	}{
		{name: "nil API", mutate: func(o *Options) { o.API = nil }},
		{name: "typed nil API", mutate: func(o *Options) {
			var client *fakeAPI
			o.API = client
		}},
		{name: "nil collector", mutate: func(o *Options) { o.Collector = nil }},
		{name: "typed nil collector", mutate: func(o *Options) {
			var host *fakeHostCollector
			o.Collector = host
		}},
		{name: "nil reconciler", mutate: func(o *Options) { o.Reconciler = nil }},
		{name: "typed nil reconciler", mutate: func(o *Options) {
			var reconciler *recordingReconciler
			o.Reconciler = reconciler
		}},
		{name: "zero service port", mutate: func(o *Options) { o.ServicePort = 0 }},
		{name: "high service port", mutate: func(o *Options) { o.ServicePort = 65536 }},
		{name: "zero memory", mutate: func(o *Options) { o.MemoryPercent = 0 }},
		{name: "high memory", mutate: func(o *Options) { o.MemoryPercent = 101 }},
		{name: "zero disk", mutate: func(o *Options) { o.DiskPercent = 0 }},
		{name: "zero total TCP", mutate: func(o *Options) { o.TotalTCPConnections = 0 }},
		{name: "zero service TCP", mutate: func(o *Options) { o.ServicePortConnections = 0 }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			opts := valid
			tt.mutate(&opts)
			if _, err := NewRunner(opts); err == nil {
				t.Fatal("NewRunner() error = nil, want validation error")
			}
		})
	}

	valid.Logger = nil
	if _, err := NewRunner(valid); err != nil {
		t.Fatalf("NewRunner() with nil logger error = %v", err)
	}
}

func newTestRunner(t *testing.T, overrides Options) *Runner {
	t.Helper()
	opts := defaultRunnerOptions()
	if overrides.API != nil {
		opts.API = overrides.API
	}
	if overrides.Collector != nil {
		opts.Collector = overrides.Collector
	}
	if overrides.Reconciler != nil {
		opts.Reconciler = overrides.Reconciler
	}
	if overrides.Logger != nil {
		opts.Logger = overrides.Logger
	}
	if overrides.HealthReporter != nil {
		opts.HealthReporter = overrides.HealthReporter
	}
	runner, err := NewRunner(opts)
	if err != nil {
		t.Fatalf("NewRunner() error = %v", err)
	}
	return runner
}

func defaultRunnerOptions() Options {
	return Options{
		API:                    &fakeAPI{},
		Collector:              &fakeHostCollector{disks: collector.DiskBatch{Complete: true}},
		Reconciler:             &recordingReconciler{},
		Logger:                 slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)),
		ServicePort:            8317,
		MemoryPercent:          80,
		DiskPercent:            80,
		TotalTCPConnections:    10,
		ServicePortConnections: 5,
	}
}

type eventLog struct {
	mu     sync.Mutex
	events []string
}

func (l *eventLog) add(event string) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.events = append(l.events, event)
}

func (l *eventLog) snapshot() []string {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.events...)
}

type fakeAPI struct {
	mu               sync.Mutex
	events           *eventLog
	healthCalls      int
	authCalls        int
	healthErr        error
	authErr          error
	files            []cliproxy.AuthFile
	onHealth         func(context.Context) error
	managementSecret string
}

func (a *fakeAPI) CheckHealth(ctx context.Context) error {
	a.mu.Lock()
	a.healthCalls++
	onHealth := a.onHealth
	err := a.healthErr
	a.mu.Unlock()
	a.events.add("api.health")
	if onHealth != nil {
		return onHealth(ctx)
	}
	return err
}

func (a *fakeAPI) AuthFiles(context.Context) ([]cliproxy.AuthFile, error) {
	a.mu.Lock()
	a.authCalls++
	files := append([]cliproxy.AuthFile(nil), a.files...)
	err := a.authErr
	a.mu.Unlock()
	a.events.add("api.auth")
	return files, err
}

type fakeHostCollector struct {
	mu        sync.Mutex
	events    *eventLog
	memory    collector.MemoryUsage
	memoryErr error
	disks     collector.DiskBatch
	diskErr   error
	tcp       collector.TCPUsage
	tcpErr    error
	onMemory  func(context.Context) (collector.MemoryUsage, error)
}

func (c *fakeHostCollector) Memory(ctx context.Context) (collector.MemoryUsage, error) {
	c.events.add("collector.memory")
	if c.onMemory != nil {
		return c.onMemory(ctx)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.memory, c.memoryErr
}

func (c *fakeHostCollector) Disks(context.Context) (collector.DiskBatch, error) {
	c.events.add("collector.disk")
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.disks, c.diskErr
}

func (c *fakeHostCollector) TCP(_ context.Context, port int) (collector.TCPUsage, error) {
	c.events.add("collector.tcp:" + fmtInt(port))
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.tcp, c.tcpErr
}

type recordingReconciler struct {
	mu            sync.Mutex
	events        *eventLog
	batches       []rule.Batch
	errorsByScope map[string]error
}

type reconcilerFunc func(context.Context, rule.Batch) error

func (f reconcilerFunc) Reconcile(ctx context.Context, batch rule.Batch) error {
	return f(ctx, batch)
}

type recordingHealthReporter struct {
	snapshots []HealthSnapshot
	err       error
}

func (r *recordingHealthReporter) ReportHealthy(_ context.Context, snapshot HealthSnapshot) error {
	r.snapshots = append(r.snapshots, snapshot)
	return r.err
}

func (r *recordingReconciler) Reconcile(_ context.Context, batch rule.Batch) error {
	r.events.add("reconcile." + batch.Scope)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.batches = append(r.batches, batch)
	return r.errorsByScope[batch.Scope]
}

func (r *recordingReconciler) snapshot() []rule.Batch {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]rule.Batch(nil), r.batches...)
}

func batchKeys(batches []rule.Batch) [][]string {
	result := make([][]string, len(batches))
	for i, batch := range batches {
		result[i] = make([]string, len(batch.Conditions))
		for j, condition := range batch.Conditions {
			result[i][j] = condition.Key
		}
	}
	return result
}

func assertRunnerBatch(t *testing.T, batch rule.Batch, scope string, complete bool, keys []string) {
	t.Helper()
	if batch.Scope != scope || batch.Complete != complete {
		t.Fatalf("batch scope/complete = %q/%t, want %q/%t", batch.Scope, batch.Complete, scope, complete)
	}
	got := batchKeys([]rule.Batch{batch})[0]
	if len(got) == 0 && keys == nil {
		return
	}
	if !reflect.DeepEqual(got, keys) {
		t.Fatalf("batch keys = %#v, want %#v", got, keys)
	}
}

func fmtInt(value int) string {
	// Avoid fmt's comparatively heavy formatting in every fake collector call.
	return strconv.Itoa(value)
}
