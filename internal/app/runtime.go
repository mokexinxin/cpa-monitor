package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/mokexinxin/cpa-monitor/internal/alerter"
	"github.com/mokexinxin/cpa-monitor/internal/cliproxy"
	"github.com/mokexinxin/cpa-monitor/internal/collector"
	"github.com/mokexinxin/cpa-monitor/internal/config"
	"github.com/mokexinxin/cpa-monitor/internal/healthreport"
	"github.com/mokexinxin/cpa-monitor/internal/logfile"
	"github.com/mokexinxin/cpa-monitor/internal/mailer"
	"github.com/mokexinxin/cpa-monitor/internal/monitor"
	"github.com/mokexinxin/cpa-monitor/internal/state"
)

func Main(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	return Run(ctx, args, stdout, stderr, DefaultDependencies())
}

func DefaultDependencies() Dependencies {
	return Dependencies{
		LoadConfig: config.Load,
		Build:      buildRuntime,
	}
}

func buildRuntime(cfg config.Config, console io.Writer) (*Runtime, error) {
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("validate runtime configuration: %w", err)
	}

	var fileWriter *logfile.Writer
	if cfg.Logging.File.Enabled {
		var err error
		fileWriter, err = logfile.NewWriter(logfile.Options{
			Path:              cfg.Logging.File.Path,
			MaxSizeBytes:      cfg.Logging.File.MaxSizeMB * 1024 * 1024,
			MaxFiles:          cfg.Logging.File.MaxFiles,
			MaxTotalSizeBytes: cfg.Logging.File.MaxTotalSizeMB * 1024 * 1024,
		})
		if err != nil {
			return nil, fmt.Errorf("initialize file logging: %w", err)
		}
	}
	closeOnError := func(err error) (*Runtime, error) {
		if fileWriter != nil {
			if closeErr := fileWriter.Close(); closeErr != nil {
				err = errors.Join(err, fmt.Errorf("close file logger after initialization failure: %w", closeErr))
			}
		}
		return nil, err
	}

	logger, err := logfile.NewLogger(cfg.Logging.Level, console, fileWriter)
	if err != nil {
		return closeOnError(fmt.Errorf("initialize logger: %w", err))
	}

	store, stateErr := state.Open(cfg.Alerts.StateFile)
	sender, err := mailer.New(mailer.Config{
		Host:      cfg.SMTP.Host,
		Port:      cfg.SMTP.Port,
		Username:  cfg.SMTP.Username,
		Password:  cfg.SMTP.Password,
		From:      cfg.SMTP.From,
		To:        cfg.SMTP.To,
		StartTLS:  cfg.SMTP.StartTLS,
		DirectTLS: cfg.SMTP.TLS,
		Timeout:   cfg.SMTP.Timeout.Duration,
	})
	if err != nil {
		return closeOnError(fmt.Errorf("initialize SMTP mailer: %w", err))
	}
	api, err := cliproxy.New(cfg.CLIProxy.BaseURL, cfg.CLIProxy.ManagementKey, cfg.CLIProxy.Timeout.Duration)
	if err != nil {
		return closeOnError(fmt.Errorf("initialize CLIProxyAPI client: %w", err))
	}
	servicePort, err := cfg.ServicePort()
	if err != nil {
		return closeOnError(fmt.Errorf("resolve CLIProxyAPI service port: %w", err))
	}
	hostname, err := os.Hostname()
	if err != nil {
		return closeOnError(fmt.Errorf("read host name: %w", err))
	}
	manager := alerter.NewManager(sender, store, hostname, cfg.CLIProxy.BaseURL, cfg.Alerts.SendRecovery)
	healthManager, err := healthreport.New(sender, store, healthreport.Options{
		Enabled:       cfg.HealthReport.Enabled,
		Interval:      cfg.HealthReport.Interval.Duration,
		RetryInterval: cfg.HealthReport.RetryInterval.Duration,
		Hostname:      hostname,
		BaseURL:       cfg.CLIProxy.BaseURL,
		Logger:        logger,
	})
	if err != nil {
		return closeOnError(fmt.Errorf("initialize healthy report manager: %w", err))
	}
	runner, err := monitor.NewRunner(monitor.Options{
		API:                    api,
		Collector:              collector.NewHostCollector(),
		Reconciler:             manager,
		HealthReporter:         healthManager,
		Logger:                 logger,
		ServicePort:            servicePort,
		MemoryPercent:          cfg.Thresholds.MemoryPercent,
		DiskPercent:            cfg.Thresholds.DiskPercent,
		TotalTCPConnections:    cfg.Thresholds.TotalTCPConnections,
		ServicePortConnections: cfg.Thresholds.ServicePortConnections,
	})
	if err != nil {
		return closeOnError(fmt.Errorf("initialize monitor runner: %w", err))
	}

	runtime := &Runtime{
		Runner:       runner,
		Logger:       logger,
		Interval:     cfg.Interval.Duration,
		InitialError: stateErr,
	}
	if fileWriter != nil {
		runtime.Close = fileWriter.Close
	}
	return runtime, nil
}
