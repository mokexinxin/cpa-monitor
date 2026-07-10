//go:build linux

package collector

import "os"

// NewHostCollector returns the production Linux /proc and statfs collector.
func NewHostCollector() HostCollector {
	return NewProcCollector(os.DirFS("/"), platformStatFS{})
}
