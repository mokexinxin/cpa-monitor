// Package dingtalk delivers monitoring notifications through a signed custom
// group robot webhook.
package dingtalk

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/mokexinxin/cpa-monitor/internal/notification"
)

const (
	maxResponseBytes = 1 << 20
	rateLimitCode    = 410100
	rateLimitPause   = 10 * time.Minute
)

// Config controls signed DingTalk custom robot delivery.
type Config struct {
	WebhookToken  string
	SigningSecret string
	Language      string
	Timeout       time.Duration
	MaxItems      int
	AtUserIDs     []string
	AtMobiles     []string
	AtAll         bool
	Logger        *slog.Logger
}

// Client is safe for concurrent use.
type Client struct {
	token      string
	secret     string
	language   string
	maxItems   int
	at         atContent
	httpClient *http.Client
	logger     *slog.Logger
	now        func() time.Time

	mu           sync.Mutex
	blockedUntil time.Time
}

func New(config Config) (*Client, error) {
	if strings.TrimSpace(config.WebhookToken) == "" {
		return nil, errors.New("DingTalk webhook token is required")
	}
	if strings.TrimSpace(config.SigningSecret) == "" {
		return nil, errors.New("DingTalk signing secret is required")
	}
	if strings.ContainsAny(config.WebhookToken, "\r\n") || strings.ContainsAny(config.SigningSecret, "\r\n") {
		return nil, errors.New("DingTalk credentials contain invalid characters")
	}
	if err := validateLanguage(config.Language); err != nil {
		return nil, err
	}
	if config.Timeout <= 0 {
		return nil, errors.New("DingTalk timeout must be greater than zero")
	}
	if config.MaxItems < 1 || config.MaxItems > 50 {
		return nil, errors.New("DingTalk max items must be between 1 and 50")
	}
	if config.AtAll && (len(config.AtUserIDs) != 0 || len(config.AtMobiles) != 0) {
		return nil, errors.New("DingTalk at all cannot be combined with individual mentions")
	}
	if config.Logger == nil {
		config.Logger = slog.Default()
	}
	return &Client{
		token:      config.WebhookToken,
		secret:     config.SigningSecret,
		language:   config.Language,
		maxItems:   config.MaxItems,
		at:         atContent{Mobiles: append([]string(nil), config.AtMobiles...), UserIDs: append([]string(nil), config.AtUserIDs...), AtAll: config.AtAll},
		httpClient: &http.Client{Timeout: config.Timeout},
		logger:     config.Logger,
		now:        time.Now,
	}, nil
}

func (c *Client) SendBatch(ctx context.Context, batch notification.Batch) error {
	payload, err := alertPayload(batch, c.language, c.maxItems, c.at)
	if err != nil {
		return err
	}
	return c.send(ctx, payload)
}

func (c *Client) SendHealth(ctx context.Context, report notification.HealthReport) error {
	payload, err := healthPayload(report, c.language, c.at)
	if err != nil {
		return err
	}
	return c.send(ctx, payload)
}

func (c *Client) send(ctx context.Context, payload markdownPayload) error {
	if ctx == nil {
		return errors.New("DingTalk context must not be nil")
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("send DingTalk notification: %w", err)
	}
	now := c.now()
	c.mu.Lock()
	blockedUntil := c.blockedUntil
	c.mu.Unlock()
	if now.Before(blockedUntil) {
		return fmt.Errorf("DingTalk robot is cooling down after rate limiting until %s", blockedUntil.UTC().Format(time.RFC3339))
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("encode DingTalk notification: %w", err)
	}
	timestamp := now.UnixMilli()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, signedWebhookURL(c.token, c.secret, timestamp), bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("create DingTalk request: %w", err)
	}
	request.Header.Set("Content-Type", "application/json; charset=utf-8")
	response, err := c.httpClient.Do(request)
	if err != nil {
		return c.safeError("send DingTalk request", err)
	}
	defer response.Body.Close()
	responseBody, readErr := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
	if readErr != nil {
		return fmt.Errorf("read DingTalk response: %w", readErr)
	}
	if len(responseBody) > maxResponseBytes {
		return errors.New("DingTalk response exceeds size limit")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("DingTalk returned HTTP status %d", response.StatusCode)
	}

	var result apiResponse
	if err := json.Unmarshal(responseBody, &result); err != nil {
		return fmt.Errorf("decode DingTalk response: %w", err)
	}
	code, err := result.code()
	if err != nil {
		return err
	}
	if code == 0 {
		return nil
	}
	if code == rateLimitCode {
		until := now.Add(rateLimitPause)
		c.mu.Lock()
		if until.After(c.blockedUntil) {
			c.blockedUntil = until
		}
		c.mu.Unlock()
		c.logger.WarnContext(ctx, "DingTalk robot rate limited; entering cooldown", "blocked_until", until.UTC())
	}
	message := strings.TrimSpace(result.ErrorMessage)
	if message == "" {
		message = "unknown API error"
	}
	return fmt.Errorf("DingTalk API error %d: %s", code, c.redact(message))
}

func (c *Client) safeError(stage string, cause error) error {
	return &clientError{message: stage + ": " + c.redact(cause.Error()), cause: cause}
}

func (c *Client) redact(value string) string {
	for _, secret := range []string{c.token, c.secret} {
		if secret != "" {
			for _, representation := range []string{secret, url.QueryEscape(secret), url.PathEscape(secret)} {
				value = strings.ReplaceAll(value, representation, "[REDACTED]")
			}
		}
	}
	return value
}

type clientError struct {
	message string
	cause   error
}

func (e *clientError) Error() string { return e.message }
func (e *clientError) Unwrap() error { return e.cause }

type apiResponse struct {
	ErrorCode    json.RawMessage `json:"errcode"`
	ErrorMessage string          `json:"errmsg"`
}

func (r apiResponse) code() (int64, error) {
	if len(r.ErrorCode) == 0 {
		return 0, errors.New("DingTalk response does not contain errcode")
	}
	var number int64
	if err := json.Unmarshal(r.ErrorCode, &number); err == nil {
		return number, nil
	}
	var text string
	if err := json.Unmarshal(r.ErrorCode, &text); err != nil {
		return 0, errors.New("DingTalk response contains an invalid errcode")
	}
	number, err := strconv.ParseInt(text, 10, 64)
	if err != nil {
		return 0, errors.New("DingTalk response contains an invalid errcode")
	}
	return number, nil
}

var (
	_ notification.AlertSender  = (*Client)(nil)
	_ notification.HealthSender = (*Client)(nil)
)
