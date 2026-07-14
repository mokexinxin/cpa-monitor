package cliproxy

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
)

// RecentRequest is one request-count bucket reported by the management API.
type RecentRequest struct {
	Time    string `json:"time"`
	Success int64  `json:"success"`
	Failed  int64  `json:"failed"`
}

// AuthFile is the subset of the auth-files wire contract used by cpa-monitor.
// Unknown response fields are intentionally ignored for forward compatibility.
type AuthFile struct {
	AuthIndex      string          `json:"auth_index"`
	Name           string          `json:"name"`
	Type           string          `json:"type"`
	Provider       string          `json:"provider"`
	Email          string          `json:"email"`
	Account        string          `json:"account"`
	Status         string          `json:"status"`
	StatusMessage  string          `json:"status_message"`
	Disabled       bool            `json:"disabled"`
	Unavailable    bool            `json:"unavailable"`
	Success        int64           `json:"success"`
	Failed         int64           `json:"failed"`
	RecentRequests []RecentRequest `json:"recent_requests"`
	IDToken        json.RawMessage `json:"id_token"`
}

// UnmarshalJSON accepts auth_index as either a string or an integer JSON
// number. Numbers are converted without passing through float64 so large
// indexes do not lose precision.
func (a *AuthFile) UnmarshalJSON(data []byte) error {
	var wire struct {
		AuthIndex      json.RawMessage `json:"auth_index"`
		Name           string          `json:"name"`
		Type           string          `json:"type"`
		Provider       string          `json:"provider"`
		Email          string          `json:"email"`
		Account        string          `json:"account"`
		Status         string          `json:"status"`
		StatusMessage  string          `json:"status_message"`
		Disabled       bool            `json:"disabled"`
		Unavailable    bool            `json:"unavailable"`
		Success        int64           `json:"success"`
		Failed         int64           `json:"failed"`
		RecentRequests []RecentRequest `json:"recent_requests"`
		IDToken        json.RawMessage `json:"id_token"`
	}
	if err := json.Unmarshal(data, &wire); err != nil {
		return err
	}

	index, err := normalizeAuthIndex(wire.AuthIndex)
	if err != nil {
		return err
	}
	*a = AuthFile{
		AuthIndex:      index,
		Name:           wire.Name,
		Type:           wire.Type,
		Provider:       wire.Provider,
		Email:          wire.Email,
		Account:        wire.Account,
		Status:         wire.Status,
		StatusMessage:  wire.StatusMessage,
		Disabled:       wire.Disabled,
		Unavailable:    wire.Unavailable,
		Success:        wire.Success,
		Failed:         wire.Failed,
		RecentRequests: wire.RecentRequests,
		IDToken:        append(json.RawMessage(nil), wire.IDToken...),
	}
	return nil
}

// ChatGPTAccountID returns the Codex account identifier exposed by current
// CLIProxyAPI auth-files responses. It also accepts the older raw-JWT form so
// quota lookups continue to work across CLIProxyAPI upgrades.
func (a AuthFile) ChatGPTAccountID() string {
	raw := bytes.TrimSpace(a.IDToken)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return ""
	}
	if raw[0] == '"' {
		var token string
		if json.Unmarshal(raw, &token) != nil {
			return ""
		}
		parts := strings.Split(token, ".")
		if len(parts) < 2 {
			return ""
		}
		payload, err := base64.RawURLEncoding.DecodeString(parts[1])
		if err != nil {
			return ""
		}
		raw = payload
	}

	var claims map[string]any
	if json.Unmarshal(raw, &claims) != nil {
		return ""
	}
	for _, source := range []map[string]any{claims, nestedStringMap(claims["https://api.openai.com/auth"])} {
		for _, key := range []string{"chatgpt_account_id", "chatgptAccountId"} {
			if value, ok := source[key].(string); ok {
				if value = strings.TrimSpace(value); value != "" {
					return value
				}
			}
		}
	}
	return ""
}

func nestedStringMap(value any) map[string]any {
	if result, ok := value.(map[string]any); ok {
		return result
	}
	return nil
}

func normalizeAuthIndex(raw json.RawMessage) (string, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		// A missing index is an entry-level problem handled by the rule layer.
		return "", nil
	}
	if raw[0] == '"' {
		var value string
		if err := json.Unmarshal(raw, &value); err != nil {
			return "", fmt.Errorf("auth_index must be a JSON string or integer number")
		}
		return value, nil
	}

	start := 0
	if raw[0] == '-' {
		start = 1
	}
	if start == len(raw) {
		return "", fmt.Errorf("auth_index must be a JSON string or integer number")
	}
	for _, digit := range raw[start:] {
		if digit < '0' || digit > '9' {
			return "", fmt.Errorf("auth_index must be a JSON string or integer number")
		}
	}
	if bytes.Equal(raw, []byte("-0")) {
		return "0", nil
	}
	return string(raw), nil
}
