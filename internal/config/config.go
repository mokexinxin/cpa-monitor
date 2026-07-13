package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/mail"
	"net/url"
	"os"
	"reflect"
	"strconv"
	"strings"
	"time"
	"unicode"

	"go.yaml.in/yaml/v3"
)

const (
	defaultBaseURL              = "http://127.0.0.1:8317"
	defaultStateFile            = "state/alerts.json"
	defaultLogFile              = "logs/monitor.log"
	maxMegabytesWithoutOverflow = math.MaxInt64 / (1024 * 1024)
)

// Config is the complete cpa-monitor configuration.
type Config struct {
	Interval     Duration           `yaml:"interval"`
	CLIProxy     CLIProxyConfig     `yaml:"cliproxy"`
	Thresholds   ThresholdsConfig   `yaml:"thresholds"`
	Alerts       AlertsConfig       `yaml:"alerts"`
	HealthReport HealthReportConfig `yaml:"health_report"`
	SMTP         SMTPConfig         `yaml:"smtp"`
	DingTalk     DingTalkConfig     `yaml:"dingtalk"`
	Logging      LoggingConfig      `yaml:"logging"`
}

// CLIProxyConfig controls CLIProxyAPI health and management requests.
type CLIProxyConfig struct {
	BaseURL          string   `yaml:"base_url"`
	ManagementKey    string   `yaml:"management_key"`
	ManagementKeyEnv string   `yaml:"management_key_env"`
	ServicePort      int      `yaml:"service_port"`
	Timeout          Duration `yaml:"timeout"`
}

// ThresholdsConfig defines values at which resource alerts become active.
type ThresholdsConfig struct {
	MemoryPercent          float64 `yaml:"memory_percent"`
	DiskPercent            float64 `yaml:"disk_percent"`
	TotalTCPConnections    int     `yaml:"total_tcp_connections"`
	ServicePortConnections int     `yaml:"service_port_connections"`
}

// AlertsConfig controls recovery notifications and persistent deduplication.
type AlertsConfig struct {
	SendRecovery    bool   `yaml:"send_recovery"`
	StateFile       string `yaml:"state_file"`
	PrimaryChannel  string `yaml:"primary_channel"`
	FallbackChannel string `yaml:"fallback_channel"`
}

// HealthReportConfig controls scheduled healthy-status notification delivery. It is
// disabled by default so upgrading an existing configuration never starts a
// new class of email without an explicit choice.
type HealthReportConfig struct {
	Enabled       bool     `yaml:"enabled"`
	Interval      Duration `yaml:"interval"`
	RetryInterval Duration `yaml:"retry_interval"`
	Channel       string   `yaml:"channel"`
}

// SMTPConfig controls localized authenticated SMTP delivery. Exactly one of
// StartTLS and TLS must be enabled; plaintext SMTP is deliberately unsupported.
type SMTPConfig struct {
	Host        string   `yaml:"host"`
	Port        int      `yaml:"port"`
	Language    string   `yaml:"language"`
	Username    string   `yaml:"username"`
	UsernameEnv string   `yaml:"username_env"`
	Password    string   `yaml:"password"`
	PasswordEnv string   `yaml:"password_env"`
	From        string   `yaml:"from"`
	To          []string `yaml:"to"`
	StartTLS    bool     `yaml:"starttls"`
	TLS         bool     `yaml:"tls"`
	Timeout     Duration `yaml:"timeout"`
}

// DingTalkConfig controls delivery through one signed DingTalk custom group
// robot. Only the access token is configurable; the API endpoint is fixed so
// configuration cannot turn the monitor into an arbitrary HTTP client.
type DingTalkConfig struct {
	WebhookToken     string   `yaml:"webhook_token"`
	WebhookTokenEnv  string   `yaml:"webhook_token_env"`
	SigningSecret    string   `yaml:"signing_secret"`
	SigningSecretEnv string   `yaml:"signing_secret_env"`
	Language         string   `yaml:"language"`
	Timeout          Duration `yaml:"timeout"`
	MaxItems         int      `yaml:"max_items"`
	AtUserIDs        []string `yaml:"at_user_ids"`
	AtMobiles        []string `yaml:"at_mobiles"`
	AtAll            bool     `yaml:"at_all"`
}

