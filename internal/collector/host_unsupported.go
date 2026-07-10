//go:build !linux

package collector

import (
	"context"
	"fmt"
	"runtime"
)

type unsupportedCollector struct{}

// NewHostCollector builds an explicit unsupported collector on non-Linux
// systems, while NewProcCollector remains available for portable tests.
func NewHostCollector() HostCollector { return unsupportedCollector{} }

func (unsupportedCollector) Memory(context.Context) (MemoryUsage, error) {
	return MemoryUsage{}, unsupportedError("memory")
}

func (unsupportedCollector) Disks(context.Context) (DiskBatch, error) {
	return DiskBatch{Complete: false}, unsupportedError("disk")
}

func (unsupportedCollector) TCP(context.Context, int) (TCPUsage, error) {
	return TCPUsage{}, unsupportedError("TCP")
}

func unsupportedError(check string) error {
	return fmt.Errorf("collect %s on %s: %w", check, runtime.GOOS, ErrUnsupportedPlatform)
}
