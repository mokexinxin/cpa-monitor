package app

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"time"

	"github.com/mokexinxin/cpa-monitor/internal/config"
	"github.com/mokexinxin/cpa-monitor/internal/monitor"
	"github.com/mokexinxin/cpa-monitor/internal/notification"
)

type Runtime struct {
	Runner       monitor.CycleRunner
	Logger       *slog.Logger
	Interval     time.Duration
	InitialError error
	Close        func() error
}

type Dependencies struct {
	LoadConfig       func(string) (config.Config, error)
	Build            func(config.Config, io.Writer) (*Runtime, error)
	TestNotification func(context.Context, config.Config, string) error
}

// Run parses CLI arguments and maps startup/runtime outcomes to a process exit
// code. The injected boundaries keep argument and one-shot behavior testable.
func Run(ctx context.Context, args []string, stdout, stderr io.Writer, deps Dependencies) int {
	if stdout == nil {
		stdout = io.Discard
	}
	if stderr == nil {
		stderr = io.Discard
	}
	if ctx == nil {
		fmt.Fprintln(stderr, "cpa-monitor: context is nil")
		return 2
	}

	flags := flag.NewFlagSet("cpa-monitor", flag.ContinueOnError)
	flags.SetOutput(stderr)
	configPath := flags.String("config", "config.yaml", "path to YAML configuration")
	once := flags.Bool("once", false, "run one full check cycle and exit")
	checkConfig := flags.Bool("check-config", false, "validate configuration and exit")
	testNotification := flags.String("test-notification", "", "send a test through primary, dingtalk, or smtp and exit")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if flags.NArg() != 0 {
		fmt.Fprintf(stderr, "cpa-monitor: %d unexpected positional argument(s)\n", flags.NArg())
		return 2
	}
	if *testNotification != "" {
		if *testNotification != "primary" && *testNotification != "dingtalk" && *testNotification != "smtp" {
			fmt.Fprintln(stderr, "cpa-monitor: --test-notification must be primary, dingtalk, or smtp")
			return 2
		}
		if *once || *checkConfig {
			fmt.Fprintln(stderr, "cpa-monitor: --test-notification cannot be combined with --once or --check-config")
			return 2
		}
	}
	if deps.LoadConfig == nil {
		fmt.Fprintln(stderr, "cpa-monitor: configuration loader is nil")
		return 1
	}

	cfg, err := deps.LoadConfig(*configPath)
	if err != nil {
		fmt.Fprintf(stderr, "cpa-monitor: load configuration: %v\n", err)
		return 1
	}
	if *checkConfig {
		if err := cfg.Validate(); err != nil {
			fmt.Fprintf(stderr, "cpa-monitor: validate configuration: %v\n", err)
			return 1
		}
		fmt.Fprintln(stdout, "cpa-monitor: configuration is valid")
		return 0
	}
	if *testNotification != "" {
		if deps.TestNotification == nil {
			fmt.Fprintln(stderr, "cpa-monitor: notification tester is nil")
			return 1
		}
		if err := deps.TestNotification(ctx, cfg, *testNotification); err != nil {
			fmt.Fprintf(stderr, "cpa-monitor: test notification failed: %v\n", err)
			return 1
		}
		fmt.Fprintf(stdout, "cpa-monitor: test notification sent successfully through %s\n", *testNotification)
		return 0
	}
	if deps.Build == nil {
		fmt.Fprintln(stderr, "cpa-monitor: runtime builder is nil")
		return 1
	}
	runtime, err := deps.Build(cfg, stdout)
	if err != nil {
		closeErr := closeRuntime(runtime)
		combined := errors.Join(err, closeErr)
		fmt.Fprintf(stderr, "cpa-monitor: initialize runtime: %v\n", combined)
		return 1
	}
	if runtime == nil || runtime.Runner == nil {
		combined := errors.Join(errors.New("runtime builder returned no runner"), closeRuntime(runtime))
		fmt.Fprintf(stderr, "cpa-monitor: initialize runtime: %v\n", combined)
		return 1
	}
	logger := runtime.Logger
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(stdout, nil))
	}
	if runtime.InitialError != nil {
		logger.ErrorContext(ctx, "runtime initialized with degraded alert state", "error", runtime.InitialError)
	}

	if *once {
		runErr := monitor.RunSingle(ctx, runtime.Runner)
		beforeClose := errors.Join(runtime.InitialError, runErr)
		if beforeClose != nil {
			logger.ErrorContext(ctx, "one-shot monitor cycle failed", "error", beforeClose)
		}
		closeErr := closeRuntime(runtime)
		combined := errors.Join(beforeClose, closeErr)
		if combined != nil {
			fmt.Fprintf(stderr, "cpa-monitor: one-shot failed: %v\n", combined)
			return 1
		}
		return 0
	}

	runErr := monitor.RunDaemon(ctx, runtime.Runner, runtime.Interval, logger)
	if runErr != nil {
		logger.ErrorContext(ctx, "monitor daemon stopped with an error", "error", runErr)
	}
	closeErr := closeRuntime(runtime)
	if combined := errors.Join(runErr, closeErr); combined != nil {
		fmt.Fprintf(stderr, "cpa-monitor: daemon failed: %v\n", combined)
		return 1
	}
	return 0
}

func testNotification(ctx context.Context, cfg config.Config, target string) error {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	var sender notification.AlertSender
	var err error
	if target == "primary" {
		transports, buildErr := buildTransports(cfg, logger)
		if buildErr != nil {
			return buildErr
		}
		sender, err = buildAlertRouter(cfg, transports, logger)
	} else {
		if !cfg.UsesChannel(target) {
			return fmt.Errorf("channel %q is not configured", target)
		}
		sender, _, err = buildChannel(cfg, target, logger)
	}
	if err != nil {
		return err
	}
	if sender == nil {
		return fmt.Errorf("channel %q is not available", target)
	}
	hostname, err := os.Hostname()
	if err != nil {
		return fmt.Errorf("read host name: %w", err)
	}
	now := time.Now().UTC()
	event := notification.Event{
		Kind: notification.Alert, Scope: "test", Object: "CPA Monitor notification test",
		Hostname: hostname, Timestamp: now, Key: "test:notification", Current: "test",
		Threshold: "not applicable", Details: "This is an explicit CPA Monitor test notification.",
		BaseURL: cfg.CLIProxy.BaseURL,
	}
	return sender.SendBatch(ctx, notification.Batch{
		Kind: event.Kind, Scope: event.Scope, Hostname: event.Hostname, Timestamp: event.Timestamp,
		Events: []notification.Event{event},
	})
}

func closeRuntime(runtime *Runtime) error {
	if runtime == nil || runtime.Close == nil {
		return nil
	}
	return runtime.Close()
}
