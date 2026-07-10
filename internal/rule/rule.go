package rule

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/mokexinxin/cpa-monitor/internal/cliproxy"
	"github.com/mokexinxin/cpa-monitor/internal/collector"
)

var quotaKeywords = [...]string{
	"quota",
	"usage limit",
	"limit reached",
	"exhausted",
	"额度",
	"限额",
}

// Health turns a failed liveness request into a trustworthy down condition.
// The failure is condition detail, not Batch.Errors, because it describes the
// monitored service rather than a monitor execution failure.
func Health(checkErr error) Batch {
	batch := Batch{Scope: ScopeHealth, Complete: true}
	if checkErr == nil {
		return batch
	}
	batch.Conditions = []Condition{{
		Key:       "health:cliproxy_down",
		Scope:     ScopeHealth,
		Summary:   "CLIProxyAPI health check failed",
		Current:   "down",
		Threshold: "healthy",
		Details: map[string]string{
			"error":   checkErr.Error(),
			"service": "CLIProxyAPI",
		},
	}}
	return batch
}

// Memory evaluates one successfully collected host memory fact.
func Memory(usage collector.MemoryUsage, threshold float64) Batch {
	batch := Batch{Scope: ScopeMemory, Complete: true}
	if !(usage.UsedPercent >= threshold) {
		return batch
	}
	current := formatPercent(usage.UsedPercent)
	batch.Conditions = []Condition{{
		Key:       "resource:memory",
		Scope:     ScopeMemory,
		Summary:   fmt.Sprintf("memory usage %s", current),
		Current:   current,
		Threshold: formatPercent(threshold),
		Details: map[string]string{
			"kind":            "memory",
			"total_bytes":     strconv.FormatUint(usage.TotalBytes, 10),
			"available_bytes": strconv.FormatUint(usage.AvailableBytes, 10),
			"used_bytes":      strconv.FormatUint(usage.UsedBytes, 10),
			"used_percent":    current,
		},
	}}
	return batch
}

// Disks evaluates every successfully collected mount and preserves the
// collector batch's completeness and mount-level errors. Duplicate mount keys
// collapse to the highest reported usage.
func Disks(input collector.DiskBatch, threshold float64) Batch {
	batch := Batch{
		Scope:    ScopeDisk,
		Complete: input.Complete && len(input.Errors) == 0,
		Errors:   make([]error, len(input.Errors)),
	}
	for i := range input.Errors {
		batch.Errors[i] = input.Errors[i]
	}

	byKey := make(map[string]Condition, len(input.Disks))
	percentByKey := make(map[string]float64, len(input.Disks))
	for _, disk := range input.Disks {
		if !(disk.UsedPercent >= threshold) {
			continue
		}
		key := "resource:disk:" + disk.MountPoint
		if oldPercent, exists := percentByKey[key]; exists && oldPercent >= disk.UsedPercent {
			continue
		}
		current := formatPercent(disk.UsedPercent)
		byKey[key] = Condition{
			Key:       key,
			Scope:     ScopeDisk,
			Summary:   fmt.Sprintf("disk %s usage %s", disk.MountPoint, current),
			Current:   current,
			Threshold: formatPercent(threshold),
			Details: map[string]string{
				"kind":            "disk",
				"mount_point":     disk.MountPoint,
				"filesystem_type": disk.FilesystemType,
				"total_bytes":     strconv.FormatUint(disk.TotalBytes, 10),
				"used_bytes":      strconv.FormatUint(disk.UsedBytes, 10),
				"used_percent":    current,
			},
		}
		percentByKey[key] = disk.UsedPercent
	}
	batch.Conditions = sortedConditions(byKey)
	return batch
}

