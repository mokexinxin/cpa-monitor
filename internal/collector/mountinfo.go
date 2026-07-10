package collector

import (
	"bufio"
	"fmt"
	"io"
	"sort"
	"strings"
)

var skippedFilesystemTypes = map[string]struct{}{
	"proc": {}, "sysfs": {}, "tmpfs": {}, "devtmpfs": {},
	"devpts": {}, "cgroup": {}, "cgroup2": {}, "overlay": {},
	"squashfs": {}, "securityfs": {}, "debugfs": {}, "tracefs": {},
	"fusectl": {}, "mqueue": {}, "pstore": {}, "autofs": {},
	"binfmt_misc": {}, "bpf": {}, "configfs": {}, "hugetlbfs": {},
	"nsfs": {},
}

// ShouldSkipFilesystem performs an exact match against the approved pseudo
// filesystem list. Similar names (for example fuse.sshfs) remain monitored.
func ShouldSkipFilesystem(filesystemType string) bool {
	_, skip := skippedFilesystemTypes[filesystemType]
	return skip
}

// ParseMountInfo parses Linux /proc/self/mountinfo, filters pseudo filesystems,
// de-duplicates mount points, and returns a stable mount-point-sorted result.
func ParseMountInfo(r io.Reader) ([]Mount, error) {
	if r == nil {
		return nil, fmt.Errorf("line 0: read mountinfo: nil reader")
	}

	byMountPoint := make(map[string]Mount)
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 4096), 4*1024*1024)
	lineNumber := 0
	for scanner.Scan() {
		lineNumber++
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		separator := -1
		for i, field := range fields {
			if field == "-" {
				separator = i
				break
			}
		}
		if separator < 6 || separator+2 >= len(fields) {
			return nil, fmt.Errorf("line %d: invalid mountinfo separator or fields", lineNumber)
		}
		mountPoint, err := decodeMountInfoField(fields[4])
		if err != nil {
			return nil, fmt.Errorf("line %d: decode mount point: %w", lineNumber, err)
		}
		filesystemType := fields[separator+1]
		if mountPoint == "" {
			return nil, fmt.Errorf("line %d: empty mount point", lineNumber)
		}
		if filesystemType == "" {
			return nil, fmt.Errorf("line %d: empty filesystem type", lineNumber)
		}
		if ShouldSkipFilesystem(filesystemType) {
			continue
		}
		if _, duplicate := byMountPoint[mountPoint]; !duplicate {
			byMountPoint[mountPoint] = Mount{
				MountPoint:     mountPoint,
				FilesystemType: filesystemType,
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("line %d: read mountinfo: %w", lineNumber+1, err)
	}

	mounts := make([]Mount, 0, len(byMountPoint))
	for _, mount := range byMountPoint {
		mounts = append(mounts, mount)
	}
	sort.Slice(mounts, func(i, j int) bool {
		if mounts[i].MountPoint == mounts[j].MountPoint {
			return mounts[i].FilesystemType < mounts[j].FilesystemType
		}
		return mounts[i].MountPoint < mounts[j].MountPoint
	})
	return mounts, nil
}

func decodeMountInfoField(value string) (string, error) {
	var decoded strings.Builder
	decoded.Grow(len(value))
	for i := 0; i < len(value); {
		if value[i] != '\\' {
			decoded.WriteByte(value[i])
			i++
			continue
		}
		if i+4 > len(value) {
			return "", fmt.Errorf("truncated escape at byte %d", i)
		}
		escape := value[i : i+4]
		switch escape {
		case `\040`:
			decoded.WriteByte(' ')
		case `\011`:
			decoded.WriteByte('\t')
		case `\012`:
			decoded.WriteByte('\n')
		case `\134`:
			decoded.WriteByte('\\')
		default:
			return "", fmt.Errorf("unsupported escape %q at byte %d", escape, i)
		}
		i += 4
	}
	return decoded.String(), nil
}