// LoggingConfig controls structured log filtering and optional bounded files.
type LoggingConfig struct {
	Level string            `yaml:"level"`
	File  FileLoggingConfig `yaml:"file"`
}

// FileLoggingConfig controls local log rotation limits. MaxFiles counts rotated
// backups and does not include the active file.
type FileLoggingConfig struct {
	Enabled        bool   `yaml:"enabled"`
	Path           string `yaml:"path"`
	MaxSizeMB      int64  `yaml:"max_size_mb"`
	MaxFiles       int    `yaml:"max_files"`
	MaxTotalSizeMB int64  `yaml:"max_total_size_mb"`
}

// Default returns a fresh configuration populated with all documented
// defaults. Required credentials and SMTP addresses remain empty.
func Default() Config {
	return Config{
		Interval: Duration{Duration: 60 * time.Second},
		CLIProxy: CLIProxyConfig{
			BaseURL: defaultBaseURL,
			Timeout: Duration{Duration: 10 * time.Second},
		},
		Thresholds: ThresholdsConfig{
			MemoryPercent:          80,
			DiskPercent:            80,
			TotalTCPConnections:    3000,
			ServicePortConnections: 800,
		},
		Alerts: AlertsConfig{
			StateFile:      defaultStateFile,
			PrimaryChannel: "smtp",
		},
		HealthReport: HealthReportConfig{
			Interval:      Duration{Duration: 24 * time.Hour},
			RetryInterval: Duration{Duration: 15 * time.Minute},
		},
		SMTP: SMTPConfig{
			Port:     587,
			Language: "zh-CN",
			StartTLS: true,
			Timeout:  Duration{Duration: 10 * time.Second},
		},
		DingTalk: DingTalkConfig{
			Language: "zh-CN",
			Timeout:  Duration{Duration: 10 * time.Second},
			MaxItems: 10,
		},
		Logging: LoggingConfig{
			Level: "info",
			File: FileLoggingConfig{
				Path:           defaultLogFile,
				MaxSizeMB:      20,
				MaxFiles:       5,
				MaxTotalSizeMB: 80,
			},
		},
	}
}

// Load reads path and applies overrides from the process environment.
func Load(path string) (Config, error) {
	return LoadWithEnv(path, os.LookupEnv)
}

// LoadWithEnv reads, strictly decodes, overrides, and validates a config. An
// environment value overrides its inline secret whenever lookupEnv reports it
// as set, including when the environment value is empty.
func LoadWithEnv(path string, lookupEnv func(string) (string, bool)) (Config, error) {
	contents, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("read configuration: %w", err)
	}

	if err := checkYAMLShape(contents); err != nil {
		return Config{}, err
	}

	cfg := Default()
	decoder := yaml.NewDecoder(bytes.NewReader(contents))
	decoder.KnownFields(true)
	if err := decoder.Decode(&cfg); err != nil {
		return Config{}, fmt.Errorf("decode configuration: %w", err)
	}

	if lookupEnv == nil {
		lookupEnv = func(string) (string, bool) { return "", false }
	}
	cfg.applyEnvironment(lookupEnv)
	cfg.Alerts.PrimaryChannel = normalizeChannel(cfg.Alerts.PrimaryChannel)
	cfg.Alerts.FallbackChannel = normalizeChannel(cfg.Alerts.FallbackChannel)
	cfg.HealthReport.Channel = normalizeChannel(cfg.HealthReport.Channel)
	cfg.SMTP.Language = normalizeLanguage(cfg.SMTP.Language)
	cfg.DingTalk.Language = normalizeLanguage(cfg.DingTalk.Language)
	cfg.DingTalk.AtUserIDs = normalizeStringList(cfg.DingTalk.AtUserIDs)
	cfg.DingTalk.AtMobiles = normalizeStringList(cfg.DingTalk.AtMobiles)
	cfg.Logging.Level = strings.ToLower(strings.TrimSpace(cfg.Logging.Level))

	if err := cfg.Validate(); err != nil {
		return Config{}, fmt.Errorf("validate configuration: %w", err)
	}
	return cfg, nil
}

