//go:build !linux

package collector

import "fmt"

type platformStatFS struct{}

func (platformStatFS) StatFS(path string) (StatFSInfo, error) {
	return StatFSInfo{}, fmt.Errorf("statfs %q: %w", path, ErrUnsupportedPlatform)
}
