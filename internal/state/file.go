package state

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

const schemaVersion = 1

type Record struct {
	Key         string            `json:"key"`
	Scope       string            `json:"scope"`
	Summary     string            `json:"summary,omitempty"`
	Current     string            `json:"current,omitempty"`
	Threshold   string            `json:"threshold,omitempty"`
	Details     map[string]string `json:"details,omitempty"`
	ActivatedAt time.Time         `json:"activated_at"`
}

type document struct {
	Version int      `json:"version"`
	Active  []Record `json:"active"`
}

type File struct {
	mu      sync.RWMutex
	saveMu  sync.Mutex
	path    string
	records map[string]Record
	save    func(string, []byte) error
}

func New(path string) *File {
	return &File{
		path:    path,
		records: make(map[string]Record),
		save:    atomicWrite,
	}
}

// Open returns an empty, usable File together with an error when persisted
// state is unreadable. Callers can log the error and keep monitoring.
func Open(path string) (*File, error) {
	store := New(path)
	file, err := os.Open(path)
	if os.IsNotExist(err) {
		return store, nil
	}
	if err != nil {
		return store, fmt.Errorf("read alert state: %w", err)
	}
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		return store, fmt.Errorf("secure alert state: %w", err)
	}
	data, err := io.ReadAll(file)
	if err != nil {
		_ = file.Close()
		return store, fmt.Errorf("read alert state: %w", err)
	}
	if err := file.Close(); err != nil {
		return store, fmt.Errorf("close alert state after read: %w", err)
	}

	var doc document
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&doc); err != nil {
		return store, fmt.Errorf("decode alert state: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return store, errors.New("decode alert state: multiple JSON values are not allowed")
		}
		return store, fmt.Errorf("decode alert state trailing data: %w", err)
	}
	if doc.Version != schemaVersion {
		return store, fmt.Errorf("unsupported alert state version %d", doc.Version)
	}
	for _, record := range doc.Active {
		if err := validateRecord(record); err != nil {
			return New(path), fmt.Errorf("invalid alert state record: %w", err)
		}
		if _, exists := store.records[record.Key]; exists {
			return New(path), fmt.Errorf("duplicate alert state key %q", record.Key)
		}
		store.records[record.Key] = cloneRecord(record)
	}
	return store, nil
}

func (f *File) Path() string {
	return f.path
}

func (f *File) Get(key string) (Record, bool) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	record, ok := f.records[key]
	return cloneRecord(record), ok
}

func (f *File) Records() map[string]Record {
	f.mu.RLock()
	defer f.mu.RUnlock()
	result := make(map[string]Record, len(f.records))
	for key, record := range f.records {
		result[key] = cloneRecord(record)
	}
	return result
}

func (f *File) ByScope(scope string) []Record {
	f.mu.RLock()
	defer f.mu.RUnlock()
	result := make([]Record, 0)
	for _, record := range f.records {
		if record.Scope == scope {
			result = append(result, cloneRecord(record))
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Key < result[j].Key })
	return result
}

func (f *File) Put(record Record) error {
	if err := validateRecord(record); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.records[record.Key] = cloneRecord(record)
	return nil
}

func (f *File) Delete(key string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.records[key]; !ok {
		return false
	}
	delete(f.records, key)
	return true
}

func (f *File) Save() error {
	f.saveMu.Lock()
	defer f.saveMu.Unlock()

	f.mu.RLock()
	records := make([]Record, 0, len(f.records))
	for _, record := range f.records {
		records = append(records, cloneRecord(record))
	}
	f.mu.RUnlock()

	sort.Slice(records, func(i, j int) bool { return records[i].Key < records[j].Key })
	data, err := json.MarshalIndent(document{Version: schemaVersion, Active: records}, "", "  ")
	if err != nil {
		return fmt.Errorf("encode alert state: %w", err)
	}
	data = append(data, '\n')
	if err := f.save(f.path, data); err != nil {
		return fmt.Errorf("save alert state: %w", err)
	}
	return nil
}

func validateRecord(record Record) error {
	if record.Key == "" {
		return errors.New("alert state key is required")
	}
	if record.Scope == "" {
		return errors.New("alert state scope is required")
	}
	return nil
}

func cloneRecord(record Record) Record {
	if record.Details != nil {
		original := record.Details
		record.Details = make(map[string]string, len(original))
		for key, value := range original {
			record.Details[key] = value
		}
	}
	return record
}

func atomicWrite(path string, data []byte) (err error) {
	if path == "" {
		return errors.New("alert state path is required")
	}
	dir := filepath.Dir(path)
	_, statErr := os.Stat(dir)
	createdDirectory := os.IsNotExist(statErr)
	if statErr != nil && !createdDirectory {
		return fmt.Errorf("stat alert state directory: %w", statErr)
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("create alert state directory: %w", err)
	}
	if createdDirectory {
		if err := os.Chmod(dir, 0o750); err != nil {
			return fmt.Errorf("secure new alert state directory: %w", err)
		}
	}

	temp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("create temporary alert state: %w", err)
	}
	tempPath := temp.Name()
	defer func() {
		_ = temp.Close()
		_ = os.Remove(tempPath)
	}()

	if err := temp.Chmod(0o600); err != nil {
		return fmt.Errorf("secure temporary alert state: %w", err)
	}
	if _, err := temp.Write(data); err != nil {
		return fmt.Errorf("write temporary alert state: %w", err)
	}
	if err := temp.Sync(); err != nil {
		return fmt.Errorf("sync temporary alert state: %w", err)
	}
	if err := temp.Close(); err != nil {
		return fmt.Errorf("close temporary alert state: %w", err)
	}
	if err := os.Rename(tempPath, path); err != nil {
		return fmt.Errorf("replace alert state: %w", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return fmt.Errorf("secure alert state: %w", err)
	}

	directory, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("open alert state directory: %w", err)
	}
	defer directory.Close()
	if err := directory.Sync(); err != nil {
		return fmt.Errorf("sync alert state directory: %w", err)
	}
	return nil
}
