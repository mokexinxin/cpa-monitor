package logfile

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestWriterRotatesAndEnforcesLimits(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "monitor.log")
	w, err := NewWriter(Options{Path: path, MaxSizeBytes: 10, MaxFiles: 2, MaxTotalSizeBytes: 20})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = w.Close() })

	if _, err := w.Write([]byte("12345678abcdeFGHIJKLMN")); err != nil {
		t.Fatal(err)
	}
	assertManagedLimits(t, path, 10, 2, 20)

	if _, err := os.Stat(path + ".1"); err != nil {
		t.Fatalf("expected a rotated backup: %v", err)
	}
}

func TestWriterHandlesSingleOversizedWrite(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "monitor.log")
	w, err := NewWriter(Options{Path: path, MaxSizeBytes: 8, MaxFiles: 2, MaxTotalSizeBytes: 16})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

	payload := bytes.Repeat([]byte("x"), 41)
	n, err := w.Write(payload)
	if err != nil {
		t.Fatal(err)
	}
	if n != len(payload) {
		t.Fatalf("Write() = %d, want %d", n, len(payload))
	}
	assertManagedLimits(t, path, 8, 2, 16)
}

func TestWriterCleansExistingFilesOnStartup(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "monitor.log")
	writeSizedFile(t, path, 15)
	writeSizedFile(t, path+".1", 12)
	writeSizedFile(t, path+".2", 8)
	writeSizedFile(t, path+".3", 5)
	unrelated := filepath.Join(dir, "keep.txt")
	writeSizedFile(t, unrelated, 30)

	w, err := NewWriter(Options{Path: path, MaxSizeBytes: 10, MaxFiles: 2, MaxTotalSizeBytes: 20})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

	assertManagedLimits(t, path, 10, 2, 20)
	if _, err := os.Stat(path + ".3"); !os.IsNotExist(err) {
		t.Fatalf("old backup was not removed: %v", err)
	}
	if info, err := os.Stat(unrelated); err != nil || info.Size() != 30 {
		t.Fatalf("unrelated file changed: info=%v err=%v", info, err)
	}
}

func TestWriterTreatsConfiguredPathLiterally(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "monitor[prod]*?.log")
	writeSizedFile(t, path+".01", 7)
	w, err := NewWriter(Options{Path: path, MaxSizeBytes: 8, MaxFiles: 2, MaxTotalSizeBytes: 16})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(bytes.Repeat([]byte("x"), 24)); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path + ".01"); !os.IsNotExist(err) {
		t.Fatalf("non-canonical backup was not removed: %v", err)
	}
	assertManagedLimits(t, path, 8, 2, 16)
}

func TestWriterConcurrentWritesRemainBounded(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "monitor.log")
	w, err := NewWriter(Options{Path: path, MaxSizeBytes: 64, MaxFiles: 3, MaxTotalSizeBytes: 192})
	if err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, _ = fmt.Fprintf(w, "record-%02d-abcdefghijklmnopqrstuvwxyz\n", i)
		}(i)
	}
	wg.Wait()
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	assertManagedLimits(t, path, 64, 3, 192)
}

func TestNewWriterRejectsInvalidOptions(t *testing.T) {
	t.Parallel()

	tests := []Options{
		{},
		{Path: "x", MaxSizeBytes: 0, MaxFiles: 1, MaxTotalSizeBytes: 1},
		{Path: "x", MaxSizeBytes: 2, MaxFiles: -1, MaxTotalSizeBytes: 2},
		{Path: "x", MaxSizeBytes: 2, MaxFiles: 1, MaxTotalSizeBytes: 1},
	}
	for _, opts := range tests {
		if _, err := NewWriter(opts); err == nil {
			t.Fatalf("NewWriter(%+v) unexpectedly succeeded", opts)
		}
	}
}

func assertManagedLimits(t *testing.T, path string, maxSize int64, maxFiles int, maxTotal int64) {
	t.Helper()

	var total int64
	for i := 0; i <= maxFiles; i++ {
		name := path
		if i > 0 {
			name = fmt.Sprintf("%s.%d", path, i)
		}
		info, err := os.Stat(name)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		if info.Size() > maxSize {
			t.Fatalf("%s size = %d, max %d", name, info.Size(), maxSize)
		}
		total += info.Size()
	}
	if total > maxTotal {
		t.Fatalf("managed total = %d, max %d", total, maxTotal)
	}
}

func writeSizedFile(t *testing.T, path string, size int) {
	t.Helper()
	if err := os.WriteFile(path, bytes.Repeat([]byte("z"), size), 0o600); err != nil {
		t.Fatal(err)
	}
}
