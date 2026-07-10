package rule

import (
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/mokexinxin/cpa-monitor/internal/cliproxy"
	"github.com/mokexinxin/cpa-monitor/internal/collector"
)

func TestHealth(t *testing.T) {
	t.Parallel()

	healthy := Health(nil)
	assertBatch(t, healthy, ScopeHealth, true, nil)

	downErr := errors.New("connection refused")
	down := Health(downErr)
	assertBatch(t, down, ScopeHealth, true, []string{"health:cliproxy_down"})
	if down.Err() != nil {
		t.Fatalf("health down Batch.Err() = %v, want nil", down.Err())
	}
	condition := down.Conditions[0]
	if condition.Current != "down" || condition.Threshold != "healthy" || condition.Details["error"] != downErr.Error() {
		t.Fatalf("health condition = %#v", condition)
	}
}

func TestMemoryThresholdAndDetails(t *testing.T) {
	t.Parallel()

	usage := collector.MemoryUsage{
		TotalBytes:     1000,
		AvailableBytes: 200,
		UsedBytes:      800,
		UsedPercent:    80,
	}
	assertBatch(t, Memory(usage, 80.1), ScopeMemory, true, nil)

	batch := Memory(usage, 80)
	assertBatch(t, batch, ScopeMemory, true, []string{"resource:memory"})
	condition := batch.Conditions[0]
	if condition.Current != "80.0%" || condition.Threshold != "80.0%" {
		t.Errorf("current/threshold = %q/%q", condition.Current, condition.Threshold)
	}
	wantDetails := map[string]string{
		"kind":            "memory",
		"total_bytes":     "1000",
		"available_bytes": "200",
		"used_bytes":      "800",
		"used_percent":    "80.0%",
	}
	if !reflect.DeepEqual(condition.Details, wantDetails) {
		t.Errorf("details = %#v, want %#v", condition.Details, wantDetails)
	}
}

func TestDisksThresholdDetailsCompletenessSortingAndDeduplication(t *testing.T) {
	t.Parallel()

	statErr := errors.New("statfs failed")
	input := collector.DiskBatch{
		Complete: false,
		Errors:   []collector.DiskError{{MountPoint: "/missing", Err: statErr}},
		Disks: []collector.DiskUsage{
			{MountPoint: "/z", FilesystemType: "xfs", TotalBytes: 1000, UsedBytes: 900, UsedPercent: 90},
			{MountPoint: "/below", FilesystemType: "ext4", TotalBytes: 1000, UsedBytes: 799, UsedPercent: 79.9},
			{MountPoint: "/a", FilesystemType: "ext4", TotalBytes: 2000, UsedBytes: 1600, UsedPercent: 80},
			{MountPoint: "/z", FilesystemType: "xfs", TotalBytes: 1000, UsedBytes: 950, UsedPercent: 95},
		},
	}
	batch := Disks(input, 80)
	assertBatch(t, batch, ScopeDisk, false, []string{"resource:disk:/a", "resource:disk:/z"})
	if !errors.Is(batch.Err(), statErr) {
		t.Fatalf("Batch.Err() = %v, want statfs error", batch.Err())
	}
	if got, want := batch.Conditions[1].Current, "95.0%"; got != want {
		t.Errorf("deduplicated /z Current = %q, want %q", got, want)
	}
	wantDetails := map[string]string{
		"kind":            "disk",
		"mount_point":     "/a",
		"filesystem_type": "ext4",
		"total_bytes":     "2000",
		"used_bytes":      "1600",
		"used_percent":    "80.0%",
	}
	if !reflect.DeepEqual(batch.Conditions[0].Details, wantDetails) {
		t.Errorf("disk details = %#v, want %#v", batch.Conditions[0].Details, wantDetails)
	}
}

func TestTCPThresholdKeysDetailsAndSorting(t *testing.T) {
	t.Parallel()

	assertBatch(t, TCP(collector.TCPUsage{TotalConnections: 2999, ServicePortConnections: 799}, 8317, 3000, 800), ScopeNetwork, true, nil)

	batch := TCP(collector.TCPUsage{TotalConnections: 3000, ServicePortConnections: 800}, 8317, 3000, 800)
	assertBatch(t, batch, ScopeNetwork, true, []string{"network:service_port:8317", "network:total_tcp"})
	service := batch.Conditions[0]
	if service.Current != "800" || service.Threshold != "800" || service.Details["service_port"] != "8317" {
		t.Errorf("service condition = %#v", service)
	}
	total := batch.Conditions[1]
	if total.Current != "3000" || total.Threshold != "3000" || total.Details["connections"] != "3000" {
		t.Errorf("total condition = %#v", total)
	}
}

