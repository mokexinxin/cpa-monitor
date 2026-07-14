package cliproxy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
)

const (
	maxVersionResponseBytes = 64 << 10
	CLIProxyAPIReleasesURL  = "https://github.com/router-for-me/CLIProxyAPI/releases"
)

// VersionStatus compares the running CLIProxyAPI binary version with the
// latest GitHub release returned by its protected management endpoint.
type VersionStatus struct {
	CurrentVersion      string
	LatestVersion       string
	ComparisonAvailable bool
	UpdateAvailable     bool
}

// VersionStatus retrieves the latest upstream release and reads the running
// binary version from CLIProxyAPI's X-CPA-VERSION management response header.
func (c *Client) VersionStatus(ctx context.Context) (VersionStatus, error) {
	req, err := c.newRequest(ctx, "v0", "management", "latest-version")
	if err != nil {
		return VersionStatus{}, c.wrapError("build CLIProxyAPI version request", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.managementKey)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return VersionStatus{}, c.wrapError("CLIProxyAPI version request failed", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return VersionStatus{}, fmt.Errorf("CLIProxyAPI latest-version request returned HTTP status %d", resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, int64(maxVersionResponseBytes)+1))
	if err != nil {
		return VersionStatus{}, c.wrapError("read CLIProxyAPI version response", err)
	}
	if len(body) > maxVersionResponseBytes {
		return VersionStatus{}, fmt.Errorf("CLIProxyAPI version response exceeds %d byte limit", maxVersionResponseBytes)
	}
	var envelope struct {
		LatestVersion string `json:"latest-version"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return VersionStatus{}, c.wrapError("decode CLIProxyAPI version response", err)
	}
	current := strings.TrimSpace(resp.Header.Get("X-CPA-VERSION"))
	latest := strings.TrimSpace(envelope.LatestVersion)
	if err := validateVersionValue("current", current); err != nil {
		return VersionStatus{}, err
	}
	if err := validateVersionValue("latest", latest); err != nil {
		return VersionStatus{}, err
	}

	comparison, comparable := compareReleaseVersions(current, latest)
	return VersionStatus{
		CurrentVersion:      current,
		LatestVersion:       latest,
		ComparisonAvailable: comparable,
		UpdateAvailable:     comparable && comparison < 0,
	}, nil
}

func validateVersionValue(name, value string) error {
	if value == "" {
		return fmt.Errorf("CLIProxyAPI %s version is missing", name)
	}
	if len(value) > 128 || !validHeaderValue(value) {
		return fmt.Errorf("CLIProxyAPI %s version is invalid", name)
	}
	return nil
}

type semanticVersion struct {
	core       [3]uint64
	prerelease string
}

func compareReleaseVersions(current, latest string) (int, bool) {
	current = strings.TrimSpace(current)
	latest = strings.TrimSpace(latest)
	if strings.EqualFold(strings.TrimPrefix(current, "v"), strings.TrimPrefix(latest, "v")) {
		return 0, true
	}
	left, leftOK := parseSemanticVersion(current)
	right, rightOK := parseSemanticVersion(latest)
	if !leftOK || !rightOK {
		return 0, false
	}
	for i := range left.core {
		if left.core[i] < right.core[i] {
			return -1, true
		}
		if left.core[i] > right.core[i] {
			return 1, true
		}
	}
	if left.prerelease == right.prerelease {
		return 0, true
	}
	if left.prerelease == "" {
		return 1, true
	}
	if right.prerelease == "" {
		return -1, true
	}
	return comparePrerelease(left.prerelease, right.prerelease), true
}

func parseSemanticVersion(value string) (semanticVersion, bool) {
	value = strings.TrimSpace(value)
	if len(value) > 0 && (value[0] == 'v' || value[0] == 'V') {
		value = value[1:]
	}
	if plus := strings.IndexByte(value, '+'); plus >= 0 {
		value = value[:plus]
	}
	result := semanticVersion{}
	if dash := strings.IndexByte(value, '-'); dash >= 0 {
		result.prerelease = value[dash+1:]
		value = value[:dash]
		if result.prerelease == "" {
			return semanticVersion{}, false
		}
	}
	parts := strings.Split(value, ".")
	if len(parts) != len(result.core) {
		return semanticVersion{}, false
	}
	for i, part := range parts {
		if part == "" || (len(part) > 1 && part[0] == '0') {
			return semanticVersion{}, false
		}
		number, err := strconv.ParseUint(part, 10, 64)
		if err != nil {
			return semanticVersion{}, false
		}
		result.core[i] = number
	}
	return result, true
}

func comparePrerelease(left, right string) int {
	leftParts, rightParts := strings.Split(left, "."), strings.Split(right, ".")
	for i := 0; i < len(leftParts) && i < len(rightParts); i++ {
		if leftParts[i] == rightParts[i] {
			continue
		}
		leftNumber, leftErr := strconv.ParseUint(leftParts[i], 10, 64)
		rightNumber, rightErr := strconv.ParseUint(rightParts[i], 10, 64)
		switch {
		case leftErr == nil && rightErr == nil:
			if leftNumber < rightNumber {
				return -1
			}
			return 1
		case leftErr == nil:
			return -1
		case rightErr == nil:
			return 1
		case leftParts[i] < rightParts[i]:
			return -1
		default:
			return 1
		}
	}
	if len(leftParts) < len(rightParts) {
		return -1
	}
	if len(leftParts) > len(rightParts) {
		return 1
	}
	return 0
}
