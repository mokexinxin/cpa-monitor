package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"

	"github.com/mokexinxin/cpa-monitor/internal/alerter"
	"github.com/mokexinxin/cpa-monitor/internal/cliproxy"
	"github.com/mokexinxin/cpa-monitor/internal/collector"
	"github.com/mokexinxin/cpa-monitor/internal/config"
	"github.com/mokexinxin/cpa-monitor/internal/dingtalk"
	"github.com/mokexinxin/cpa-monitor/internal/healthreport"
	"github.com/mokexinxin/cpa-monitor/internal/logfile"
	"github.com/mokexinxin/cpa-monitor/internal/mailer"
	"github.com/mokexinxin/cpa-monitor/internal/monitor"
	"github.com/mokexinxin/cpa-monitor/internal/notification"
	"github.com/mokexinxin/cpa-monitor/internal/state"
)

func Main(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	return Run(ctx, args, stdout, stderr, DefaultDependencies())
}

func DefaultDependencies() Dependencies {
	return Dependencies{
		LoadConfig:       config.Load,
		Build:            buildRuntime,
		TestNotification: testNotification,
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

	transports, err := buildTransports(cfg, logger)
	if err != nil {
		return closeOnError(err)
	}
	alertSender, err := buildAlertRouter(cfg, transports, logger)
	if err != nil {
		return closeOnError(fmt.Errorf("initialize alert notification router: %w", err))
	}
	store, stateErr := state.Open(cfg.Alerts.StateFile)
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
	manager := alerter.NewManager(alertSender, store, hostname, cfg.CLIProxy.BaseURL, cfg.Alerts.SendRecovery)
	healthChannel := cfg.HealthReportChannel()
	healthSender := transports.health[healthChannel]
	if healthSender == nil {
		// A disabled manager still requires a non-nil sender. Its primary
		// transport is always available and no message will be sent.
		healthSender = transports.health[cfg.Alerts.PrimaryChannel]
	}
	healthManager, err := healthreport.New(healthSender, store, healthreport.Options{
		Enabled:        cfg.HealthReport.Enabled,
		Interval:       cfg.HealthReport.Interval.Duration,
		RetryInterval:  cfg.HealthReport.RetryInterval.Duration,
		Hostname:       hostname,
		BaseURL:        cfg.CLIProxy.BaseURL,
		Logger:         logger,
		QuotaFetcher:   api,
		VersionFetcher: api,
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

type transportSet struct {
	alert  map[string]notification.AlertSender
	health map[string]notification.HealthSender
}

func buildTransports(cfg config.Config, logger *slog.Logger) (transportSet, error) {
	result := transportSet{
		alert:  make(map[string]notification.AlertSender, 2),
		health: make(map[string]notification.HealthSender, 2),
	}
	for _, channel := range []string{"smtp", "dingtalk"} {
		if !cfg.UsesChannel(channel) {
			continue
		}
		alertSender, healthSender, err := buildChannel(cfg, channel, logger)
		if err != nil {
			return transportSet{}, err
		}
		result.alert[channel], result.health[channel] = alertSender, healthSender
	}
	return result, nil
}

func buildChannel(cfg config.Config, channel string, logger *slog.Logger) (notification.AlertSender, notification.HealthSender, error) {
	switch channel {
	case "smtp":
		sender, err := mailer.New(mailer.Config{
			Host: cfg.SMTP.Host, Port: cfg.SMTP.Port, Language: cfg.SMTP.Language,
			Username: cfg.SMTP.Username, Password: cfg.SMTP.Password,
			From: cfg.SMTP.From, To: cfg.SMTP.To, StartTLS: cfg.SMTP.StartTLS,
			DirectTLS: cfg.SMTP.TLS, Timeout: cfg.SMTP.Timeout.Duration,
		})
		if err != nil {
			return nil, nil, fmt.Errorf("initialize SMTP mailer: %w", err)
		}
		return sender, sender, nil
	case "dingtalk":
		sender, err := dingtalk.New(dingtalk.Config{
			WebhookToken: cfg.DingTalk.WebhookToken, SigningSecret: cfg.DingTalk.SigningSecret,
			Language: cfg.DingTalk.Language, Timeout: cfg.DingTalk.Timeout.Duration,
			MaxItems: cfg.DingTalk.MaxItems, AtUserIDs: cfg.DingTalk.AtUserIDs,
			AtMobiles: cfg.DingTalk.AtMobiles, AtAll: cfg.DingTalk.AtAll, Logger: logger,
		})
		if err != nil {
			return nil, nil, fmt.Errorf("initialize DingTalk client: %w", err)
		}
		return sender, sender, nil
	default:
		return nil, nil, fmt.Errorf("notification channel %q is unsupported", channel)
	}
}

func buildAlertRouter(cfg config.Config, transports transportSet, logger *slog.Logger) (notification.AlertSender, error) {
	primary := notification.NamedAlertSender{Name: cfg.Alerts.PrimaryChannel, Sender: transports.alert[cfg.Alerts.PrimaryChannel]}
	var fallback *notification.NamedAlertSender
	if cfg.Alerts.FallbackChannel != "" {
		fallback = &notification.NamedAlertSender{Name: cfg.Alerts.FallbackChannel, Sender: transports.alert[cfg.Alerts.FallbackChannel]}
	}
	return notification.NewRouter(primary, fallback, logger)
}