func TestAuthUnhealthyClassifications(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		file cliproxy.AuthFile
	}{
		{name: "unavailable", file: cliproxy.AuthFile{Unavailable: true}},
		{name: "quota uppercase", file: cliproxy.AuthFile{StatusMessage: "QUOTA EXCEEDED"}},
		{name: "quota capitalized", file: cliproxy.AuthFile{StatusMessage: "Quota exceeded"}},
		{name: "quota mixed embedded", file: cliproxy.AuthFile{StatusMessage: "account qUoTa has been reached"}},
		{name: "usage limit", file: cliproxy.AuthFile{StatusMessage: "USAGE LIMIT reached"}},
		{name: "limit reached", file: cliproxy.AuthFile{StatusMessage: "Limit Reached yesterday"}},
		{name: "exhausted", file: cliproxy.AuthFile{StatusMessage: "credits eXhAuStEd"}},
		{name: "Chinese quota", file: cliproxy.AuthFile{StatusMessage: "账户额度已用尽"}},
		{name: "Chinese limit", file: cliproxy.AuthFile{StatusMessage: "已达到限额"}},
		{name: "non-active", file: cliproxy.AuthFile{Status: "error"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.file.AuthIndex = "42"
			batch := Auth([]cliproxy.AuthFile{tt.file})
			assertBatch(t, batch, ScopeAuth, true, []string{"auth:42"})
		})
	}
}

func TestAuthHealthyAndDisabledSemantics(t *testing.T) {
	t.Parallel()

	for _, file := range []cliproxy.AuthFile{
		{AuthIndex: "1"},
		{AuthIndex: "1", Status: " Active "},
		{AuthIndex: "1", Status: "aCtIvE"},
		{AuthIndex: "1", Disabled: true, StatusMessage: "quota exhausted"},
		{AuthIndex: "1", Disabled: true, Status: "error"},
		{AuthIndex: "1", Disabled: true, Status: "error", StatusMessage: "限额"},
	} {
		assertBatch(t, Auth([]cliproxy.AuthFile{file}), ScopeAuth, true, nil)
	}

	file := cliproxy.AuthFile{
		AuthIndex:     "1",
		Disabled:      true,
		Unavailable:   true,
		Status:        "error",
		StatusMessage: "quota",
	}
	batch := Auth([]cliproxy.AuthFile{file})
	assertBatch(t, batch, ScopeAuth, true, []string{"auth:1"})
	if got, want := batch.Conditions[0].Current, "unavailable"; got != want {
		t.Errorf("disabled unavailable reason = %q, want %q", got, want)
	}
}

func TestAuthStableSortedKeysAndIdentityDetails(t *testing.T) {
	t.Parallel()

	batch := Auth([]cliproxy.AuthFile{
		{
			AuthIndex:     "z",
			Name:          "Zed",
			Type:          "codex",
			Provider:      "openai",
			Email:         "z@example.com",
			Account:       "account-z",
			Status:        "blocked",
			StatusMessage: "quota reached",
		},
		{AuthIndex: "a", Name: "Alpha", Unavailable: true},
	})
	assertBatch(t, batch, ScopeAuth, true, []string{"auth:a", "auth:z"})
	details := batch.Conditions[1].Details
	for key, want := range map[string]string{
		"auth_index":     "z",
		"name":           "Zed",
		"type":           "codex",
		"provider":       "openai",
		"email":          "z@example.com",
		"account":        "account-z",
		"status":         "blocked",
		"status_message": "quota reached",
	} {
		if got := details[key]; got != want {
			t.Errorf("Details[%q] = %q, want %q", key, got, want)
		}
	}
}

