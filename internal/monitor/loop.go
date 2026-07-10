package monitor

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"
)

// CycleRunner executes one complete, non-overlapping monitor cycle.
type CycleRunner interface {
	RunOnce(context.Context) error
}

// RunSingle executes exactly one cycle and returns its error to the caller.
// It does not create a ticker.
func RunSingle(ctx context.Context, runner CycleRunner) error {
	if ctx == nil {
		return errors.New("monitor: nil context")
	}
	if runner == nil {
		return errors.New("monitor: nil cycle runner")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return runner.RunOnce(ctx)
}

// RunDaemon executes one cycle immediately, then one cycle for each interval
// tick. Calls are deliberately synchronous: a slow cycle can coalesce ticker
// events, but cycles can never overlap. Per-cycle errors are logged and do not
// terminate the daemon. Context cancellation is treated as a graceful stop.
func RunDaemon(ctx context.Context, runner CycleRunner, interval time.Duration, logger *slog.Logger) error {
	return runDaemon(ctx, runner, interval, logger, newSystemTicker)
}

type daemonTicker interface {
	Ticks() <-chan time.Time
	Stop()
}

type tickerFactory func(time.Duration) daemonTicker

type systemTicker struct {
	ticker *time.Ticker
}

func newSystemTicker(interval time.Duration) daemonTicker {
	return &systemTicker{ticker: time.NewTicker(interval)}
}

func (t *systemTicker) Ticks() <-chan time.Time { return t.ticker.C }

func (t *systemTicker) Stop() { t.ticker.Stop() }

func runDaemon(
	ctx context.Context,
	runner CycleRunner,
	interval time.Duration,
	logger *slog.Logger,
	newTicker tickerFactory,
) error {
	if ctx == nil {
		return errors.New("monitor: nil context")
	}
	if runner == nil {
		return errors.New("monitor: nil cycle runner")
	}
	if interval <= 0 {
		return fmt.Errorf("monitor: interval must be greater than zero, got %s", interval)
	}
	if newTicker == nil {
		return errors.New("monitor: nil ticker factory")
	}
	if ctx.Err() != nil {
		return nil
	}
	if logger == nil {
		logger = slog.Default()
	}

	// Construct the ticker before the immediate run so the interval is measured
	// from the cycle's start, rather than from its completion.
	ticker := newTicker(interval)
	if ticker == nil {
		return errors.New("monitor: ticker factory returned nil")
	}
	defer ticker.Stop()

	cycle := uint64(1)
	runCycle := func() {
		err := runner.RunOnce(ctx)
		if err != nil && !isContextStop(ctx, err) {
			logger.ErrorContext(ctx, "monitor cycle failed",
				"cycle", cycle,
				"error", err,
			)
		}
		cycle++
	}

	runCycle()
	for {
		if ctx.Err() != nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return nil
		case _, ok := <-ticker.Ticks():
			if !ok {
				return errors.New("monitor: ticker stopped unexpectedly")
			}
			// Cancellation and a tick can become ready together. Check again
			// before starting work so shutdown does not add another cycle.
			if ctx.Err() != nil {
				return nil
			}
			runCycle()
		}
	}
}

func isContextStop(ctx context.Context, err error) bool {
	contextErr := ctx.Err()
	return contextErr != nil && errors.Is(err, contextErr)
}
