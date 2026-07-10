package monitor

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestLoopRunsImmediatelyThenOnEachTickAndStopsTicker(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	ticker := newFakeTicker()
	runner := &scriptedRunner{calls: make(chan int, 4)}
	factoryCalled := make(chan time.Duration, 1)
	done := make(chan error, 1)
	go func() {
		done <- runDaemon(ctx, runner, 42*time.Second, discardLogger(), func(interval time.Duration) daemonTicker {
			factoryCalled <- interval
			return ticker
		})
	}()

	if got := receive(t, factoryCalled); got != 42*time.Second {
		t.Fatalf("ticker interval = %s, want 42s", got)
	}
	if got := receive(t, runner.calls); got != 1 {
		t.Fatalf("immediate call = %d, want 1", got)
	}
	ticker.tick()
	if got := receive(t, runner.calls); got != 2 {
		t.Fatalf("first tick call = %d, want 2", got)
	}
	ticker.tick()
	if got := receive(t, runner.calls); got != 3 {
		t.Fatalf("second tick call = %d, want 3", got)
	}

	cancel()
	if err := receive(t, done); err != nil {
		t.Fatalf("runDaemon() error = %v", err)
	}
	if !ticker.stopped.Load() {
		t.Fatal("ticker was not stopped")
	}
	select {
	case call := <-runner.calls:
		t.Fatalf("unexpected call %d after cancellation", call)
	default:
	}
}

func TestLoopCreatesTickerBeforeImmediateCycle(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	events := make(chan string, 2)
	ticker := newFakeTicker()
	runner := runnerFunc(func(context.Context) error {
		events <- "cycle"
		return nil
	})
	done := make(chan error, 1)
	go func() {
		done <- runDaemon(ctx, runner, time.Minute, discardLogger(), func(time.Duration) daemonTicker {
			events <- "ticker"
			return ticker
		})
	}()
	if first := receive(t, events); first != "ticker" {
		t.Fatalf("first event = %q, want ticker", first)
	}
	if second := receive(t, events); second != "cycle" {
		t.Fatalf("second event = %q, want cycle", second)
	}
	cancel()
	if err := receive(t, done); err != nil {
		t.Fatalf("runDaemon() error = %v", err)
	}
}

func TestLoopLogsCycleErrorsAndContinues(t *testing.T) {
	t.Parallel()

	firstError := errors.New("first cycle failed")
	secondError := errors.New("second cycle failed")
	runner := &scriptedRunner{
		calls:   make(chan int, 3),
		results: []error{firstError, secondError, nil},
	}
	ticker := newFakeTicker()
	var logOutput bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logOutput, &slog.HandlerOptions{Level: slog.LevelDebug}))
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	done := make(chan error, 1)
	go func() {
		done <- runDaemon(ctx, runner, time.Minute, logger, func(time.Duration) daemonTicker { return ticker })
	}()

	receive(t, runner.calls)
	ticker.tick()
	receive(t, runner.calls)
	ticker.tick()
	receive(t, runner.calls)
	cancel()
	if err := receive(t, done); err != nil {
		t.Fatalf("runDaemon() error = %v", err)
	}

	logs := logOutput.String()
	for _, want := range []string{"monitor cycle failed", "cycle=1", "first cycle failed", "cycle=2", "second cycle failed"} {
		if !strings.Contains(logs, want) {
			t.Errorf("logs = %q, want %q", logs, want)
		}
	}
}

func TestLoopNeverRunsCyclesConcurrently(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	ticker := newFakeTicker()
	runner := newBlockingRunner()
	done := make(chan error, 1)
	go func() {
		done <- runDaemon(ctx, runner, time.Minute, discardLogger(), func(time.Duration) daemonTicker { return ticker })
	}()

	if got := receive(t, runner.started); got != 1 {
		t.Fatalf("first cycle = %d, want 1", got)
	}
	// A tick becomes pending while the immediate cycle is still blocked.
	ticker.tick()
	select {
	case call := <-runner.started:
		t.Fatalf("cycle %d started concurrently", call)
	default:
	}
	runner.release <- struct{}{}
	if got := receive(t, runner.started); got != 2 {
		t.Fatalf("second cycle = %d, want 2", got)
	}
	if got := runner.maxActive.Load(); got != 1 {
		t.Fatalf("maximum concurrent cycles = %d, want 1", got)
	}

	cancel() // the context-aware second cycle exits without an explicit release
	if err := receive(t, done); err != nil {
		t.Fatalf("runDaemon() error = %v", err)
	}
	if got := runner.maxActive.Load(); got != 1 {
		t.Fatalf("maximum concurrent cycles after stop = %d, want 1", got)
	}
}

func TestLoopCancellationBeforeStartDoesNotCreateTickerOrRun(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var tickerCalls atomic.Int32
	var runnerCalls atomic.Int32
	err := runDaemon(ctx, runnerFunc(func(context.Context) error {
		runnerCalls.Add(1)
		return nil
	}), time.Minute, discardLogger(), func(time.Duration) daemonTicker {
		tickerCalls.Add(1)
		return newFakeTicker()
	})
	if err != nil {
		t.Fatalf("runDaemon() error = %v", err)
	}
	if got := tickerCalls.Load(); got != 0 {
		t.Errorf("ticker factory calls = %d, want 0", got)
	}
	if got := runnerCalls.Load(); got != 0 {
		t.Errorf("runner calls = %d, want 0", got)
	}
}

