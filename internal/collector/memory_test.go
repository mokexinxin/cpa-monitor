package collector_test

import (
	"math"
	"os"
	"strings"
	"testing"

	"github.com/mokexinxin/cpa-monitor/internal/collector"
)

func TestParseMemInfo(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		input         string
		want          collector.MemoryUsage
		wantErr       bool
		errorContains []string
	}{
		{
			name:  "field order extra fields and whitespace",
			input: readFixture(t, "testdata/meminfo-valid.txt"),
			want: collector.MemoryUsage{
				TotalBytes:     16 * 1024 * 1024,
				AvailableBytes: 4 * 1024 * 1024,
				UsedBytes:      12 * 1024 * 1024,
				UsedPercent:    75,
			},
		},
		{
			name:          "missing total",
			input:         "MemAvailable: 1 kB\n",
			wantErr:       true,
			errorContains: []string{"line", "MemTotal"},
		},
		{
			name:          "missing available",
			input:         "MemTotal: 1 kB\n",
			wantErr:       true,
			errorContains: []string{"line", "MemAvailable"},
		},
		{
			name:          "wrong unit",
			input:         readFixture(t, "testdata/meminfo-invalid.txt"),
			wantErr:       true,
			errorContains: []string{"line 1", "MemTotal", "MB"},
		},
		{
			name:          "zero total",
			input:         "MemTotal: 0 kB\nMemAvailable: 0 kB\n",
			wantErr:       true,
			errorContains: []string{"line 1", "MemTotal"},
		},
		{
			name:          "available exceeds total",
			input:         "MemTotal: 1 kB\nMemAvailable: 2 kB\n",
			wantErr:       true,
			errorContains: []string{"line 2", "MemAvailable", "MemTotal"},
		},
		{
			name:          "byte conversion overflow",
			input:         "MemTotal: 18014398509481984 kB\nMemAvailable: 1 kB\n",
			wantErr:       true,
			errorContains: []string{"line 1", "MemTotal", "overflow"},
		},
		{
			name:          "numeric overflow",
			input:         "MemTotal: 18446744073709551616 kB\nMemAvailable: 1 kB\n",
			wantErr:       true,
			errorContains: []string{"line 1", "MemTotal"},
		},
		{
			name:          "duplicate field",
			input:         "MemTotal: 2 kB\nMemTotal: 2 kB\nMemAvailable: 1 kB\n",
			wantErr:       true,
			errorContains: []string{"line 2", "MemTotal", "duplicate"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := collector.ParseMemInfo(strings.NewReader(tt.input))
			if tt.wantErr {
				if err == nil {
					t.Fatalf("ParseMemInfo() error = nil, want error")
				}
				for _, part := range tt.errorContains {
					if !strings.Contains(err.Error(), part) {
						t.Errorf("ParseMemInfo() error = %q, want it to contain %q", err, part)
					}
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseMemInfo() error = %v", err)
			}
			if got.TotalBytes != tt.want.TotalBytes || got.AvailableBytes != tt.want.AvailableBytes || got.UsedBytes != tt.want.UsedBytes {
				t.Errorf("ParseMemInfo() bytes = %+v, want %+v", got, tt.want)
			}
			if math.Abs(got.UsedPercent-tt.want.UsedPercent) > 1e-9 {
				t.Errorf("ParseMemInfo() UsedPercent = %v, want %v", got.UsedPercent, tt.want.UsedPercent)
			}
		})
	}
}

func TestParseMemInfoNilReader(t *testing.T) {
	t.Parallel()
	if _, err := collector.ParseMemInfo(nil); err == nil || !strings.Contains(err.Error(), "line") {
		t.Fatalf("ParseMemInfo(nil) error = %v, want line-numbered error", err)
	}
}

func readFixture(t *testing.T, path string) string {
	t.Helper()
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read fixture %s: %v", path, err)
	}
	return string(contents)
}