func (c *Config) applyEnvironment(lookupEnv func(string) (string, bool)) {
	if c.CLIProxy.ManagementKeyEnv != "" {
		if value, ok := lookupEnv(c.CLIProxy.ManagementKeyEnv); ok {
			c.CLIProxy.ManagementKey = value
		}
	}
	if c.SMTP.UsernameEnv != "" {
		if value, ok := lookupEnv(c.SMTP.UsernameEnv); ok {
			c.SMTP.Username = value
		}
	}
	if c.SMTP.PasswordEnv != "" {
		if value, ok := lookupEnv(c.SMTP.PasswordEnv); ok {
			c.SMTP.Password = value
		}
	}
	if c.DingTalk.WebhookTokenEnv != "" {
		if value, ok := lookupEnv(c.DingTalk.WebhookTokenEnv); ok {
			c.DingTalk.WebhookToken = value
		}
	}
	if c.DingTalk.SigningSecretEnv != "" {
		if value, ok := lookupEnv(c.DingTalk.SigningSecretEnv); ok {
			c.DingTalk.SigningSecret = value
		}
	}
}

// Validate checks all invariants without including credential values in an
// error. Callers may safely log returned errors.
func (c Config) Validate() error {
	if c.Interval.Duration <= 0 {
		return errors.New("interval must be greater than zero")
	}
	if c.CLIProxy.Timeout.Duration <= 0 {
		return errors.New("cliproxy.timeout must be greater than zero")
	}
	if err := validatePercent("thresholds.memory_percent", c.Thresholds.MemoryPercent); err != nil {
		return err
	}
	if err := validatePercent("thresholds.disk_percent", c.Thresholds.DiskPercent); err != nil {
		return err
	}
	if c.Thresholds.TotalTCPConnections <= 0 {
		return errors.New("thresholds.total_tcp_connections must be greater than zero")
	}
	if c.Thresholds.ServicePortConnections <= 0 {
		return errors.New("thresholds.service_port_connections must be greater than zero")
	}

	parsedBaseURL, err := parseBaseURL(c.CLIProxy.BaseURL)
	if err != nil {
		return fmt.Errorf("cliproxy.base_url is invalid: %w", err)
	}
	if strings.EqualFold(parsedBaseURL.Scheme, "http") && !isLoopbackHost(parsedBaseURL.Hostname()) {
		return errors.New("cliproxy.base_url must use HTTPS for non-loopback hosts")
	}
	if c.CLIProxy.ServicePort < 0 || c.CLIProxy.ServicePort > 65535 {
		return errors.New("cliproxy.service_port must be between 0 and 65535")
	}
	if _, err := c.ServicePort(); err != nil {
		return fmt.Errorf("cliproxy.service_port is invalid: %w", err)
	}
	if strings.TrimSpace(c.CLIProxy.ManagementKey) == "" {
		return errors.New("cliproxy management key must not be empty")
	}
	if strings.ContainsAny(c.CLIProxy.ManagementKey, "\r\n") {
		return errors.New("cliproxy management key must not contain line breaks")
	}

	if strings.TrimSpace(c.Alerts.StateFile) == "" {
		return errors.New("alerts.state_file must not be empty")
	}
	if !validChannel(c.Alerts.PrimaryChannel) {
		return errors.New("alerts.primary_channel must be smtp or dingtalk")
	}
	if c.Alerts.FallbackChannel != "" && !validChannel(c.Alerts.FallbackChannel) {
		return errors.New("alerts.fallback_channel must be empty, smtp, or dingtalk")
	}
	if c.Alerts.FallbackChannel == c.Alerts.PrimaryChannel {
		return errors.New("alerts.fallback_channel must differ from alerts.primary_channel")
	}
	if c.HealthReport.Channel != "" && !validChannel(c.HealthReport.Channel) {
		return errors.New("health_report.channel must be empty, smtp, or dingtalk")
	}
	if c.HealthReport.Interval.Duration <= 0 {
		return errors.New("health_report.interval must be greater than zero")
	}
	if c.HealthReport.RetryInterval.Duration <= 0 {
		return errors.New("health_report.retry_interval must be greater than zero")
	}

	if c.UsesChannel("smtp") {
		if err := validateSMTP(c.SMTP); err != nil {
			return err
		}
	}
	if c.UsesChannel("dingtalk") {
		if err := validateDingTalk(c.DingTalk); err != nil {
			return err
		}
	}
	if _, ok := map[string]struct{}{"debug": {}, "info": {}, "warn": {}, "error": {}}[c.Logging.Level]; !ok {
		return errors.New("logging.level must be one of debug, info, warn, or error")
	}
	if c.Logging.File.Enabled {
		if strings.TrimSpace(c.Logging.File.Path) == "" {
			return errors.New("logging.file.path must not be empty when file logging is enabled")
		}
		if c.Logging.File.MaxSizeMB <= 0 || c.Logging.File.MaxSizeMB > maxMegabytesWithoutOverflow {
			return errors.New("logging.file.max_size_mb must be a positive representable size")
		}
		if c.Logging.File.MaxFiles <= 0 {
			return errors.New("logging.file.max_files must be greater than zero")
		}
		if c.Logging.File.MaxTotalSizeMB <= 0 || c.Logging.File.MaxTotalSizeMB > maxMegabytesWithoutOverflow {
			return errors.New("logging.file.max_total_size_mb must be a positive representable size")
		}
		if c.Logging.File.MaxTotalSizeMB < c.Logging.File.MaxSizeMB {
			return errors.New("logging.file.max_total_size_mb must be at least logging.file.max_size_mb")
		}
	}
	return nil
}

