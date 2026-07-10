package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/mokexinxin/cpa-monitor/internal/alerter"
	"github.com/mokexinxin/cpa-monitor/internal/cliproxy"
	"github.com/mokexinxin/cpa-monitor/internal/collector"
	"github.com/mokexinxin/cpa-monitor/internal/config"
	"github.com/mokexinxin/cpa-monitor/internal/mailer"
	"github.com/mokexinxin/cpa-monitor/internal/monitor"
	"github.com/mokexinxin/cpa-monitor/internal/state"
)

func TestIntegrationOnceRunsRealPipelineWithInjectedCollectorAndMailer(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	statePath := filepath.Join(dir, "state", "alerts.json")
	contents := fmt.Sprintf(`
cliproxy:
  management_key: test-management-key
alerts:
  state_file: %q
smtp:
  host: smtp.example.com
  from: monitor@example.com
  to: [admin@example.com]
`, statePath)
	if err := os.WriteFile(configPath, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}

	api := &integrationAPI{
		healthErr: errors.New("service unavailable"),
		files: []cliproxy.AuthFile{{
			AuthIndex: "account-1", Email: "user@example.com", StatusMessage: "QUOTA exhausted",
		}},
	}
	host := &integrationHost{
		memory: collector.MemoryUsage{TotalBytes: 1000, AvailableBytes: 100, UsedBytes: 900, UsedPercent: 90},
		disks:  collector.DiskBatch{Complete: true},
		tcp: collector.TCPUsage{
			TotalConnections:       4000,
			ServicePortConnections: 900,
		},
	}
	sender := &integrationSender{}
	var loadedPath string
	deps := Dependencies{
		LoadConfig: func(path string) (config.Config, error) {
			loadedPath = path
			return config.Load(path)
		},
		Build: func(cfg config.Config, _ io.Writer) (*Runtime, error) {
			store := state.New(cfg.Alerts.StateFile)
			manager := alerter.NewManager(sender, store, "integration-host", cfg.CLIProxy.BaseURL, cfg.Alerts.SendRecovery)
			port, err := cfg.ServicePort()
			if err != nil {
				return nil, err
			}
			runner, err := monitor.NewRunner(monitor.Options{
				API: api, Collector: host, Reconciler: manager,
				Logger:                 slog.New(slog.NewTextHandler(io.Discard, nil)),
				ServicePort:            port,
				MemoryPercent:          cfg.Thresholds.MemoryPercent,
				DiskPercent:            cfg.Thresholds.DiskPercent,
				TotalTCPConnections:    cfg.Thresholds.TotalTCPConnections,
				ServicePortConnections: cfg.Thresholds.ServicePortConnections,
			})
			if err != nil {
				return nil, err
			}
			return &Runtime{Runner: runner, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Interval: cfg.Interval.Duration}, nil
		},
	}

	if code := Run(context.Background(), []string{"--config", configPath, "--once"}, io.Discard, io.Discard, deps); code != 0 {
		t.Fatalf("Run() code = %d", code)
	}
	if loadedPath != configPath {
		t.Fatalf("loaded path = %q", loadedPath)
	}
	if api.healthCalls != 1 || api.authCalls != 1 || host.memoryCalls != 1 || host.diskCalls != 1 || host.tcpCalls != 1 {
		t.Fatalf("calls: api=%d/%d host=%d/%d/%d", api.healthCalls, api.authCalls, host.memoryCalls, host.diskCalls, host.tcpCalls)
	}

	keys := make([]string, len(sender.events))
	for i := range sender.events {
		keys[i] = sender.events[i].Key
		if sender.events[i].Kind != mailer.Alert {
			t.Fatalf("event = %#v", sender.events[i])
		}
	}
	slices.Sort(keys)
	want := []string{
		"auth:account-1",
		"health:cliproxy_down",
		"network:service_port:8317",
		"network:total_tcp",
		"resource:memory",
	}
	if !slices.Equal(keys, want) {
		t.Fatalf("alert keys = %#v, want %#v", keys, want)
	}
	stored, err := state.Open(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(stored.Records()); got != len(want) {
		t.Fatalf("persisted records = %d, want %d", got, len(want))
	}
}

type integrationAPI struct {
	healthErr   error
	files       []cliproxy.AuthFile
	healthCalls int
	authCalls   int
}

func (a *integrationAPI) CheckHealth(context.Context) error {
	a.healthCalls++
	return a.healthErr
}

func (a *integrationAPI) AuthFiles(context.Context) ([]cliproxy.AuthFile, error) {
	a.authCalls++
	return a.files, nil
}

type integrationHost struct {
	memory collector.MemoryUsage
	disks  collector.DiskBatch
	tcp    collector.TCPUsage

	memoryCalls int
	diskCalls   int
	tcpCalls    int
}

func (h *integrationHost) Memory(context.Context) (collector.MemoryUsage, error) {
	h.memoryCalls++
	return h.memory, nil
}

func (h *integrationHost) Disks(context.Context) (collector.DiskBatch, error) {
	h.diskCalls++
	return h.disks, nil
}

func (h *integrationHost) TCP(context.Context, int) (collector.TCPUsage, error) {
	h.tcpCalls++
	return h.tcp, nil
}

type integrationSender struct {
	events []mailer.Event
}

func (s *integrationSender) Send(_ context.Context, event mailer.Event) error {
	s.events = append(s.events, event)
	return nil
}
