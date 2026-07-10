package collector_test

import (
	"errors"
	"math"
	"reflect"
	"strings"
	"testing"

	"github.com/mokexinxin/cpa-monitor/internal/collector"
)

func TestParseMountInfo(t *testing.T) {
	t.Parallel()

	got, err := collector.ParseMountInfo(strings.NewReader(readFixture(t, "testdata/mountinfo-valid.txt")))
	if err != nil {
		t.Fatalf("ParseMountInfo() error = %v", err)
	}
	want := []collector.Mount{
		{MountPoint: "/", FilesystemType: "ext4"},
		{MountPoint: "/data disk", FilesystemType: "xfs"},
		{MountPoint: "/remote", FilesystemType: "fuse.sshfs"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ParseMountInfo() = %#v, want %#v", got, want)
	}
}

func TestParseMountInfoDecodesKernelEscapes(t *testing.T) {
	t.Parallel()
	input := `1 0 8:1 / /space\040tab\011newline\012slash\134end rw - ext4 /dev/root rw` + "\n"
	got, err := collector.ParseMountInfo(strings.NewReader(input))
	if err != nil {
		t.Fatalf("ParseMountInfo() error = %v", err)
	}
	want := "/space tab\tnewline\nslash\\end"
	if len(got) != 1 || got[0].MountPoint != want {
		t.Fatalf("ParseMountInfo() = %#v, want mount point %q", got, want)
	}
}

func TestParseMountInfoRejectsMalformedRows(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		input string
	}{
		{name: "missing separator", input: readFixture(t, "testdata/mountinfo-invalid.txt")},
		{name: "unknown escape", input: "1 0 8:1 / /bad\\777 rw - ext4 /dev/root rw\n"},
		{name: "truncated escape", input: "1 0 8:1 / /bad\\1 rw - ext4 /dev/root rw\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := collector.ParseMountInfo(strings.NewReader(tt.input))
			if err == nil || !strings.Contains(err.Error(), "line 1") {
				t.Fatalf("ParseMountInfo() error = %v, want line 1 error", err)
			}
		})
	}
}

func TestShouldSkipFilesystemExactApprovedList(t *testing.T) {
	t.Parallel()
	skipped := []string{
		"proc", "sysfs", "tmpfs", "devtmpfs", "devpts", "cgroup",
		"cgroup2", "overlay", "squashfs", "securityfs", "debugfs",
		"tracefs", "fusectl", "mqueue", "pstore", "autofs",
		"binfmt_misc", "bpf", "configfs", "hugetlbfs", "nsfs",
	}
	for _, filesystemType := range skipped {
		if !collector.ShouldSkipFilesystem(filesystemType) {
			t.Errorf("ShouldSkipFilesystem(%q) = false, want true", filesystemType)
		}
	}
	for _, filesystemType := range []string{"ext4", "xfs", "nfs", "fuse.sshfs", "Proc", "overlay2"} {
		if collector.ShouldSkipFilesystem(filesystemType) {
			t.Errorf("ShouldSkipFilesystem(%q) = true, want false", filesystemType)
		}
	}
}

func TestCollectDisksPreservesSuccessOnPartialFailure(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("statfs unavailable")
	mounts := []collector.Mount{
		{MountPoint: "/broken", FilesystemType: "xfs"},
		{MountPoint: "/", FilesystemType: "ext4"},
		{MountPoint: "/", FilesystemType: "duplicate"},
		{MountPoint: "/pseudo", FilesystemType: "tmpfs"},
	}
	batch := collector.CollectDisks(mounts, collector.StatFSFunc(func(path string) (collector.StatFSInfo, error) {
		switch path {
		case "/":
			return collector.StatFSInfo{Blocks: 100, BlocksFree: 25, BlockSize: 4096}, nil
		case "/broken":
			return collector.StatFSInfo{}, sentinel
		default:
			t.Fatalf("unexpected statfs path %q", path)
			return collector.StatFSInfo{}, nil
		}
	}))
	if batch.Complete {
		t.Fatal("CollectDisks() Complete = true, want false")
	}
	if len(batch.Disks) != 1 {
		t.Fatalf("CollectDisks() Disks = %#v, want one successful disk", batch.Disks)
	}
	got := batch.Disks[0]
	if got.MountPoint != "/" || got.FilesystemType != "ext4" || got.TotalBytes != 409600 || got.UsedBytes != 307200 || math.Abs(got.UsedPercent-75) > 1e-9 {
		t.Errorf("CollectDisks() successful disk = %+v, want 75%% ext4 usage", got)
	}
	if len(batch.Errors) != 1 || batch.Errors[0].MountPoint != "/broken" || !errors.Is(batch.Errors[0], sentinel) {
		t.Errorf("CollectDisks() Errors = %#v, want /broken sentinel", batch.Errors)
	}
	if !errors.Is(batch.Err(), sentinel) {
		t.Errorf("DiskBatch.Err() = %v, want sentinel", batch.Err())
	}
}

func TestCollectDisksRejectsInvalidStats(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		info collector.StatFSInfo
		part string
	}{
		{name: "zero blocks", info: collector.StatFSInfo{Blocks: 0, BlockSize: 4096}, part: "zero blocks"},
		{name: "zero block size", info: collector.StatFSInfo{Blocks: 1}, part: "zero block size"},
		{name: "free exceeds total", info: collector.StatFSInfo{Blocks: 1, BlocksFree: 2, BlockSize: 1}, part: "exceed"},
		{name: "total overflow", info: collector.StatFSInfo{Blocks: math.MaxUint64, BlockSize: 2}, part: "overflow"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			batch := collector.CollectDisks(
				[]collector.Mount{{MountPoint: "/", FilesystemType: "ext4"}},
				collector.StatFSFunc(func(string) (collector.StatFSInfo, error) { return tt.info, nil }),
			)
			if batch.Complete || len(batch.Disks) != 0 || len(batch.Errors) != 1 {
				t.Fatalf("CollectDisks() = %+v, want one error and no disks", batch)
			}
			if !strings.Contains(batch.Errors[0].Error(), tt.part) {
				t.Errorf("CollectDisks() error = %q, want %q", batch.Errors[0], tt.part)
			}
		})
	}
}