// HealthReportChannel returns the configured health-report transport, or the
// alert primary channel when health_report.channel is omitted.
func (c Config) HealthReportChannel() string {
	if c.HealthReport.Channel != "" {
		return c.HealthReport.Channel
	}
	return c.Alerts.PrimaryChannel
}

// UsesChannel reports whether runtime construction needs the named transport.
func (c Config) UsesChannel(channel string) bool {
	if c.Alerts.PrimaryChannel == channel || c.Alerts.FallbackChannel == channel {
		return true
	}
	return c.HealthReport.Enabled && c.HealthReportChannel() == channel
}

func validateSMTP(c SMTPConfig) error {
	if c.Timeout.Duration <= 0 {
		return errors.New("smtp.timeout must be greater than zero")
	}
	if c.Language != "zh-CN" && c.Language != "en" {
		return errors.New("smtp.language must be zh-CN or en")
	}
	if !validSMTPHost(c.Host) {
		return errors.New("smtp.host must be a valid hostname or IP address without a port")
	}
	if c.Port < 1 || c.Port > 65535 {
		return errors.New("smtp.port must be between 1 and 65535")
	}
	if !validMailbox(c.From) {
		return errors.New("smtp.from must be a valid email address")
	}
	if len(c.To) == 0 {
		return errors.New("smtp.to must contain at least one email address")
	}
	for _, recipient := range c.To {
		if !validMailbox(recipient) {
			return errors.New("smtp.to contains an invalid email address")
		}
	}
	if (c.Username == "") != (c.Password == "") {
		return errors.New("smtp authentication username and password must both be set or both be empty")
	}
	if c.StartTLS == c.TLS {
		return errors.New("smtp.starttls and smtp.tls: exactly one mode must be enabled")
	}
	return nil
}

