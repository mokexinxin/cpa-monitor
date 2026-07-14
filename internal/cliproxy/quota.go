package cliproxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	maxAPICallResponseBytes = 1 << 20
	codexUsageURL           = "https://chatgpt.com/backend-api/wham/usage"
	codexUserAgent          = "codex_cli_rs/0.76.0 (Debian 13.0.0; x86_64) WindowsTerminal"
)

// QuotaWindowKind identifies the billing window returned by the Codex usage
// API. Unknown durations retain their primary/secondary position.
type QuotaWindowKind string

const (
	QuotaWindowFiveHour  QuotaWindowKind = "five_hour"
	QuotaWindowWeekly    QuotaWindowKind = "weekly"
	QuotaWindowMonthly   QuotaWindowKind = "monthly"
	QuotaWindowPrimary   QuotaWindowKind = "primary"
	QuotaWindowSecondary QuotaWindowKind = "secondary"
)

// QuotaWindow is one Codex plan-limit window.
type QuotaWindow struct {
	Kind         QuotaWindowKind
	UsedPercent  *float64
	ResetAt      time.Time
	ResetAfter   time.Duration
	WindowLength time.Duration
}

// CodexQuota is the plan and main Codex rate-limit windows for one account.
type CodexQuota struct {
	PlanType string
	Windows  []QuotaWindow
}

