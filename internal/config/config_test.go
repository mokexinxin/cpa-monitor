package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLoadAppliesDocumentedDefaults(t *testing.T) {
	t.Parallel()

	cfg, err := LoadWithEnv("testdata/minimal.yaml", noEnvironment)
	if err != nil {
		t.Fatalf("LoadWithEnv() error = %v", err)
	}

	if got, want := cfg.Interval.Duration, 60*time.Second; got != want {
		t.Errorf("Interval = %v, want %v", got, want)
	}
	if got, want := cfg.CLIProxy.BaseURL, "http://127.0.0.1:8317"; got != want {
		t.Errorf("CLIProxy.BaseURL = %q, want %q", got, want)
	}
	if got, want := cfg.CLIProxy.Timeout.Duration, 10*time.Second; got != want {
		t.Errorf("CLIProxy.Timeout = %v, want %v", got, want)
	}
	if got, want := cfg.Thresholds.MemoryPercent, 80.0; got != want {
		t.Errorf("MemoryPercent = %v, want %v", got, want)
	}
	if got, want := cfg.Thresholds.DiskPercent, 80.0; got != want {
		t.Errorf("DiskPercent = %v, want %v", got, want)
	}
	if got, want := cfg.Thresholds.TotalTCPConnections, 3000; got != want {
		t.Errorf("TotalTCPConnections = %d, want %d", got, want)
	}
	if got, want := cfg.Thresholds.ServicePortConnections, 800; got != want {
		t.Errorf("ServicePortConnections = %d, want %d", got, want)
	}
	if cfg.Alerts.SendRecovery {
		t.Error("Alerts.SendRecovery = true, want false")
	}
	if got, want := cfg.Alerts.StateFile, "state/alerts.json"; got != want {
		t.Errorf("Alerts.StateFile = %q, want %q", got, want)
	}
	if got, want := cfg.SMTP.Port, 587; got != want {
		t.Errorf("SMTP.Port = %d, want %d", got, want)
	}
	if got, want := cfg.SMTP.Timeout.Duration, 10*time.Second; got != want {
		t.Errorf("SMTP.Timeout = %v, want %v", got, want)
	}
	if !cfg.SMTP.StartTLS || cfg.SMTP.TLS {
		t.Errorf("SMTP TLS modes = starttls:%t tls:%t, want true/false", cfg.SMTP.StartTLS, cfg.SMTP.TLS)
	}
	if got, want := cfg.Logging.Level, "info"; got != want {
		t.Errorf("Logging.Level = %q, want %q", got, want)
	}
	if cfg.Logging.File.Enabled {
		t.Error("Logging.File.Enabled = true, want false")
	}
	if got, want := cfg.Logging.File.Path, "logs/monitor.log"; got != want {
		t.Errorf("Logging.File.Path = %q, want %q", got, want)
	}
	if got, want := cfg.Logging.File.MaxSizeMB, int64(20); got != want {
		t.Errorf("Logging.File.MaxSizeMB = %d, want %d", got, want)
	}
	if got, want := cfg.Logging.File.MaxFiles, 5; got != want {
		t.Errorf("Logging.File.MaxFiles = %d, want %d", got, want)
	}
	if got, want := cfg.Logging.File.MaxTotalSizeMB, int64(80); got != want {
		t.Errorf("Logging.File.MaxTotalSizeMB = %d, want %d", got, want)
	}
}

func TestLoadParsesDurations(t *testing.T) {
	t.Parallel()

	path := writeConfig(t, validYAML(`
interval: 90s
cliproxy:
  management_key: key
  timeout: 1500ms
smtp:
  host: smtp.example.com
  from: monitor@example.com
  to: [ops@example.com]
  timeout: 3m
`))
	cfg, err := LoadWithEnv(path, noEnvironment)
	if err != nil {
		t.Fatalf("LoadWithEnv() error = %v", err)
	}
	if got, want := cfg.Interval.Duration, 90*time.Second; got != want {
		t.Errorf("Interval = %v, want %v", got, want)
	}
	if got, want := cfg.CLIProxy.Timeout.Duration, 1500*time.Millisecond; got != want {
		t.Errorf("CLIProxy.Timeout = %v, want %v", got, want)
	}
	if got, want := cfg.SMTP.Timeout.Duration, 3*time.Minute; got != want {
		t.Errorf("SMTP.Timeout = %v, want %v", got, want)
	}
}