func validateDingTalk(c DingTalkConfig) error {
	if c.Timeout.Duration <= 0 {
		return errors.New("dingtalk.timeout must be greater than zero")
	}
	if c.Language != "zh-CN" && c.Language != "en" {
		return errors.New("dingtalk.language must be zh-CN or en")
	}
	if c.MaxItems < 1 || c.MaxItems > 50 {
		return errors.New("dingtalk.max_items must be between 1 and 50")
	}
	if c.AtAll && (len(c.AtUserIDs) != 0 || len(c.AtMobiles) != 0) {
		return errors.New("dingtalk.at_all cannot be combined with at_user_ids or at_mobiles")
	}
	if strings.TrimSpace(c.WebhookToken) == "" {
		return errors.New("dingtalk webhook token must not be empty")
	}
	if strings.TrimSpace(c.SigningSecret) == "" {
		return errors.New("dingtalk signing secret must not be empty")
	}
	if containsUnsafeValue(c.WebhookToken) {
		return errors.New("dingtalk webhook token contains invalid characters")
	}
	if containsUnsafeValue(c.SigningSecret) {
		return errors.New("dingtalk signing secret contains invalid characters")
	}
	for _, value := range append(append([]string(nil), c.AtUserIDs...), c.AtMobiles...) {
		if containsUnsafeValue(value) {
			return errors.New("dingtalk mention lists contain invalid characters")
		}
	}
	return nil
}

func validChannel(channel string) bool { return channel == "smtp" || channel == "dingtalk" }

func normalizeChannel(value string) string { return strings.ToLower(strings.TrimSpace(value)) }

func normalizeLanguage(value string) string {
	value = strings.TrimSpace(value)
	if strings.EqualFold(value, "zh-cn") {
		return "zh-CN"
	}
	return strings.ToLower(value)
}

func normalizeStringList(values []string) []string {
	result := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	return result
}

func containsUnsafeValue(value string) bool {
	if value != strings.TrimSpace(value) {
		return true
	}
	for _, character := range value {
		if unicode.IsSpace(character) || unicode.IsControl(character) {
			return true
		}
	}
	return false
}

func validatePercent(name string, value float64) error {
	if math.IsNaN(value) || math.IsInf(value, 0) || value <= 0 || value > 100 {
		return fmt.Errorf("%s must be greater than 0 and at most 100", name)
	}
	return nil
}

// ServicePort returns an explicit service port or derives one from BaseURL.
// HTTP and HTTPS without explicit ports map to 80 and 443 respectively.
func (c Config) ServicePort() (int, error) {
	if c.CLIProxy.ServicePort != 0 {
		if c.CLIProxy.ServicePort < 1 || c.CLIProxy.ServicePort > 65535 {
			return 0, errors.New("explicit port must be between 1 and 65535")
		}
		return c.CLIProxy.ServicePort, nil
	}

	parsed, err := parseBaseURL(c.CLIProxy.BaseURL)
	if err != nil {
		return 0, err
	}
	if portText := parsed.Port(); portText != "" {
		port, err := strconv.Atoi(portText)
		if err != nil || port < 1 || port > 65535 {
			return 0, errors.New("base URL port must be between 1 and 65535")
		}
		return port, nil
	}
	if strings.EqualFold(parsed.Scheme, "http") {
		return 80, nil
	}
	return 443, nil
}

func parseBaseURL(raw string) (*url.URL, error) {
	if raw == "" || raw != strings.TrimSpace(raw) {
		return nil, errors.New("must be a non-empty URL without surrounding whitespace")
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return nil, errors.New("must be a valid URL")
	}
	if !strings.EqualFold(parsed.Scheme, "http") && !strings.EqualFold(parsed.Scheme, "https") {
		return nil, errors.New("scheme must be http or https")
	}
	if parsed.Host == "" || parsed.Hostname() == "" {
		return nil, errors.New("host must not be empty")
	}
	if parsed.User != nil {
		return nil, errors.New("must not contain user information")
	}
	if parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, errors.New("must not contain a query or fragment")
	}
	if err := validateURLAuthority(parsed.Host); err != nil {
		return nil, err
	}
	if portText := parsed.Port(); portText != "" {
		port, err := strconv.Atoi(portText)
		if err != nil || port < 1 || port > 65535 {
			return nil, errors.New("port must be between 1 and 65535")
		}
	}
	return parsed, nil
}

