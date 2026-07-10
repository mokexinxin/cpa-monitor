package collector_test

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/mokexinxin/cpa-monitor/internal/collector"
)

func TestParseTCP(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		input       string
		servicePort int
		want        collector.TCPUsage
	}{
		{
			name:        "IPv4 counts every state",
			input:       readFixture(t, "testdata/tcp4.txt"),
			servicePort: 8317,
			want:        collector.TCPUsage{TotalConnections: 3, ServicePortConnections: 1},
		},
		{
			name:        "IPv6 counts every state",
			input:       readFixture(t, "testdata/tcp6.txt"),
			servicePort: 8317,
			want:        collector.TCPUsage{TotalConnections: 2, ServicePortConnections: 1},
		},
		{
			name:        "port zero",
			input:       readFixture(t, "testdata/tcp4.txt"),
			servicePort: 0,
			want:        collector.TCPUsage{TotalConnections: 3, ServicePortConnections: 1},
		},
		{
			name:        "port 65535",
			input:       readFixture(t, "testdata/tcp4.txt"),
			servicePort: 65535,
			want:        collector.TCPUsage{TotalConnections: 3, ServicePortConnections: 1},
		},
		{
			name:        "empty table",
			input:       "  sl  local_address rem_address st\n",
			servicePort: 8317,
			want:        collector.TCPUsage{},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := collector.ParseTCP(strings.NewReader(tt.input), tt.servicePort)
			if err != nil {
				t.Fatalf("ParseTCP() error = %v", err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("ParseTCP() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestParseTCPRejectsMalformedRowsWithLineNumber(t *testing.T) {
	t.Parallel()
	tests := []string{
		"bad row",
		"0 0100007F:207D 0:0 01",
		"0: 0100007F 0:0 01",
		"0: NOTHEX:207D 0:0 01",
		"0: 0100007F:10000 0:0 01",
		"0: 0100007F:207D 0:0 XX",
	}
	for _, row := range tests {
		row := row
		t.Run(row, func(t *testing.T) {
			t.Parallel()
			input := "sl local_address rem_address st\n" + row + "\n"
			_, err := collector.ParseTCP(strings.NewReader(input), 8317)
			if err == nil || !strings.Contains(err.Error(), "line 2") {
				t.Fatalf("ParseTCP() error = %v, want line 2 error", err)
			}
		})
	}
}

func TestParseTCPRejectsInvalidServicePort(t *testing.T) {
	t.Parallel()
	for _, port := range []int{-1, 65536} {
		if _, err := collector.ParseTCP(strings.NewReader(""), port); err == nil || !strings.Contains(err.Error(), "line 0") {
			t.Errorf("ParseTCP(servicePort=%d) error = %v, want line 0 error", port, err)
		}
	}
}

func TestInjectedProcCollector(t *testing.T) {
	t.Parallel()
	files := fstest.MapFS{
		"proc/meminfo":        {Data: []byte("MemAvailable: 25 kB\nMemTotal: 100 kB\n")},
		"proc/self/mountinfo": {Data: []byte("1 0 8:1 / / rw - ext4 /dev/root rw\n2 1 8:2 / /data rw - xfs /dev/data rw\n")},
		"proc/net/tcp":        {Data: []byte(readFixture(t, "testdata/tcp4.txt"))},
		"proc/net/tcp6":       {Data: []byte(readFixture(t, "testdata/tcp6.txt"))},
	}
	host := collector.NewProcCollector(files, collector.StatFSFunc(func(path string) (collector.StatFSInfo, error) {
		if path != "/" && path != "/data" {
			t.Fatalf("unexpected statfs path %q", path)
		}
		return collector.StatFSInfo{Blocks: 10, BlocksFree: 2, BlockSize: 1024}, nil
	}))

	memory, err := host.Memory(context.Background())
	if err != nil || memory.UsedPercent != 75 {
		t.Errorf("Memory() = %+v, %v, want 75%%", memory, err)
	}
	disks, err := host.Disks(context.Background())
	if err != nil || !disks.Complete || len(disks.Disks) != 2 {
		t.Errorf("Disks() = %+v, %v, want two complete disks", disks, err)
	}
	tcp, err := host.TCP(context.Background(), 8317)
	if err != nil {
		t.Fatalf("TCP() error = %v", err)
	}
	if want := (collector.TCPUsage{TotalConnections: 5, ServicePortConnections: 2}); tcp != want {
		t.Errorf("TCP() = %+v, want %+v", tcp, want)
	}
}

func TestInjectedProcCollectorDiskPartialError(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("no access")
	files := fstest.MapFS{
		"proc/self/mountinfo": {Data: []byte("1 0 8:1 / / rw - ext4 /dev/root rw\n2 1 8:2 / /data rw - xfs /dev/data rw\n")},
	}
	host := collector.NewProcCollector(files, collector.StatFSFunc(func(path string) (collector.StatFSInfo, error) {
		if path == "/data" {
			return collector.StatFSInfo{}, sentinel
		}
		return collector.StatFSInfo{Blocks: 10, BlocksFree: 2, BlockSize: 1024}, nil
	}))
	batch, err := host.Disks(context.Background())
	if !errors.Is(err, sentinel) {
		t.Errorf("Disks() error = %v, want sentinel", err)
	}
	if batch.Complete || len(batch.Disks) != 1 || batch.Disks[0].MountPoint != "/" {
		t.Errorf("Disks() batch = %+v, want successful root and incomplete", batch)
	}
}

func TestInjectedProcCollectorTCPFailureIsUnknown(t *testing.T) {
	t.Parallel()
	files := fstest.MapFS{
		"proc/net/tcp": {Data: []byte(readFixture(t, "testdata/tcp4.txt"))},
	}
	host := collector.NewProcCollector(files, nil)
	got, err := host.TCP(context.Background(), 8317)
	if err == nil {
		t.Fatal("TCP() error = nil, want missing tcp6 error")
	}
	if got != (collector.TCPUsage{}) {
		t.Errorf("TCP() = %+v, want zero unknown result", got)
	}
}

func TestInjectedProcCollectorHonorsCanceledContext(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	host := collector.NewProcCollector(fstest.MapFS{}, nil)
	if _, err := host.Memory(ctx); !errors.Is(err, context.Canceled) {
		t.Errorf("Memory() error = %v, want context.Canceled", err)
	}
	if batch, err := host.Disks(ctx); !errors.Is(err, context.Canceled) || batch.Complete {
		t.Errorf("Disks() = %+v, %v, want incomplete context.Canceled", batch, err)
	}
	if _, err := host.TCP(ctx, 8317); !errors.Is(err, context.Canceled) {
		t.Errorf("TCP() error = %v, want context.Canceled", err)
	}
}