func TestLoadRejectsUnknownFieldWithFullPath(t *testing.T) {
	t.Parallel()

	path := writeConfig(t, validYAML(`
cliproxy:
  management_key: key
smtp:
  host: smtp.example.com
  from: monitor@example.com
  to: [ops@example.com]
  surprise: true
`))
	_, err := LoadWithEnv(path, noEnvironment)
	if err == nil {
		t.Fatal("LoadWithEnv() error = nil, want unknown-field error")
	}
	if !strings.Contains(err.Error(), "smtp.surprise") {
		t.Fatalf("error = %q, want nested field path", err)
	}
}

func TestEnvironmentOverridesSecretsWhenSet(t *testing.T) {
	t.Parallel()

	path := writeConfig(t, validYAML(`
cliproxy:
  management_key: inline-management
  management_key_env: MANAGEMENT_ENV
smtp:
  host: smtp.example.com
  username: inline-user
  username_env: USER_ENV
  password: inline-password
  password_env: PASSWORD_ENV
  from: monitor@example.com
  to: [ops@example.com]
`))
	env := map[string]string{
		"MANAGEMENT_ENV": "environment-management",
		"USER_ENV":       "environment-user",
		"PASSWORD_ENV":   "environment-password",
	}
	cfg, err := LoadWithEnv(path, mapLookup(env))
	if err != nil {
		t.Fatalf("LoadWithEnv() error = %v", err)
	}
	if cfg.CLIProxy.ManagementKey != env["MANAGEMENT_ENV"] {
		t.Errorf("management key was not overridden")
	}
	if cfg.SMTP.Username != env["USER_ENV"] {
		t.Errorf("SMTP username was not overridden")
	}
	if cfg.SMTP.Password != env["PASSWORD_ENV"] {
		t.Errorf("SMTP password was not overridden")
	}
}

func TestLoadUsesProcessEnvironment(t *testing.T) {
	const (
		managementName = "CPA_MONITOR_TEST_MANAGEMENT_KEY"
		usernameName   = "CPA_MONITOR_TEST_SMTP_USERNAME"
		passwordName   = "CPA_MONITOR_TEST_SMTP_PASSWORD"
	)
	t.Setenv(managementName, "process-management")
	t.Setenv(usernameName, "process-user")
	t.Setenv(passwordName, "process-password")

	path := writeConfig(t, validYAML(fmt.Sprintf(`
cliproxy:
  management_key_env: %s
smtp:
  host: smtp.example.com
  username_env: %s
  password_env: %s
  from: monitor@example.com
  to: [ops@example.com]
`, managementName, usernameName, passwordName)))
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.CLIProxy.ManagementKey != "process-management" ||
		cfg.SMTP.Username != "process-user" ||
		cfg.SMTP.Password != "process-password" {
		t.Fatal("Load() did not apply process environment overrides")
	}
}

func TestUnsetEnvironmentKeepsInlineSecrets(t *testing.T) {
	t.Parallel()

	path := writeConfig(t, validYAML(`
cliproxy:
  management_key: inline-management
  management_key_env: NOT_SET
smtp:
  host: smtp.example.com
  username: inline-user
  username_env: ALSO_NOT_SET
  password: inline-password
  password_env: STILL_NOT_SET
  from: monitor@example.com
  to: [ops@example.com]
`))
	cfg, err := LoadWithEnv(path, noEnvironment)
	if err != nil {
		t.Fatalf("LoadWithEnv() error = %v", err)
	}
	if got, want := cfg.CLIProxy.ManagementKey, "inline-management"; got != want {
		t.Errorf("ManagementKey = %q, want inline value", got)
	}
	if got, want := cfg.SMTP.Username, "inline-user"; got != want {
		t.Errorf("Username = %q, want inline value", got)
	}
	if got, want := cfg.SMTP.Password, "inline-password"; got != want {
		t.Errorf("Password = %q, want inline value", got)
	}
}