func validateURLAuthority(authority string) error {
	if strings.HasPrefix(authority, "[") {
		end := strings.LastIndexByte(authority, ']')
		if end < 0 || net.ParseIP(authority[1:end]) == nil {
			return errors.New("host must contain a valid bracketed IPv6 address")
		}
		suffix := authority[end+1:]
		if suffix == "" {
			return nil
		}
		if suffix == ":" {
			return errors.New("port must not be empty")
		}
		if !strings.HasPrefix(suffix, ":") {
			return errors.New("host has invalid text after IPv6 address")
		}
		return nil
	}
	if strings.Count(authority, ":") > 1 {
		return errors.New("IPv6 host must be enclosed in brackets")
	}
	if strings.HasSuffix(authority, ":") {
		return errors.New("port must not be empty")
	}
	return nil
}

func validSMTPHost(host string) bool {
	if host == "" || host != strings.TrimSpace(host) || len(host) > 253 {
		return false
	}
	for _, r := range host {
		if unicode.IsSpace(r) || unicode.IsControl(r) {
			return false
		}
	}
	if net.ParseIP(host) != nil {
		return true
	}
	if strings.ContainsAny(host, ":/[]") {
		return false
	}
	host = strings.TrimSuffix(host, ".")
	if host == "" {
		return false
	}
	for _, label := range strings.Split(host, ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, r := range label {
			if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') && r != '-' {
				return false
			}
		}
	}
	return true
}

func isLoopbackHost(host string) bool {
	if strings.EqualFold(strings.TrimSuffix(host, "."), "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func validMailbox(value string) bool {
	if value == "" || strings.ContainsAny(value, "\r\n") {
		return false
	}
	address, err := mail.ParseAddress(value)
	if err != nil {
		return false
	}
	at := strings.LastIndexByte(address.Address, '@')
	return at > 0 && at < len(address.Address)-1
}

func checkYAMLShape(contents []byte) error {
	decoder := yaml.NewDecoder(bytes.NewReader(contents))
	var document yaml.Node
	if err := decoder.Decode(&document); err != nil {
		if errors.Is(err, io.EOF) {
			return errors.New("decode configuration: empty YAML document")
		}
		return fmt.Errorf("decode configuration: %w", err)
	}
	var extra yaml.Node
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err != nil {
			return fmt.Errorf("decode configuration: %w", err)
		}
		return errors.New("decode configuration: multiple YAML documents are not allowed")
	}
	if err := rejectUnknownFields(&document, reflect.TypeOf(Config{}), ""); err != nil {
		return fmt.Errorf("decode configuration: %w", err)
	}
	return nil
}

func rejectUnknownFields(node *yaml.Node, target reflect.Type, path string) error {
	if node == nil {
		return nil
	}
	if node.Kind == yaml.DocumentNode {
		if len(node.Content) == 0 {
			return nil
		}
		return rejectUnknownFields(node.Content[0], target, path)
	}
	if node.Kind == yaml.AliasNode {
		return rejectUnknownFields(node.Alias, target, path)
	}
	for target.Kind() == reflect.Pointer {
		target = target.Elem()
	}
	if node.Kind != yaml.MappingNode || target.Kind() != reflect.Struct {
		return nil
	}

	fields := make(map[string]reflect.Type, target.NumField())
	for i := 0; i < target.NumField(); i++ {
		field := target.Field(i)
		if !field.IsExported() {
			continue
		}
		name := strings.Split(field.Tag.Get("yaml"), ",")[0]
		if name == "-" {
			continue
		}
		if name == "" {
			name = strings.ToLower(field.Name)
		}
		fields[name] = field.Type
	}

	for i := 0; i+1 < len(node.Content); i += 2 {
		key, value := node.Content[i], node.Content[i+1]
		name := key.Value
		fullPath := name
		if path != "" {
			fullPath = path + "." + name
		}
		fieldType, ok := fields[name]
		if !ok {
			return fmt.Errorf("unknown field %q at line %d", fullPath, key.Line)
		}
		if err := rejectUnknownFields(value, fieldType, fullPath); err != nil {
			return err
		}
	}
	return nil
}
