package collector

import (
	"errors"
	"fmt"
	"math/bits"
	"sort"
)

// CollectDisks calls the injected statfs implementation for every real unique
// mount. A failure does not discard successful mounts.
func CollectDisks(mounts []Mount, statFS StatFS) DiskBatch {
	normalized := normalizeMounts(mounts)
	batch := DiskBatch{
		Disks:    make([]DiskUsage, 0, len(normalized)),
		Complete: true,
	}
	if statFS == nil {
		batch.Complete = false
		batch.Errors = append(batch.Errors, DiskError{Err: errors.New("nil statfs implementation")})
		return batch
	}

	for _, mount := range normalized {
		info, err := statFS.StatFS(mount.MountPoint)
		if err == nil {
			var usage DiskUsage
			usage, err = diskUsage(mount, info)
			if err == nil {
				batch.Disks = append(batch.Disks, usage)
				continue
			}
		}
		batch.Complete = false
		batch.Errors = append(batch.Errors, DiskError{
			MountPoint: mount.MountPoint,
			Err:        err,
		})
	}
	return batch
}

func normalizeMounts(mounts []Mount) []Mount {
	byMountPoint := make(map[string]Mount, len(mounts))
	for _, mount := range mounts {
		if mount.MountPoint == "" || mount.FilesystemType == "" || ShouldSkipFilesystem(mount.FilesystemType) {
			continue
		}
		if _, exists := byMountPoint[mount.MountPoint]; !exists {
			byMountPoint[mount.MountPoint] = mount
		}
	}
	normalized := make([]Mount, 0, len(byMountPoint))
	for _, mount := range byMountPoint {
		normalized = append(normalized, mount)
	}
	sort.Slice(normalized, func(i, j int) bool {
		return normalized[i].MountPoint < normalized[j].MountPoint
	})
	return normalized
}

func diskUsage(mount Mount, info StatFSInfo) (DiskUsage, error) {
	if info.Blocks == 0 {
		return DiskUsage{}, errors.New("statfs reports zero blocks")
	}
	if info.BlockSize == 0 {
		return DiskUsage{}, errors.New("statfs reports zero block size")
	}
	if info.BlocksFree > info.Blocks {
		return DiskUsage{}, fmt.Errorf("statfs free blocks %d exceed total blocks %d", info.BlocksFree, info.Blocks)
	}
	totalHigh, totalBytes := bits.Mul64(info.Blocks, info.BlockSize)
	if totalHigh != 0 {
		return DiskUsage{}, errors.New("statfs total bytes overflow uint64")
	}
	usedBlocks := info.Blocks - info.BlocksFree
	usedHigh, usedBytes := bits.Mul64(usedBlocks, info.BlockSize)
	if usedHigh != 0 {
		return DiskUsage{}, errors.New("statfs used bytes overflow uint64")
	}
	return DiskUsage{
		MountPoint:     mount.MountPoint,
		FilesystemType: mount.FilesystemType,
		TotalBytes:     totalBytes,
		UsedBytes:      usedBytes,
		UsedPercent:    float64(usedBlocks) / float64(info.Blocks) * 100,
	}, nil
}
