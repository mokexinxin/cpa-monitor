package logfile

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
)

type Options struct {
	Path              string
	MaxSizeBytes      int64
	MaxFiles          int
	MaxTotalSizeBytes int64
}

type Writer struct {
	mu     sync.Mutex
	opts   Options
	file   *os.File
	size   int64
	closed bool
}

type backupFile struct {
	index     int
	path      string
	canonical bool
}

func NewWriter(opts Options) (*Writer, error) {
	if strings.TrimSpace(opts.Path) == "" {
		return nil, errors.New("log path is required")
	}
	if opts.MaxSizeBytes <= 0 {
		return nil, errors.New("max log file size must be positive")
	}
	if opts.MaxFiles < 0 {
		return nil, errors.New("max rotated files cannot be negative")
	}
	if opts.MaxTotalSizeBytes < opts.MaxSizeBytes {
		return nil, errors.New("max total log size must be at least max file size")
	}

	if err := os.MkdirAll(filepath.Dir(opts.Path), 0o750); err != nil {
		return nil, fmt.Errorf("create log directory: %w", err)
	}
	w := &Writer{opts: opts}
	if err := w.cleanupExisting(); err != nil {
		return nil, err
	}
	if err := w.open(); err != nil {
		return nil, err
	}
	return w, nil
}

func (w *Writer) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.closed || w.file == nil {
		return 0, os.ErrClosed
	}
	if len(p) == 0 {
		return 0, nil
	}

	written := 0
	for len(p) > 0 {
		if w.size >= w.opts.MaxSizeBytes {
			if err := w.rotate(); err != nil {
				return written, err
			}
		}

		capacity := w.opts.MaxSizeBytes - w.size
		chunk := int64(len(p))
		if chunk > capacity {
			chunk = capacity
		}
		n, err := w.file.Write(p[:int(chunk)])
		written += n
		w.size += int64(n)
		p = p[n:]
		if err != nil {
			return written, fmt.Errorf("write log file: %w", err)
		}
		if n == 0 {
			return written, io.ErrShortWrite
		}
		if err := w.enforceTotal(); err != nil {
			return written, err
		}
	}
	return written, nil
}

func (w *Writer) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return nil
	}
	w.closed = true
	if w.file == nil {
		return nil
	}
	err := w.file.Close()
	w.file = nil
	return err
}

func (w *Writer) open() error {
	f, err := os.OpenFile(w.opts.Path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("open log file: %w", err)
	}
	if err := f.Chmod(0o600); err != nil {
		_ = f.Close()
		return fmt.Errorf("secure log file permissions: %w", err)
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return fmt.Errorf("stat log file: %w", err)
	}
	w.file = f
	w.size = info.Size()
	return nil
}

func (w *Writer) rotate() error {
	if w.file != nil {
		if err := w.file.Close(); err != nil {
			return fmt.Errorf("close log before rotation: %w", err)
		}
		w.file = nil
	}

	if w.opts.MaxFiles == 0 {
		if err := removeIfExists(w.opts.Path); err != nil {
			return err
		}
	} else {
		if err := removeIfExists(w.backupPath(w.opts.MaxFiles)); err != nil {
			return err
		}
		for i := w.opts.MaxFiles - 1; i >= 1; i-- {
			from := w.backupPath(i)
			to := w.backupPath(i + 1)
			if err := renameIfExists(from, to); err != nil {
				return err
			}
		}
		if err := renameIfExists(w.opts.Path, w.backupPath(1)); err != nil {
			return err
		}
	}
	if err := w.enforceTotal(); err != nil {
		return err
	}
	return w.open()
}

func (w *Writer) cleanupExisting() error {
	backups, err := w.backups()
	if err != nil {
		return err
	}
	for _, backup := range backups {
		if backup.index > w.opts.MaxFiles || !backup.canonical {
			if err := removeIfExists(backup.path); err != nil {
				return err
			}
			continue
		}
		if err := trimFile(backup.path, w.opts.MaxSizeBytes); err != nil {
			return err
		}
	}
	if err := trimFile(w.opts.Path, w.opts.MaxSizeBytes); err != nil {
		return err
	}
	return w.enforceTotal()
}

func (w *Writer) enforceTotal() error {
	total, err := fileSize(w.opts.Path)
	if err != nil {
		return err
	}
	backups, err := w.backups()
	if err != nil {
		return err
	}
	managed := backups[:0]
	for _, backup := range backups {
		if backup.index > w.opts.MaxFiles || !backup.canonical {
			if err := removeIfExists(backup.path); err != nil {
				return err
			}
			continue
		}
		size, err := fileSize(backup.path)
		if err != nil {
			return err
		}
		total += size
		managed = append(managed, backup)
	}
	for i := len(managed) - 1; total > w.opts.MaxTotalSizeBytes && i >= 0; i-- {
		path := managed[i].path
		size, err := fileSize(path)
		if err != nil {
			return err
		}
		if err := removeIfExists(path); err != nil {
			return err
		}
		total -= size
	}
	if total > w.opts.MaxTotalSizeBytes {
		return fmt.Errorf("active log size %d exceeds total limit %d", total, w.opts.MaxTotalSizeBytes)
	}
	return nil
}

func (w *Writer) backups() ([]backupFile, error) {
	directory := filepath.Dir(w.opts.Path)
	entries, err := os.ReadDir(directory)
	if err != nil {
		return nil, fmt.Errorf("list rotated logs: %w", err)
	}
	prefix := filepath.Base(w.opts.Path) + "."
	backups := make([]backupFile, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasPrefix(entry.Name(), prefix) {
			continue
		}
		suffix := strings.TrimPrefix(entry.Name(), prefix)
		index, err := strconv.Atoi(suffix)
		if err == nil && index > 0 {
			backups = append(backups, backupFile{
				index:     index,
				path:      filepath.Join(directory, entry.Name()),
				canonical: suffix == strconv.Itoa(index),
			})
		}
	}
	sort.Slice(backups, func(i, j int) bool {
		if backups[i].index == backups[j].index {
			return backups[i].path < backups[j].path
		}
		return backups[i].index < backups[j].index
	})
	return backups, nil
}

func (w *Writer) backupPath(index int) string {
	return fmt.Sprintf("%s.%d", w.opts.Path, index)
}

func trimFile(path string, max int64) error {
	info, err := os.Stat(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("stat managed log %s: %w", path, err)
	}
	if info.Size() <= max {
		return nil
	}
	if err := os.Truncate(path, max); err != nil {
		return fmt.Errorf("trim managed log %s: %w", path, err)
	}
	return nil
}

func fileSize(path string) (int64, error) {
	info, err := os.Stat(path)
	if os.IsNotExist(err) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("stat managed log %s: %w", path, err)
	}
	return info.Size(), nil
}

func removeIfExists(path string) error {
	err := os.Remove(path)
	if err == nil || os.IsNotExist(err) {
		return nil
	}
	return fmt.Errorf("remove managed log %s: %w", path, err)
}

func renameIfExists(from, to string) error {
	err := os.Rename(from, to)
	if err == nil || os.IsNotExist(err) {
		return nil
	}
	return fmt.Errorf("rotate log %s to %s: %w", from, to, err)
}