func TestSetEmptyEnvironmentOverridesInlineSecret(t *testing.T) {
	t.Parallel()

	const inlineSecret = "must-not-appear-management-secret"
	path := writeConfig(t, validYAML(fmt.Sprintf(`
cliproxy:
  management_key: %s
  management_key_env: MANAGEMENT_ENV
smtp:
  host: smtp.example.com
  from: monitor@example.com
  to: [ops@example.com]
`, inlineSecret)))
	_, err := LoadWithEnv(path, func(key string) (string, bool) {
		if key == "MANAGEMENT_ENV" {
			return "", true
		}
		return "", false
	})
	if err == nil || !strings.Contains(err.Error(), "management key") {
		t.Fatalf("error = %v, want missing management key", err)
	}
	if strings.Contains(err.Error(), inlineSecret) {
		t.Fatalf("error leaked inline secret: %q", err)
	}
}

func TestServicePort(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		config  Config
		want    int
		wantErr bool
	}{
		{name: "explicit wins", config: Config{CLIProxy: CLIProxyConfig{BaseURL: "not a URL", ServicePort: 1234}}, want: 1234},
		{name: "HTTP default", config: Config{CLIProxy: CLIProxyConfig{BaseURL: "http://example.com/"}}, want: 80},
		{name: "HTTPS default", config: Config{CLIProxy: CLIProxyConfig{BaseURL: "https://example.com/api"}}, want: 443},
		{name: "IPv4 explicit", config: Config{CLIProxy: CLIProxyConfig{BaseURL: "http://127.0.0.1:8317"}}, want: 8317},
		{name: "IPv6 explicit", config: Config{CLIProxy: CLIProxyConfig{BaseURL: "http://[::1]:9443/"}}, want: 9443},
		{name: "IPv6 default", config: Config{CLIProxy: CLIProxyConfig{BaseURL: "https://[2001:db8::1]"}}, want: 443},
		{name: "unsupported scheme", config: Config{CLIProxy: CLIProxyConfig{BaseURL: "ftp://example.com"}}, wantErr: true},
		{name: "port too large", config: Config{CLIProxy: CLIProxyConfig{BaseURL: "http://example.com:65536"}}, wantErr: true},
		{name: "empty URL port", config: Config{CLIProxy: CLIProxyConfig{BaseURL: "http://example.com:"}}, wantErr: true},
		{name: "unbracketed IPv6", config: Config{CLIProxy: CLIProxyConfig{BaseURL: "http://2001:db8::1"}}, wantErr: true},
		{name: "negative explicit", config: Config{CLIProxy: CLIProxyConfig{BaseURL: "http://example.com", ServicePort: -1}}, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := tt.config.ServicePort()
			if (err != nil) != tt.wantErr {
				t.Fatalf("ServicePort() error = %v, wantErr %t", err, tt.wantErr)
			}
			if !tt.wantErr && got != tt.want {
				t.Errorf("ServicePort() = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestValidation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		yaml      string
		wantInErr string
	}{
		{name: "invalid fixture", yaml: readFixture(t, "invalid.yaml"), wantInErr: "memory_percent"},
		{name: "zero interval", yaml: baseYAML("interval: 0s"), wantInErr: "interval"},
		{name: "bad duration", yaml: baseYAML("interval: tomorrow"), wantInErr: "duration"},
		{name: "zero HTTP timeout", yaml: baseYAML("cliproxy:\n  management_key: key\n  timeout: 0s"), wantInErr: "cliproxy.timeout"},
		{name: "zero SMTP timeout", yaml: baseYAML("smtp:\n  host: smtp.example.com\n  from: monitor@example.com\n  to: [ops@example.com]\n  timeout: 0s"), wantInErr: "smtp.timeout"},
		{name: "memory over 100", yaml: baseYAML("thresholds:\n  memory_percent: 101"), wantInErr: "memory_percent"},
		{name: "disk zero", yaml: baseYAML("thresholds:\n  disk_percent: 0"), wantInErr: "disk_percent"},
		{name: "TCP zero", yaml: baseYAML("thresholds:\n  total_tcp_connections: 0"), wantInErr: "total_tcp_connections"},
		{name: "service connections zero", yaml: baseYAML("thresholds:\n  service_port_connections: 0"), wantInErr: "service_port_connections"},
		{name: "service port negative", yaml: baseYAML("cliproxy:\n  management_key: key\n  service_port: -1"), wantInErr: "service_port"},
		{name: "service port high", yaml: baseYAML("cliproxy:\n  management_key: key\n  service_port: 65536"), wantInErr: "service_port"},
		{name: "missing management key", yaml: baseYAML("cliproxy:\n  management_key: ''"), wantInErr: "management key"},
		{name: "blank management key", yaml: baseYAML("cliproxy:\n  management_key: '   '"), wantInErr: "management key"},
		{name: "management key line break", yaml: baseYAML("cliproxy:\n  management_key: |\n    first\n    second"), wantInErr: "management key"},
		{name: "bad base URL", yaml: baseYAML("cliproxy:\n  base_url: ftp://example.com\n  management_key: key"), wantInErr: "base_url"},
		{name: "remote plaintext base URL", yaml: baseYAML("cliproxy:\n  base_url: http://api.example.com\n  management_key: key"), wantInErr: "HTTPS"},
		{name: "missing SMTP host", yaml: baseYAML("smtp:\n  host: ''\n  from: monitor@example.com\n  to: [ops@example.com]"), wantInErr: "smtp.host"},
		{name: "host has port", yaml: baseYAML("smtp:\n  host: smtp.example.com:587\n  from: monitor@example.com\n  to: [ops@example.com]"), wantInErr: "smtp.host"},
		{name: "bad from", yaml: baseYAML("smtp:\n  host: smtp.example.com\n  from: no-at-sign\n  to: [ops@example.com]"), wantInErr: "smtp.from"},
		{name: "empty recipients", yaml: baseYAML("smtp:\n  host: smtp.example.com\n  from: monitor@example.com\n  to: []"), wantInErr: "smtp.to"},
		{name: "bad recipient", yaml: baseYAML("smtp:\n  host: smtp.example.com\n  from: monitor@example.com\n  to: [invalid]"), wantInErr: "smtp.to"},
		{name: "username only", yaml: baseYAML("smtp:\n  host: smtp.example.com\n  username: user\n  from: monitor@example.com\n  to: [ops@example.com]"), wantInErr: "authentication"},
		{name: "password only", yaml: baseYAML("smtp:\n  host: smtp.example.com\n  password: pass\n  from: monitor@example.com\n  to: [ops@example.com]"), wantInErr: "authentication"},
		{name: "both TLS modes", yaml: baseYAML("smtp:\n  host: smtp.example.com\n  from: monitor@example.com\n  to: [ops@example.com]\n  starttls: true\n  tls: true"), wantInErr: "exactly one"},
		{name: "neither TLS mode", yaml: baseYAML("smtp:\n  host: smtp.example.com\n  from: monitor@example.com\n  to: [ops@example.com]\n  starttls: false\n  tls: false"), wantInErr: "exactly one"},
		{name: "bad SMTP port", yaml: baseYAML("smtp:\n  host: smtp.example.com\n  port: 0\n  from: monitor@example.com\n  to: [ops@example.com]"), wantInErr: "smtp.port"},
		{name: "unknown log level", yaml: baseYAML("logging:\n  level: verbose"), wantInErr: "logging.level"},
		{name: "enabled empty path", yaml: baseYAML("logging:\n  file:\n    enabled: true\n    path: ''"), wantInErr: "logging.file.path"},
		{name: "enabled zero max size", yaml: baseYAML("logging:\n  file:\n    enabled: true\n    max_size_mb: 0"), wantInErr: "max_size_mb"},
		{name: "enabled zero backups", yaml: baseYAML("logging:\n  file:\n    enabled: true\n    max_files: 0"), wantInErr: "max_files"},
		{name: "enabled zero total", yaml: baseYAML("logging:\n  file:\n    enabled: true\n    max_total_size_mb: 0"), wantInErr: "max_total_size_mb"},
		{name: "total smaller than file", yaml: baseYAML("logging:\n  file:\n    enabled: true\n    max_size_mb: 20\n    max_total_size_mb: 19"), wantInErr: "max_total_size_mb"},
		{name: "empty state file", yaml: baseYAML("alerts:\n  state_file: ''"), wantInErr: "state_file"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := writeConfig(t, tt.yaml)
			_, err := LoadWithEnv(path, noEnvironment)
			if err == nil {
				t.Fatal("LoadWithEnv() error = nil, want validation error")
			}
			if !strings.Contains(strings.ToLower(err.Error()), strings.ToLower(tt.wantInErr)) {
				t.Errorf("error = %q, want it to mention %q", err, tt.wantInErr)
			}
		})
	}
}

func TestErrorsNeverContainSecrets(t *testing.T) {
	t.Parallel()

	const (
		management = "sensitive-management-value"
		username   = "sensitive-smtp-user"
		password   = "sensitive-smtp-password"
	)
	path := writeConfig(t, validYAML(fmt.Sprintf(`
interval: 0s
cliproxy:
  management_key: %s
smtp:
  host: smtp.example.com
  username: %s
  password: %s
  from: monitor@example.com
  to: [ops@example.com]
`, management, username, password)))
	_, err := LoadWithEnv(path, noEnvironment)
	if err == nil {
		t.Fatal("LoadWithEnv() error = nil, want validation error")
	}
	for _, secret := range []string{management, username, password} {
		if strings.Contains(err.Error(), secret) {
			t.Errorf("error leaked secret %q: %v", secret, err)
		}
	}
}

func TestLoadRejectsMultipleYAMLDocuments(t *testing.T) {
	t.Parallel()

	path := writeConfig(t, readFixture(t, "minimal.yaml")+"\n---\nlogging:\n  level: debug\n")
	_, err := LoadWithEnv(path, noEnvironment)
	if err == nil || !strings.Contains(err.Error(), "multiple YAML documents") {
		t.Fatalf("error = %v, want multiple-document error", err)
	}
}

func noEnvironment(string) (string, bool) { return "", false }

func mapLookup(values map[string]string) func(string) (string, bool) {
	return func(key string) (string, bool) {
		value, ok := values[key]
		return value, ok
	}
}

func writeConfig(t *testing.T, contents string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

func readFixture(t *testing.T, name string) string {
	t.Helper()
	contents, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	return string(contents)
}

func validYAML(contents string) string { return strings.TrimSpace(contents) + "\n" }

func baseYAML(override string) string {
	// Duplicate top-level fields are intentionally avoided: each validation case
	// supplies a complete replacement for any section it needs to change.
	sections := map[string]string{
		"cliproxy": "cliproxy:\n  management_key: key",
		"smtp":     "smtp:\n  host: smtp.example.com\n  from: monitor@example.com\n  to: [ops@example.com]",
	}
	trimmed := strings.TrimSpace(override)
	for name := range sections {
		if strings.HasPrefix(trimmed, name+":") {
			sections[name] = trimmed
			trimmed = ""
			break
		}
	}
	parts := []string{sections["cliproxy"], sections["smtp"]}
	if trimmed != "" {
		parts = append(parts, trimmed)
	}
	return strings.Join(parts, "\n") + "\n"
}
