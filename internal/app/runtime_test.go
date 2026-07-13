package app

import (
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/mokexinxin/cpa-monitor/internal/config"
)

func TestBuildRuntimeCreatesOptionalLoggerAndKeepsCorruptStateError(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	cfg := validRuntimeConfig(dir)
	cfg.Logging.File.Enabled = true
	if err := os.MkdirAll(filepath.Dir(cfg.Alerts.StateFile), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfg.Alerts.StateFile, []byte("not-json"), 0o600); err != nil {
		t.Fatal(err)
	}

	runtime, err := buildRuntime(cfg, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.Runner == nil || runtime.Logger == nil || runtime.Interval != time.Minute {
		t.Fatalf("runtime = %#v", runtime)
	}
	if runtime.InitialError == nil {
		t.Fatal("expected corrupt state to be retained as InitialError")
	}
	if err := closeRuntime(runtime); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(cfg.Logging.File.Path); err != nil {
		t.Fatalf("log file was not created: %v", err)
	}
}

func TestBuildRuntimeWithoutFileLogging(t *testing.T) {
	t.Parallel()

	runtime, err := buildRuntime(validRuntimeConfig(t.TempDir()), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.InitialError != nil {
		t.Fatalf("InitialError = %v", runtime.InitialError)
	}
	if err := closeRuntime(runtime); err != nil {
		t.Fatal(err)
	}
}

func TestBuildRuntimeDingTalkOnlyDoesNotRequireSMTP(t *testing.T) {
	t.Parallel()
	cfg := validRuntimeConfig(t.TempDir())
	cfg.Alerts.PrimaryChannel = "dingtalk"
	cfg.SMTP = config.SMTPConfig{}
	cfg.DingTalk.WebhookToken = "test-token"
	cfg.DingTalk.SigningSecret = "test-secret"
	runtime, err := buildRuntime(cfg, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.Runner == nil {
		t.Fatal("runtime has no runner")
	}
	if err := closeRuntime(runtime); err != nil {
		t.Fatal(err)
	}
}

func TestBuildTransportsOnlyConstructsReferencedChannels(t *testing.T) {
	t.Parallel()
	cfg := validRuntimeConfig(t.TempDir())
	transports, err := buildTransports(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	if transports.alert["smtp"] == nil || transports.alert["dingtalk"] != nil {
		t.Fatalf("SMTP-only transports = %#v", transports.alert)
	}

	cfg.Alerts.PrimaryChannel = "dingtalk"
	cfg.Alerts.FallbackChannel = "smtp"
	cfg.DingTalk.WebhookToken = "test-token"
	cfg.DingTalk.SigningSecret = "test-secret"
	transports, err = buildTransports(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	if transports.alert["smtp"] == nil || transports.alert["dingtalk"] == nil {
		t.Fatalf("primary/fallback transports = %#v", transports.alert)
	}
}

func validRuntimeConfig(dir string) config.Config {
	cfg := config.Default()
	cfg.CLIProxy.ManagementKey = "management-key"
	cfg.SMTP.Host = "smtp.example.com"
	cfg.SMTP.From = "monitor@example.com"
	cfg.SMTP.To = []string{"admin@example.com"}
	cfg.Alerts.StateFile = filepath.Join(dir, "state", "alerts.json")
	cfg.Logging.File.Path = filepath.Join(dir, "logs", "monitor.log")
	return cfg
}
