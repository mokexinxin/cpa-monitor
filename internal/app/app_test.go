package app

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mokexinxin/cpa-monitor/internal/config"
)

func TestRunOnceLoadsSelectedConfigAndRunsExactlyOnce(t *testing.T) {
	t.Parallel()

	var loadedPath string
	var calls atomic.Int32
	var closed atomic.Int32
	deps := Dependencies{
		LoadConfig: func(path string) (config.Config, error) {
			loadedPath = path
			return config.Default(), nil
		},
		Build: func(config.Config, io.Writer) (*Runtime, error) {
			return &Runtime{
				Runner:   runnerFunc(func(context.Context) error { calls.Add(1); return nil }),
				Logger:   discardLogger(),
				Interval: time.Minute,
				Close:    func() error { closed.Add(1); return nil },
			}, nil
		},
	}
	var stdout, stderr bytes.Buffer
	code := Run(context.Background(), []string{"--config", "custom.yaml", "--once"}, &stdout, &stderr, deps)
	if code != 0 || loadedPath != "custom.yaml" || calls.Load() != 1 || closed.Load() != 1 {
		t.Fatalf("Run() code=%d path=%q calls=%d closed=%d stderr=%q", code, loadedPath, calls.Load(), closed.Load(), stderr.String())
	}
}

func TestRunUsesDefaultConfigPath(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var loadedPath string
	deps := Dependencies{
		LoadConfig: func(path string) (config.Config, error) { loadedPath = path; return config.Default(), nil },
		Build: func(config.Config, io.Writer) (*Runtime, error) {
			return &Runtime{Runner: runnerFunc(func(context.Context) error { return nil }), Logger: discardLogger(), Interval: time.Minute}, nil
		},
	}
	if code := Run(ctx, nil, io.Discard, io.Discard, deps); code != 0 {
		t.Fatalf("Run() code = %d", code)
	}
	if loadedPath != "config.yaml" {
		t.Fatalf("config path = %q", loadedPath)
	}
}

func TestRunCheckConfigValidatesWithoutBuildingRuntime(t *testing.T) {
	t.Parallel()

	var loadedPath string
	buildCalled := false
	deps := Dependencies{
		LoadConfig: func(path string) (config.Config, error) {
			loadedPath = path
			return validConfigForCheck(), nil
		},
		Build: func(config.Config, io.Writer) (*Runtime, error) {
			buildCalled = true
			return nil, errors.New("runtime must not be built")
		},
	}
	var stdout, stderr bytes.Buffer
	code := Run(context.Background(), []string{"--config", "checked.yaml", "--check-config"}, &stdout, &stderr, deps)
	if code != 0 {
		t.Fatalf("Run() code = %d, stderr=%q", code, stderr.String())
	}
	if loadedPath != "checked.yaml" {
		t.Fatalf("loaded path = %q", loadedPath)
	}
	if buildCalled {
		t.Fatal("runtime builder was called")
	}
	if got, want := stdout.String(), "cpa-monitor: configuration is valid\n"; got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestRunCheckConfigRejectsInvalidLoadedConfigWithoutBuildingRuntime(t *testing.T) {
	t.Parallel()

	buildCalled := false
	deps := Dependencies{
		LoadConfig: func(string) (config.Config, error) {
			cfg := validConfigForCheck()
			cfg.Interval.Duration = 0
			return cfg, nil
		},
		Build: func(config.Config, io.Writer) (*Runtime, error) {
			buildCalled = true
			return nil, nil
		},
	}
	var stdout, stderr bytes.Buffer
	if code := Run(context.Background(), []string{"--check-config"}, &stdout, &stderr, deps); code != 1 {
		t.Fatalf("Run() code = %d, stderr=%q", code, stderr.String())
	}
	if buildCalled {
		t.Fatal("runtime builder was called")
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout = %q", stdout.String())
	}
	if !strings.Contains(stderr.String(), "validate configuration") || !strings.Contains(stderr.String(), "interval") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestRunOnceReturnsNonzeroAfterRuntimeAndInitialErrors(t *testing.T) {
	t.Parallel()

	cycleErr := errors.New("collector failed")
	stateErr := errors.New("state corrupt")
	var calls atomic.Int32
	deps := Dependencies{
		LoadConfig: func(string) (config.Config, error) { return config.Default(), nil },
		Build: func(config.Config, io.Writer) (*Runtime, error) {
			return &Runtime{
				Runner: runnerFunc(func(context.Context) error { calls.Add(1); return cycleErr }),
				Logger: discardLogger(), Interval: time.Minute, InitialError: stateErr,
			}, nil
		},
	}
	var stderr bytes.Buffer
	if code := Run(context.Background(), []string{"--once"}, io.Discard, &stderr, deps); code != 1 {
		t.Fatalf("Run() code = %d, stderr=%q", code, stderr.String())
	}
	if calls.Load() != 1 {
		t.Fatalf("runner calls = %d", calls.Load())
	}
}

func TestRunLogsFailureBeforeClosingRuntime(t *testing.T) {
	t.Parallel()

	var logs bytes.Buffer
	closed := false
	deps := Dependencies{
		LoadConfig: func(string) (config.Config, error) { return config.Default(), nil },
		Build: func(config.Config, io.Writer) (*Runtime, error) {
			return &Runtime{
				Runner:   runnerFunc(func(context.Context) error { return errors.New("cycle exploded") }),
				Logger:   slog.New(slog.NewTextHandler(&logs, nil)),
				Interval: time.Minute,
				Close:    func() error { closed = true; return nil },
			}, nil
		},
	}
	if code := Run(context.Background(), []string{"--once"}, io.Discard, io.Discard, deps); code != 1 {
		t.Fatalf("Run() code = %d", code)
	}
	if !closed || !strings.Contains(logs.String(), "one-shot monitor cycle failed") || !strings.Contains(logs.String(), "cycle exploded") {
		t.Fatalf("closed=%v logs=%q", closed, logs.String())
	}
}

func TestRunDaemonLogsInitialErrorAndContinues(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var logs bytes.Buffer
	deps := Dependencies{
		LoadConfig: func(string) (config.Config, error) { return config.Default(), nil },
		Build: func(config.Config, io.Writer) (*Runtime, error) {
			return &Runtime{
				Runner: runnerFunc(func(context.Context) error { return nil }),
				Logger: slog.New(slog.NewTextHandler(&logs, nil)), Interval: time.Minute,
				InitialError: errors.New("state could not be loaded"),
			}, nil
		},
	}
	if code := Run(ctx, nil, io.Discard, io.Discard, deps); code != 0 {
		t.Fatalf("Run() code = %d", code)
	}
	if !strings.Contains(logs.String(), "state could not be loaded") {
		t.Fatalf("logs = %q", logs.String())
	}
}

func TestRunMapsArgumentConfigurationBuildAndCloseErrors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		args []string
		deps Dependencies
		want int
	}{
		{
			name: "unknown flag", args: []string{"--unknown"}, want: 2,
			deps: Dependencies{LoadConfig: func(string) (config.Config, error) { t.Fatal("loader called"); return config.Config{}, nil }},
		},
		{
			name: "positional argument", args: []string{"unexpected"}, want: 2,
			deps: Dependencies{LoadConfig: func(string) (config.Config, error) { t.Fatal("loader called"); return config.Config{}, nil }},
		},
		{
			name: "config", args: []string{"--once"}, want: 1,
			deps: Dependencies{LoadConfig: func(string) (config.Config, error) { return config.Config{}, errors.New("bad config") }},
		},
		{
			name: "build", args: []string{"--once"}, want: 1,
			deps: Dependencies{
				LoadConfig: func(string) (config.Config, error) { return config.Default(), nil },
				Build:      func(config.Config, io.Writer) (*Runtime, error) { return nil, errors.New("build failed") },
			},
		},
		{
			name: "close", args: []string{"--once"}, want: 1,
			deps: Dependencies{
				LoadConfig: func(string) (config.Config, error) { return config.Default(), nil },
				Build: func(config.Config, io.Writer) (*Runtime, error) {
					return &Runtime{Runner: runnerFunc(func(context.Context) error { return nil }), Logger: discardLogger(), Interval: time.Minute, Close: func() error { return errors.New("close failed") }}, nil
				},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var stderr bytes.Buffer
			if code := Run(context.Background(), tt.args, io.Discard, &stderr, tt.deps); code != tt.want {
				t.Fatalf("Run() code=%d, want %d; stderr=%q", code, tt.want, stderr.String())
			}
		})
	}
}

