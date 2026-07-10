package logfile

import (
	"errors"
	"io"
	"log/slog"
	"strings"
)

func NewLogger(level string, console io.Writer, file io.Writer) (*slog.Logger, error) {
	if console == nil {
		return nil, errors.New("console log writer is required")
	}
	parsed, err := parseLevel(level)
	if err != nil {
		return nil, err
	}
	writers := []io.Writer{console}
	if file != nil {
		writers = append(writers, file)
	}
	handler := slog.NewTextHandler(fanoutWriter{writers: writers}, &slog.HandlerOptions{Level: parsed})
	return slog.New(handler), nil
}

type fanoutWriter struct {
	writers []io.Writer
}

func (w fanoutWriter) Write(p []byte) (int, error) {
	var writeErrors []error
	for _, writer := range w.writers {
		n, err := writer.Write(p)
		if err != nil {
			writeErrors = append(writeErrors, err)
		} else if n != len(p) {
			writeErrors = append(writeErrors, io.ErrShortWrite)
		}
	}
	return len(p), errors.Join(writeErrors...)
}

func parseLevel(value string) (slog.Level, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "debug":
		return slog.LevelDebug, nil
	case "info":
		return slog.LevelInfo, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return 0, errors.New("logging.level must be one of debug, info, warn, error")
	}
}
