package config

import (
	"path/filepath"
	"runtime"
	"testing"
)

func TestExampleConfig(t *testing.T) {
	t.Parallel()

	_, current, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate test file")
	}
	path := filepath.Join(filepath.Dir(current), "..", "..", "config.example.yaml")
	values := map[string]string{
		"CPA_MANAGEMENT_KEY": "test-management-key",
		"CPA_SMTP_USERNAME":  "test-user",
		"CPA_SMTP_PASSWORD":  "test-password",
	}
	cfg, err := LoadWithEnv(path, func(name string) (string, bool) {
		value, ok := values[name]
		return value, ok
	})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.CLIProxy.ManagementKey != values["CPA_MANAGEMENT_KEY"] || cfg.SMTP.Username != values["CPA_SMTP_USERNAME"] {
		t.Fatal("example environment overrides were not applied")
	}
	if got, err := cfg.ServicePort(); err != nil || got != 8317 {
		t.Fatalf("ServicePort() = %d, %v", got, err)
	}
}