func TestLoopClosedTickerReturnsErrorAndStops(t *testing.T) {
	t.Parallel()

	ticker := newFakeTicker()
	runner := &scriptedRunner{calls: make(chan int, 1)}
	done := make(chan error, 1)
	go func() {
		done <- runDaemon(context.Background(), runner, time.Minute, discardLogger(), func(time.Duration) daemonTicker { return ticker })
	}()
	receive(t, runner.calls)
	close(ticker.ticks)
	err := receive(t, done)
	if err == nil || !strings.Contains(err.Error(), "ticker stopped") {
		t.Fatalf("runDaemon() error = %v, want ticker stopped error", err)
	}
	if !ticker.stopped.Load() {
		t.Fatal("ticker was not stopped after its channel closed")
	}
}

func TestLoopValidatesInputsWithoutCreatingTicker(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		ctx      context.Context
		runner   CycleRunner
		interval time.Duration
		factory  tickerFactory
	}{
		{name: "nil context", runner: runnerFunc(func(context.Context) error { return nil }), interval: time.Second, factory: func(time.Duration) daemonTicker { return newFakeTicker() }},
		{name: "nil runner", ctx: context.Background(), interval: time.Second, factory: func(time.Duration) daemonTicker { return newFakeTicker() }},
		{name: "zero interval", ctx: context.Background(), runner: runnerFunc(func(context.Context) error { return nil }), factory: func(time.Duration) daemonTicker { return newFakeTicker() }},
		{name: "nil factory", ctx: context.Background(), runner: runnerFunc(func(context.Context) error { return nil }), interval: time.Second},
		{name: "nil ticker", ctx: context.Background(), runner: runnerFunc(func(context.Context) error { return nil }), interval: time.Second, factory: func(time.Duration) daemonTicker { return nil }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if err := runDaemon(tt.ctx, tt.runner, tt.interval, discardLogger(), tt.factory); err == nil {
				t.Fatal("runDaemon() error = nil, want validation error")
			}
		})
	}
}

func TestOnceRunsExactlyOnceWithoutTicker(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("cycle failed")
	var calls atomic.Int32
	runner := runnerFunc(func(context.Context) error {
		calls.Add(1)
		return sentinel
	})
	if err := RunSingle(context.Background(), runner); !errors.Is(err, sentinel) {
		t.Fatalf("RunSingle() error = %v, want sentinel", err)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("RunSingle() calls = %d, want 1", got)
	}
}

func TestOnceHonorsCanceledContext(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var calls atomic.Int32
	err := RunSingle(ctx, runnerFunc(func(context.Context) error {
		calls.Add(1)
		return nil
	}))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("RunSingle() error = %v, want context.Canceled", err)
	}
	if got := calls.Load(); got != 0 {
		t.Fatalf("RunSingle() calls = %d, want 0", got)
	}
}

type fakeTicker struct {
	ticks   chan time.Time
	stopped atomic.Bool
}

func newFakeTicker() *fakeTicker {
	// time.Ticker exposes a one-element channel, which coalesces ticks while a
	// synchronous cycle is running.
	return &fakeTicker{ticks: make(chan time.Time, 1)}
}

func (t *fakeTicker) Ticks() <-chan time.Time { return t.ticks }

func (t *fakeTicker) Stop() { t.stopped.Store(true) }

func (t *fakeTicker) tick() { t.ticks <- time.Unix(1, 0) }

type scriptedRunner struct {
	mu      sync.Mutex
	count   int
	results []error
	calls   chan int
}

func (r *scriptedRunner) RunOnce(context.Context) error {
	r.mu.Lock()
	r.count++
	call := r.count
	var err error
	if call <= len(r.results) {
		err = r.results[call-1]
	}
	r.mu.Unlock()
	r.calls <- call
	return err
}

type runnerFunc func(context.Context) error

func (f runnerFunc) RunOnce(ctx context.Context) error { return f(ctx) }

type blockingRunner struct {
	active    atomic.Int32
	maxActive atomic.Int32
	count     atomic.Int32
	started   chan int
	release   chan struct{}
}

func newBlockingRunner() *blockingRunner {
	return &blockingRunner{
		started: make(chan int, 4),
		release: make(chan struct{}, 4),
	}
}

func (r *blockingRunner) RunOnce(ctx context.Context) error {
	active := r.active.Add(1)
	defer r.active.Add(-1)
	for {
		maximum := r.maxActive.Load()
		if active <= maximum || r.maxActive.CompareAndSwap(maximum, active) {
			break
		}
	}
	call := int(r.count.Add(1))
	r.started <- call
	select {
	case <-r.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func receive[T any](t *testing.T, channel <-chan T) T {
	t.Helper()
	select {
	case value := <-channel:
		return value
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for test event")
		var zero T
		return zero
	}
}