// TCP evaluates total-host and service-port connection counts.
func TCP(usage collector.TCPUsage, servicePort, totalThreshold, serviceThreshold int) Batch {
	batch := Batch{Scope: ScopeNetwork, Complete: true}
	byKey := make(map[string]Condition, 2)
	if usage.TotalConnections >= totalThreshold {
		current := strconv.Itoa(usage.TotalConnections)
		byKey["network:total_tcp"] = Condition{
			Key:       "network:total_tcp",
			Scope:     ScopeNetwork,
			Summary:   fmt.Sprintf("total TCP connections %s", current),
			Current:   current,
			Threshold: strconv.Itoa(totalThreshold),
			Details: map[string]string{
				"kind":        "total_tcp",
				"connections": current,
			},
		}
	}
	if usage.ServicePortConnections >= serviceThreshold {
		key := fmt.Sprintf("network:service_port:%d", servicePort)
		current := strconv.Itoa(usage.ServicePortConnections)
		byKey[key] = Condition{
			Key:       key,
			Scope:     ScopeNetwork,
			Summary:   fmt.Sprintf("TCP connections on service port %d: %s", servicePort, current),
			Current:   current,
			Threshold: strconv.Itoa(serviceThreshold),
			Details: map[string]string{
				"kind":         "service_port_tcp",
				"service_port": strconv.Itoa(servicePort),
				"connections":  current,
			},
		}
	}
	batch.Conditions = sortedConditions(byKey)
	return batch
}

// Auth evaluates the complete auth-files array. Missing and duplicate indexes
// make the batch incomplete and all entries sharing a duplicate index are
// skipped, preventing colliding alert keys and false recoveries.
func Auth(files []cliproxy.AuthFile) Batch {
	batch := Batch{Scope: ScopeAuth, Complete: true}
	indexes := make([]string, len(files))
	reasonsByEntry := make([][]string, len(files))
	counts := make(map[string]int, len(files))
	for i := range files {
		reasonsByEntry[i] = authReasons(files[i])
		indexes[i] = strings.TrimSpace(files[i].AuthIndex)
		if indexes[i] != "" {
			counts[indexes[i]]++
		}
	}

	byKey := make(map[string]Condition, len(files))
	for i, file := range files {
		reasons := reasonsByEntry[i]
		index := indexes[i]
		if index == "" {
			if len(reasons) == 0 {
				continue
			}
			batch.Complete = false
			batch.Errors = append(batch.Errors, AuthEntryError{
				Position: i + 1,
				Err:      ErrMissingAuthIndex,
			})
			continue
		}
		if counts[index] > 1 {
			batch.Complete = false
			batch.Errors = append(batch.Errors, AuthEntryError{
				Position:  i + 1,
				AuthIndex: index,
				Err:       ErrDuplicateAuthIndex,
			})
			continue
		}
		if len(reasons) == 0 {
			continue
		}

		key := "auth:" + index
		identity := firstNonEmpty(file.Email, file.Account, file.Name, index)
		current := strings.Join(reasons, ", ")
		byKey[key] = Condition{
			Key:       key,
			Scope:     ScopeAuth,
			Summary:   fmt.Sprintf("auth %s %s", identity, current),
			Current:   current,
			Threshold: "active and available",
			Details: map[string]string{
				"kind":           "auth",
				"auth_index":     index,
				"name":           file.Name,
				"provider":       file.Provider,
				"type":           file.Type,
				"email":          file.Email,
				"account":        file.Account,
				"status":         file.Status,
				"status_message": file.StatusMessage,
				"disabled":       strconv.FormatBool(file.Disabled),
				"unavailable":    strconv.FormatBool(file.Unavailable),
				"reason":         current,
			},
		}
	}
	batch.Conditions = sortedConditions(byKey)
	return batch
}

func authReasons(file cliproxy.AuthFile) []string {
	reasons := make([]string, 0, 3)
	if file.Unavailable {
		reasons = append(reasons, "unavailable")
	}
	if file.Disabled {
		return reasons
	}
	if containsQuotaKeyword(file.StatusMessage) {
		reasons = append(reasons, "quota-like status message")
	}
	status := strings.TrimSpace(file.Status)
	if status != "" && !strings.EqualFold(status, "active") {
		reasons = append(reasons, "non-active status")
	}
	return reasons
}

func containsQuotaKeyword(message string) bool {
	lower := strings.ToLower(message)
	for _, keyword := range quotaKeywords {
		if strings.Contains(lower, keyword) {
			return true
		}
	}
	return false
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return "unknown"
}

func formatPercent(value float64) string {
	return fmt.Sprintf("%.1f%%", value)
}

func sortedConditions(byKey map[string]Condition) []Condition {
	conditions := make([]Condition, 0, len(byKey))
	for _, condition := range byKey {
		conditions = append(conditions, condition)
	}
	sort.Slice(conditions, func(i, j int) bool {
		return conditions[i].Key < conditions[j].Key
	})
	return conditions
}