func TestAuthMissingAndDuplicateIndexesAreEntryErrors(t *testing.T) {
	t.Parallel()

	batch := Auth([]cliproxy.AuthFile{
		{AuthIndex: "", Unavailable: true},
		{AuthIndex: " duplicate ", Unavailable: true},
		{AuthIndex: "duplicate", StatusMessage: "quota"},
		{AuthIndex: "unique", Unavailable: true},
	})
	assertBatch(t, batch, ScopeAuth, false, []string{"auth:unique"})
	if got, want := len(batch.Errors), 3; got != want {
		t.Fatalf("len(Errors) = %d, want %d (%v)", got, want, batch.Errors)
	}
	if !errors.Is(batch.Errors[0], ErrMissingAuthIndex) {
		t.Errorf("first error = %v, want missing index", batch.Errors[0])
	}
	if !errors.Is(batch.Errors[1], ErrDuplicateAuthIndex) || !errors.Is(batch.Errors[2], ErrDuplicateAuthIndex) {
		t.Errorf("duplicate errors = %v", batch.Errors[1:])
	}
	var entryError AuthEntryError
	if !errors.As(batch.Err(), &entryError) || entryError.Position != 1 {
		t.Errorf("Batch.Err() = %v, first AuthEntryError = %#v", batch.Err(), entryError)
	}
}

func TestAuthHealthyEntriesDoNotRequireIndexes(t *testing.T) {
	t.Parallel()

	batch := Auth([]cliproxy.AuthFile{
		{Status: "active"},
		{Disabled: true, StatusMessage: "QUOTA exhausted"},
	})
	assertBatch(t, batch, ScopeAuth, true, nil)
	if err := batch.Err(); err != nil {
		t.Fatalf("Batch.Err() = %v", err)
	}
}

func TestAuthDuplicateIndexAcrossHealthyAndUnhealthyEntriesIsIncomplete(t *testing.T) {
	t.Parallel()

	batch := Auth([]cliproxy.AuthFile{
		{AuthIndex: "same", Status: "active"},
		{AuthIndex: "same", Unavailable: true},
	})
	assertBatch(t, batch, ScopeAuth, false, nil)
	if got := len(batch.Errors); got != 2 {
		t.Fatalf("len(Errors) = %d, want 2", got)
	}
	for _, err := range batch.Errors {
		if !errors.Is(err, ErrDuplicateAuthIndex) {
			t.Fatalf("error = %v, want duplicate index", err)
		}
	}
}

func TestEveryBatchHasSortedUniqueKeysAndMatchingScopes(t *testing.T) {
	t.Parallel()

	batches := []Batch{
		Health(errors.New("down")),
		Memory(collector.MemoryUsage{UsedPercent: 100}, 80),
		Disks(collector.DiskBatch{Complete: true, Disks: []collector.DiskUsage{
			{MountPoint: "/b", UsedPercent: 90},
			{MountPoint: "/a", UsedPercent: 90},
			{MountPoint: "/b", UsedPercent: 91},
		}}, 80),
		TCP(collector.TCPUsage{TotalConnections: 2, ServicePortConnections: 2}, 80, 1, 1),
		Auth([]cliproxy.AuthFile{{AuthIndex: "b", Unavailable: true}, {AuthIndex: "a", Unavailable: true}}),
	}
	for _, batch := range batches {
		last := ""
		seen := make(map[string]bool)
		for _, condition := range batch.Conditions {
			if condition.Scope != batch.Scope {
				t.Errorf("condition %q scope = %q, batch scope = %q", condition.Key, condition.Scope, batch.Scope)
			}
			if seen[condition.Key] {
				t.Errorf("batch %q has duplicate key %q", batch.Scope, condition.Key)
			}
			if last != "" && condition.Key < last {
				t.Errorf("batch %q keys are not sorted: %q before %q", batch.Scope, last, condition.Key)
			}
			seen[condition.Key] = true
			last = condition.Key
		}
	}
}

func assertBatch(t *testing.T, batch Batch, scope string, complete bool, wantKeys []string) {
	t.Helper()
	if batch.Scope != scope || batch.Complete != complete {
		t.Fatalf("batch scope/complete = %q/%t, want %q/%t", batch.Scope, batch.Complete, scope, complete)
	}
	gotKeys := make([]string, len(batch.Conditions))
	for i := range batch.Conditions {
		gotKeys[i] = batch.Conditions[i].Key
	}
	if !slices.Equal(gotKeys, wantKeys) {
		t.Fatalf("condition keys = %#v, want %#v", gotKeys, wantKeys)
	}
	for _, condition := range batch.Conditions {
		if strings.TrimSpace(condition.Summary) == "" || condition.Details == nil {
			t.Errorf("incomplete condition = %#v", condition)
		}
	}
}
