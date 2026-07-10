package cliproxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const maxAuthFilesResponseBytes = 8 << 20

// Client talks only to CLIProxyAPI's public HTTP wire contract.
type Client struct {
	baseURL       *url.URL
	managementKey string
	httpClient    *http.Client
}

// New constructs a client with a process-wide request timeout. The base URL
// may include a path prefix and may have an optional trailing slash.
func New(baseURL, managementKey string, timeout time.Duration) (*Client, error) {
	if strings.TrimSpace(managementKey) == "" {
		return nil, fmt.Errorf("management key must not be empty")
	}
	if !validHeaderValue(managementKey) {
		return nil, fmt.Errorf("management key contains invalid characters")
	}
	if timeout <= 0 {
		return nil, fmt.Errorf("HTTP timeout must be greater than zero")
	}

	parsed, err := url.Parse(baseURL)
	if err != nil {
		return nil, redactError("invalid CLIProxyAPI base URL", err, managementKey)
	}
	if !parsed.IsAbs() || parsed.Hostname() == "" {
		return nil, fmt.Errorf("CLIProxyAPI base URL must be absolute and include a host")
	}
	if !strings.EqualFold(parsed.Scheme, "http") && !strings.EqualFold(parsed.Scheme, "https") {
		return nil, fmt.Errorf("CLIProxyAPI base URL scheme must be http or https")
	}
	if strings.EqualFold(parsed.Scheme, "http") && !isLoopbackHost(parsed.Hostname()) {
		return nil, fmt.Errorf("CLIProxyAPI base URL must use HTTPS for non-loopback hosts")
	}
	if parsed.User != nil {
		return nil, fmt.Errorf("CLIProxyAPI base URL must not contain user information")
	}
	if parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" {
		return nil, fmt.Errorf("CLIProxyAPI base URL must not contain a query or fragment")
	}
	parsed.Scheme = strings.ToLower(parsed.Scheme)

	return &Client{
		baseURL:       parsed,
		managementKey: managementKey,
		httpClient: &http.Client{
			Timeout: timeout,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}, nil
}

// CheckHealth reports success only for an HTTP 200 from /healthz. Its body is
// deliberately ignored because liveness is defined by the status code alone.
func (c *Client) CheckHealth(ctx context.Context) error {
	req, err := c.newRequest(ctx, "healthz")
	if err != nil {
		return c.wrapError("build CLIProxyAPI health request", err)
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return c.wrapError("CLIProxyAPI health request failed", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("CLIProxyAPI health check returned HTTP status %d", resp.StatusCode)
	}
	return nil
}

// AuthFiles retrieves and decodes the management auth-files response.
func (c *Client) AuthFiles(ctx context.Context) ([]AuthFile, error) {
	req, err := c.newRequest(ctx, "v0", "management", "auth-files")
	if err != nil {
		return nil, c.wrapError("build CLIProxyAPI management request", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.managementKey)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, c.wrapError("CLIProxyAPI management request failed", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("CLIProxyAPI auth-files request returned HTTP status %d", resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, int64(maxAuthFilesResponseBytes)+1))
	if err != nil {
		return nil, c.wrapError("read CLIProxyAPI auth-files response", err)
	}
	if len(body) > maxAuthFilesResponseBytes {
		return nil, fmt.Errorf("CLIProxyAPI auth-files response exceeds %d byte limit", maxAuthFilesResponseBytes)
	}

	var envelope struct {
		Files json.RawMessage `json:"files"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return nil, c.wrapError("decode CLIProxyAPI auth-files response", err)
	}
	rawFiles := bytes.TrimSpace(envelope.Files)
	if len(rawFiles) == 0 {
		return nil, fmt.Errorf("CLIProxyAPI auth-files response is missing required files array")
	}
	if rawFiles[0] != '[' {
		return nil, fmt.Errorf("CLIProxyAPI auth-files response field files must be an array")
	}

	var files []AuthFile
	if err := json.Unmarshal(rawFiles, &files); err != nil {
		return nil, c.wrapError("decode CLIProxyAPI auth-files entries", err)
	}
	return files, nil
}

func (c *Client) newRequest(ctx context.Context, pathElements ...string) (*http.Request, error) {
	endpoint := c.baseURL.JoinPath(pathElements...)
	return http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
}

func (c *Client) wrapError(prefix string, cause error) error {
	return redactError(prefix, cause, c.managementKey)
}

type safeWrappedError struct {
	message string
	cause   error
}

func (e *safeWrappedError) Error() string { return e.message }
func (e *safeWrappedError) Unwrap() error { return e.cause }

func redactError(prefix string, cause error, secret string) error {
	detail := cause.Error()
	if secret != "" {
		detail = strings.ReplaceAll(detail, secret, "[REDACTED]")
	}
	return &safeWrappedError{
		message: prefix + ": " + detail,
		cause:   cause,
	}
}

func validHeaderValue(value string) bool {
	for i := 0; i < len(value); i++ {
		if value[i] <= 31 || value[i] == 127 {
			return false
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
