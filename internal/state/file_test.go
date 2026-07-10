package state

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestOpenMissingAndRoundTrip(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "nested", "alerts.json")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := store.Records(); len(got) != 0 {
		t.Fatalf("initial records = %#v", got)
	}

	want := Record{
		Key:         "resource:memory",
		Scope:       "memory",
		Summary:     "memory usage 84.2%",
		Current:     "84.2%",
		Threshold:   "80.0%",
		Details:     map[string]string{"host": "example", "kind": "memory"},
		ActivatedAt: time.Date(2026, 7, 10, 1, 2, 3, 0, time.UTC),
	}
	if err := store.Put(want); err != nil {
		t.Fatal(err)
	}
	if err := store.Save(); err != nil {
		t.Fatal(err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("state mode = %o, want 600", info.Mode().Perm())
	}
	dirInfo, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if dirInfo.Mode().Perm() != 0o750 {
		t.Fatalf("state directory mode = %o, want 750", dirInfo.Mode().Perm())
	}

	reloaded, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := reloaded.Get(want.Key)
	if !ok || got.Key != want.Key || got.Scope != want.Scope || got.Summary != want.Summary || !got.ActivatedAt.Equal(want.ActivatedAt) {
		t.Fatalf("reloaded record = %#v, ok=%v", got, ok)
	}
	if got.Details["host"] != "example" {
		t.Fatalf("details = %#v", got.Details)
	}
}

func TestSaveIsDeterministicAndLeavesNoTemporaryFiles(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "alerts.json")
	store := New(path)
	for _, record := range []Record{
		{Key: "z:key", Scope: "z", ActivatedAt: time.Unix(2, 0).UTC()},
		{Key: "a:key", Scope: "a", ActivatedAt: time.Unix(1, 0).UTC()},
	} {
		if err := store.Put(record); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Save(); err != nil {
		t.Fatal(err)
	}
	first, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Save(); err != nil {
		t.Fatal(err)
	}
	second, _ := os.ReadFile(path)
	if string(first) != string(second) {
		t.Fatalf("state output is not deterministic:\n%s\n%s", first, second)
	}
	if !strings.HasSuffix(string(first), "\n") || strings.Index(string(first), "a:key") > strings.Index(string(first), "z:key") {
		t.Fatalf("unexpected JSON ordering: %s", first)
	}
	matches, err := filepath.Glob(filepath.Join(dir, ".alerts.json.tmp-*"))
	if err != nil || len(matches) != 0 {
		t.Fatalf("temporary files = %v, err=%v", matches, err)
	}
}

func TestOpenCorruptOrUnknownVersionReturnsUsableEmptyStore(t *testing.T) {
	t.Parallel()

	for name, contents := range map[string]string{
		"corrupt":  `{not-json`,
		"version":  `{"version":99,"active":[]}`,
		"trailing": `{"version":1,"active":[]} {"extra":true}`,
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "alerts.json")
			if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
				t.Fatal(err)
			}
			store, err := Open(path)
			if err == nil {
				t.Fatal("expected load error")
			}
			if store == nil || len(store.Records()) != 0 {
				t.Fatalf("store = %#v", store)
			}
		})
	}
}

func TestOpenTightensExistingFilePermissions(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "alerts.json")
	if err := os.WriteFile(path, []byte("{\"version\":1,\"active\":[]}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("state mode = %o, want 600", info.Mode().Perm())
	}
}

func TestSaveFailurePreservesOldFileAndMemory(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "alerts.json")
	store := New(path)
	if err := store.Put(Record{Key: "old", Scope: "test"}); err != nil {
		t.Fatal(err)
	}
	if err := store.Save(); err != nil {
		t.Fatal(err)
	}
	old, _ := os.ReadFile(path)

	if err := store.Put(Record{Key: "new", Scope: "test"}); err != nil {
		t.Fatal(err)
	}
	store.save = func(string, []byte) error { return errors.New("injected failure") }
	if err := store.Save(); err == nil {
		t.Fatal("expected save failure")
	}
	after, _ := os.ReadFile(path)
	if string(after) != string(old) {
		t.Fatal("old state file changed after failed save")
	}
	if _, ok := store.Get("new"); !ok {
		t.Fatal("in-memory mutation was rolled back")
	}
}

func TestAtomicWriteRenameFailureCleansTemporaryFile(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	target := filepath.Join(dir, "alerts.json")
	if err := os.Mkdir(target, 0o750); err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(target, "keep")
	if err := os.WriteFile(sentinel, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	for attempt := 0; attempt < 2; attempt++ {
		if err := atomicWrite(target, []byte("new")); err == nil {
			t.Fatal("atomicWrite() unexpectedly replaced a directory")
		}
	}
	if got, err := os.ReadFile(sentinel); err != nil || string(got) != "old" {
		t.Fatalf("old target changed: %q, %v", got, err)
	}
	matches, err := filepath.Glob(filepath.Join(dir, ".alerts.json.tmp-*"))
	if err != nil || len(matches) != 0 {
		t.Fatalf("temporary files = %v, err=%v", matches, err)
	}
}

func TestConcurrentSavesCannotWriteAnOlderSnapshotLast(t *testing.T) {
	path := filepath.Join(t.TempDir(), "alerts.json")
	store := New(path)
	if err := store.Put(Record{Key: "old", Scope: "test"}); err != nil {
		t.Fatal(err)
	}

	type saveCall struct {
		data    string
		release chan struct{}
	}
	calls := make(chan saveCall, 2)
	store.save = func(_ string, data []byte) error {
		call := saveCall{data: string(data), release: make(chan struct{})}
		calls <- call
		<-call.release
		return nil
	}

	doneA := make(chan error, 1)
	go func() { doneA <- store.Save() }()
	first := <-calls
	if !strings.Contains(first.data, `"old"`) {
		t.Fatalf("first snapshot = %s", first.data)
	}
	if err := store.Put(Record{Key: "new", Scope: "test"}); err != nil {
		t.Fatal(err)
	}
	startedB := make(chan struct{})
	doneB := make(chan error, 1)
	go func() {
		close(startedB)
		doneB <- store.Save()
	}()
	<-startedB
	select {
	case second := <-calls:
		close(second.release)
		close(first.release)
		t.Fatal("second Save reached the writer before the first completed")
	case <-time.After(50 * time.Millisecond):
	}
	close(first.release)
	if err := <-doneA; err != nil {
		t.Fatal(err)
	}
	second := <-calls
	if !strings.Contains(second.data, `"new"`) {
		t.Fatalf("second snapshot = %s", second.data)
	}
	close(second.release)
	if err := <-doneB; err != nil {
		t.Fatal(err)
	}
}

func TestRecordMutationAndValidation(t *testing.T) {
	t.Parallel()

	store := New(filepath.Join(t.TempDir(), "alerts.json"))
	if err := store.Put(Record{}); err == nil {
		t.Fatal("expected validation error")
	}
	if err := store.Put(Record{Key: "k", Scope: "scope"}); err != nil {
		t.Fatal(err)
	}
	if got := store.ByScope("scope"); len(got) != 1 {
		t.Fatalf("ByScope = %#v", got)
	}
	store.Delete("k")
	if _, ok := store.Get("k"); ok {
		t.Fatal("record was not deleted")
	}
}

func TestStateDocumentIsValidJSON(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "alerts.json")
	store := New(path)
	if err := store.Save(); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	var decoded any
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
}
