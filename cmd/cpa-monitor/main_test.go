package main

import (
	"bytes"
	"io"
	"strings"
	"testing"
)

func TestRunHelp(t *testing.T) {
	t.Parallel()
	var stderr bytes.Buffer
	if code := run([]string{"--help"}, io.Discard, &stderr); code != 0 {
		t.Fatalf("run() code = %d", code)
	}
	if !strings.Contains(stderr.String(), "Usage") || !strings.Contains(stderr.String(), "-once") || !strings.Contains(stderr.String(), "-check-config") {
		t.Fatalf("help output = %q", stderr.String())
	}
}

func TestRunRejectsUnknownFlag(t *testing.T) {
	t.Parallel()
	if code := run([]string{"--not-a-flag"}, io.Discard, io.Discard); code != 2 {
		t.Fatalf("run() code = %d, want 2", code)
	}
}
