package collector

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
)

// ErrUnsupportedPlatform is returned by the host collector on non-Linux
// platforms. Pure parsers and injected ProcCollector instances remain usable
// on every platform.
var ErrUnsupportedPlatform = errors.New("collector: unsupported platform")

// HostCollector collects one independently trustworthy host fact at a time.
// A failed method represents an unknown batch and must not be interpreted as a
// healthy result.
type HostCollector interface {
	Memory(context.Context) (MemoryUsage, error)
	Disks(context.Context) (DiskBatch, error)
	TCP(context.Context, int) (TCPUsage, error)
}

type MemoryUsage struct {
	TotalBytes     uint64
	AvailableBytes uint64
	UsedBytes      uint64
	UsedPercent    float64
}

type Mount struct {
	MountPoint     string
	FilesystemType string
}

type DiskUsage struct {
	MountPoint     string
	FilesystemType string
	TotalBytes     uint64
	UsedBytes      uint64
	UsedPercent    float64
}

// DiskError identifies the mount whose statfs result was unavailable or
// invalid. It deliberately retains the underlying error for errors.Is/As.
type DiskError struct {
	MountPoint string
	Err        error
}

func (e DiskError) Error() string {
	if e.MountPoint == "" {
		return fmt.Sprintf("collect disk: %v", e.Err)
	}
	return fmt.Sprintf("collect disk %q: %v", e.MountPoint, e.Err)
}

func (e DiskError) Unwrap() error { return e.Err }

// DiskBatch may contain useful disk facts even when Complete is false. Callers
// should evaluate the returned Disks, but must not recover missing disk alert
// keys from an incomplete batch.
type DiskBatch struct {
	Disks    []DiskUsage
	Complete bool
	Errors   []DiskError
}

// Err joins all mount-level errors in the batch.
func (b DiskBatch) Err() error {
	if len(b.Errors) == 0 {
		return nil
	}
	errs := make([]error, len(b.Errors))
	for i := range b.Errors {
		errs[i] = b.Errors[i]
	}
	return errors.Join(errs...)
}

type TCPUsage struct {
	TotalConnections       int
	ServicePortConnections int
}

// StatFSInfo is the subset of statfs data used by the collector. BlocksFree
// intentionally maps to f_bfree (not f_bavail), matching the approved usage
// formula: (Blocks-BlocksFree)/Blocks.
type StatFSInfo struct {
	Blocks     uint64
	BlocksFree uint64
	BlockSize  uint64
}

// StatFS is injectable so disk collection can be tested without invoking the
// host kernel or depending on the build platform.
type StatFS interface {
	StatFS(path string) (StatFSInfo, error)
}

type StatFSFunc func(path string) (StatFSInfo, error)

func (f StatFSFunc) StatFS(path string) (StatFSInfo, error) { return f(path) }

// NewProcCollector creates a /proc collector from injected dependencies. The
// fs.FS must expose proc files at paths such as "proc/meminfo".
func NewProcCollector(files fs.FS, statFS StatFS) HostCollector {
	return &procCollector{
		files:         files,
		statFS:        statFS,
		statFSTimeout: defaultStatFSTimeout,
		statCalls:     make(map[string]*statCall),
	}
}
