//go:build linux

package collector

import (
	"fmt"

	"golang.org/x/sys/unix"
)

type platformStatFS struct{}

func (platformStatFS) StatFS(path string) (StatFSInfo, error) {
	var stats unix.Statfs_t
	if err := unix.Statfs(path, &stats); err != nil {
		return StatFSInfo{}, fmt.Errorf("statfs %q: %w", path, err)
	}
	if stats.Bsize <= 0 {
		return StatFSInfo{}, fmt.Errorf("statfs %q: invalid block size %d", path, stats.Bsize)
	}
	return StatFSInfo{
		Blocks:     stats.Blocks,
		BlocksFree: stats.Bfree,
		BlockSize:  uint64(stats.Bsize),
	}, nil
}