func TestRunReportsPartialBuildCleanupError(t *testing.T) {
	t.Parallel()

	buildErr := errors.New("build failed")
	closeErr := errors.New("cleanup failed")
	deps := Dependencies{
		LoadConfig: func(string) (config.Config, error) { return config.Default(), nil },
		Build: func(config.Config, io.Writer) (*Runtime, error) {
			return &Runtime{Close: func() error { return closeErr }}, buildErr
		},
	}
	var stderr bytes.Buffer
	if code := Run(context.Background(), []string{"--once"}, io.Discard, &stderr, deps); code != 1 {
		t.Fatalf("Run() code = %d", code)
	}
	if !strings.Contains(stderr.String(), buildErr.Error()) || !strings.Contains(stderr.String(), closeErr.Error()) {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestRunHelpDoesNotLoadConfiguration(t *testing.T) {
	t.Parallel()
	called := false
	deps := Dependencies{LoadConfig: func(string) (config.Config, error) { called = true; return config.Config{}, nil }}
	var stderr bytes.Buffer
	if code := Run(context.Background(), []string{"--help"}, io.Discard, &stderr, deps); code != 0 {
		t.Fatalf("Run() code = %d", code)
	}
	if called || !strings.Contains(stderr.String(), "Usage") {
		t.Fatalf("called=%v stderr=%q", called, stderr.String())
	}
}

func TestRunDoesNotEchoUnexpectedPositionalArguments(t *testing.T) {
	t.Parallel()

	const secret = "token-that-must-not-be-logged"
	var stderr bytes.Buffer
	if code := Run(context.Background(), []string{secret}, io.Discard, &stderr, Dependencies{}); code != 2 {
		t.Fatalf("Run() code = %d", code)
	}
	if strings.Contains(stderr.String(), secret) || !strings.Contains(stderr.String(), "1 unexpected") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

type runnerFunc func(context.Context) error

func (f runnerFunc) RunOnce(ctx context.Context) error { return f(ctx) }

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func validConfigForCheck() config.Config {
	cfg := config.Default()
	cfg.CLIProxy.ManagementKey = "management-key"
	cfg.SMTP.Host = "smtp.example.com"
	cfg.SMTP.From = "monitor@example.com"
	cfg.SMTP.To = []string{"ops@example.com"}
	return cfg
}