// CodexQuota retrieves the real Codex plan-limit usage for one auth entry via
// CLIProxyAPI's protected management api-call endpoint. The access token is
// substituted inside CLIProxyAPI and is never returned to cpa-monitor.
func (c *Client) CodexQuota(ctx context.Context, authIndex, accountID string) (CodexQuota, error) {
	authIndex = strings.TrimSpace(authIndex)
	if authIndex == "" {
		return CodexQuota{}, errors.New("Codex quota auth index is required")
	}
	accountID = strings.TrimSpace(accountID)
	if !validHeaderValue(accountID) {
		return CodexQuota{}, errors.New("Codex account ID contains invalid characters")
	}

	headers := map[string]string{
		"Authorization": "Bearer $TOKEN$",
		"Content-Type":  "application/json",
		"User-Agent":    codexUserAgent,
	}
	if accountID != "" {
		headers["Chatgpt-Account-Id"] = accountID
	}
	payload, err := json.Marshal(struct {
		AuthIndex string            `json:"auth_index"`
		Method    string            `json:"method"`
		URL       string            `json:"url"`
		Header    map[string]string `json:"header"`
	}{AuthIndex: authIndex, Method: http.MethodGet, URL: codexUsageURL, Header: headers})
	if err != nil {
		return CodexQuota{}, c.wrapError("encode CLIProxyAPI Codex quota request", err)
	}

	endpoint := c.baseURL.JoinPath("v0", "management", "api-call")
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), bytes.NewReader(payload))
	if err != nil {
		return CodexQuota{}, c.wrapError("build CLIProxyAPI Codex quota request", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.managementKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return CodexQuota{}, c.wrapError("CLIProxyAPI Codex quota request failed", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return CodexQuota{}, fmt.Errorf("CLIProxyAPI api-call request returned HTTP status %d", resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, int64(maxAPICallResponseBytes)+1))
	if err != nil {
		return CodexQuota{}, c.wrapError("read CLIProxyAPI Codex quota response", err)
	}
	if len(body) > maxAPICallResponseBytes {
		return CodexQuota{}, fmt.Errorf("CLIProxyAPI Codex quota response exceeds %d byte limit", maxAPICallResponseBytes)
	}
	var envelope struct {
		StatusCode int    `json:"status_code"`
		Body       string `json:"body"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return CodexQuota{}, c.wrapError("decode CLIProxyAPI Codex quota response", err)
	}
	if envelope.StatusCode < 200 || envelope.StatusCode >= 300 {
		return CodexQuota{}, fmt.Errorf("Codex usage request returned HTTP status %d", envelope.StatusCode)
	}
	return parseCodexQuota([]byte(envelope.Body))
}

type flexibleNumber struct {
	value float64
	set   bool
}

func (n *flexibleNumber) UnmarshalJSON(data []byte) error {
	data = bytes.TrimSpace(data)
	if len(data) == 0 || bytes.Equal(data, []byte("null")) {
		return nil
	}
	var text string
	if data[0] == '"' {
		if err := json.Unmarshal(data, &text); err != nil {
			return err
		}
	} else {
		text = string(data)
	}
	value, err := strconv.ParseFloat(strings.TrimSpace(text), 64)
	if err != nil || math.IsNaN(value) || math.IsInf(value, 0) {
		return errors.New("quota value must be a finite number")
	}
	n.value, n.set = value, true
	return nil
}

type codexUsageWire struct {
	PlanType       string              `json:"plan_type"`
	PlanTypeCamel  string              `json:"planType"`
	RateLimit      *codexRateLimitWire `json:"rate_limit"`
	RateLimitCamel *codexRateLimitWire `json:"rateLimit"`
}

type codexRateLimitWire struct {
	Allowed              *bool            `json:"allowed"`
	LimitReached         *bool            `json:"limit_reached"`
	LimitReachedCamel    *bool            `json:"limitReached"`
	PrimaryWindow        *codexWindowWire `json:"primary_window"`
	PrimaryWindowCamel   *codexWindowWire `json:"primaryWindow"`
	SecondaryWindow      *codexWindowWire `json:"secondary_window"`
	SecondaryWindowCamel *codexWindowWire `json:"secondaryWindow"`
}

type codexWindowWire struct {
	UsedPercent             flexibleNumber `json:"used_percent"`
	UsedPercentCamel        flexibleNumber `json:"usedPercent"`
	LimitWindowSeconds      flexibleNumber `json:"limit_window_seconds"`
	LimitWindowSecondsCamel flexibleNumber `json:"limitWindowSeconds"`
	ResetAfterSeconds       flexibleNumber `json:"reset_after_seconds"`
	ResetAfterSecondsCamel  flexibleNumber `json:"resetAfterSeconds"`
	ResetAt                 flexibleNumber `json:"reset_at"`
	ResetAtCamel            flexibleNumber `json:"resetAt"`
}

func parseCodexQuota(body []byte) (CodexQuota, error) {
	var wire codexUsageWire
	if err := json.Unmarshal(body, &wire); err != nil {
		return CodexQuota{}, fmt.Errorf("decode Codex usage response: %w", err)
	}
	rateLimit := wire.RateLimit
	if rateLimit == nil {
		rateLimit = wire.RateLimitCamel
	}
	if rateLimit == nil {
		return CodexQuota{}, errors.New("Codex usage response is missing rate-limit data")
	}
	limitReached := boolValue(rateLimit.LimitReached, rateLimit.LimitReachedCamel) || (rateLimit.Allowed != nil && !*rateLimit.Allowed)
	primary := rateLimit.PrimaryWindow
	if primary == nil {
		primary = rateLimit.PrimaryWindowCamel
	}
	secondary := rateLimit.SecondaryWindow
	if secondary == nil {
		secondary = rateLimit.SecondaryWindowCamel
	}

	windows := make([]QuotaWindow, 0, 2)
	if primary != nil {
		window, err := convertCodexWindow(primary, QuotaWindowPrimary, limitReached)
		if err != nil {
			return CodexQuota{}, fmt.Errorf("decode Codex primary quota window: %w", err)
		}
		windows = append(windows, window)
	}
	if secondary != nil {
		window, err := convertCodexWindow(secondary, QuotaWindowSecondary, limitReached)
		if err != nil {
			return CodexQuota{}, fmt.Errorf("decode Codex secondary quota window: %w", err)
		}
		windows = append(windows, window)
	}
	if len(windows) == 0 {
		return CodexQuota{}, errors.New("Codex usage response contains no quota windows")
	}
	planType := strings.TrimSpace(wire.PlanType)
	if planType == "" {
		planType = strings.TrimSpace(wire.PlanTypeCamel)
	}
	return CodexQuota{PlanType: planType, Windows: windows}, nil
}

func convertCodexWindow(wire *codexWindowWire, fallback QuotaWindowKind, limitReached bool) (QuotaWindow, error) {
	used := firstNumber(wire.UsedPercent, wire.UsedPercentCamel)
	var usedPercent *float64
	if used.set {
		if used.value < 0 || used.value > 100 {
			return QuotaWindow{}, errors.New("used percent must be between 0 and 100")
		}
		value := used.value
		usedPercent = &value
	} else if limitReached {
		value := 100.0
		usedPercent = &value
	}

	windowSeconds := firstNumber(wire.LimitWindowSeconds, wire.LimitWindowSecondsCamel)
	resetAfter := firstNumber(wire.ResetAfterSeconds, wire.ResetAfterSecondsCamel)
	resetAt := firstNumber(wire.ResetAt, wire.ResetAtCamel)
	for _, value := range []flexibleNumber{windowSeconds, resetAfter, resetAt} {
		if value.set && value.value < 0 {
			return QuotaWindow{}, errors.New("quota duration and reset values must not be negative")
		}
	}
	window := QuotaWindow{Kind: fallback, UsedPercent: usedPercent}
	if windowSeconds.set {
		window.WindowLength = secondsDuration(windowSeconds.value)
		window.Kind = quotaWindowKind(window.WindowLength, fallback)
	}
	if resetAfter.set {
		window.ResetAfter = secondsDuration(resetAfter.value)
	}
	if resetAt.set && resetAt.value > 0 {
		window.ResetAt = time.Unix(int64(resetAt.value), 0).UTC()
	}
	return window, nil
}

func firstNumber(values ...flexibleNumber) flexibleNumber {
	for _, value := range values {
		if value.set {
			return value
		}
	}
	return flexibleNumber{}
}

func boolValue(values ...*bool) bool {
	for _, value := range values {
		if value != nil {
			return *value
		}
	}
	return false
}

func secondsDuration(seconds float64) time.Duration {
	return time.Duration(seconds * float64(time.Second))
}

func quotaWindowKind(length time.Duration, fallback QuotaWindowKind) QuotaWindowKind {
	switch length {
	case 5 * time.Hour:
		return QuotaWindowFiveHour
	case 7 * 24 * time.Hour:
		return QuotaWindowWeekly
	}
	if length >= 28*24*time.Hour && length <= 31*24*time.Hour {
		return QuotaWindowMonthly
	}
	return fallback
}
