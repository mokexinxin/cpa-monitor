//go:build !linux

package collector_test

import (
	"context"
	"errors"
	"testing"

	"github.com/mokexinxin/cpa-monitor/internal/collector"
)

func TestHostCollectorUnsupportedPlatform(t *testing.T) {
	t.Parallel()
	host := collector.NewHostCollector()
	if _, err := host.Memory(context.Background()); !errors.Is(err, collector.ErrUnsupportedPlatform) {
		t.Errorf("Memory() error = %v, want ErrUnsupportedPlatform", err)
	}
	if batch, err := host.Disks(context.Background()); !errors.Is(err, collector.ErrUnsupportedPlatform) || batch.Complete {
		t.Errorf("Disks() = %+v, %v, want incomplete ErrUnsupportedPlatform", batch, err)
	}
	if _, err := host.TCP(context.Background(), 8317); !errors.Is(err, collector.ErrUnsupportedPlatform) {
		t.Errorf("TCP() error = %v, want ErrUnsupportedPlatform", err)
	}
}
