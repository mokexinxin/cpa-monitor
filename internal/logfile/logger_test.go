package logfile

import (
	"bytes"
	"errors"
	"log/slog"
	"strings"
	"testing"
)

func TestNewLoggerFiltersLevelsAndTees(t *testing.T) {
	t.Parallel()

	var console bytes.Buffer
	var file bytes.Buffer
	logger, err := NewLogger("warn", &console, &file)
	if err != nil {
		t.Fatal(err)
	}
	logger.Info("hidden")
	logger.Warn("visible", slog.String("scope", "test"))

	for name, got := range map[string]string{"console": console.String(), "file": file.String()} {
		if strings.Contains(got, "hidden") || !strings.Contains(got, "visible") || !strings.Contains(got, "scope=test") {
			t.Fatalf("%s output = %q", name, got)
		}
	}
}

func TestNewLoggerRejectsUnknownLevel(t *testing.T) {
	t.Parallel()
	if _, err := NewLogger("verbose", &bytes.Buffer{}, nil); err == nil {
		t.Fatal("expected an error")
	}
}

func TestNewLoggerAttemptsFileWhenConsoleFails(t *testing.T) {
	t.Parallel()

	var file bytes.Buffer
	logger, err := NewLogger("info", failingWriter{}, &file)
	if err != nil {
		t.Fatal(err)
	}
	logger.Info("still-persisted")
	if !strings.Contains(file.String(), "still-persisted") {
		t.Fatalf("file output = %q", file.String())
	}
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("console failed") }
