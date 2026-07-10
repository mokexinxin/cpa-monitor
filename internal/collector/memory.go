package collector

import (
	"bufio"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"
)

// ParseMemInfo parses MemTotal and MemAvailable from Linux /proc/meminfo.
func ParseMemInfo(r io.Reader) (MemoryUsage, error) {
	if r == nil {
		return MemoryUsage{}, fmt.Errorf("line 0: read meminfo: nil reader")
	}

	type valueAtLine struct {
		bytes uint64
		line  int
	}
	values := make(map[string]valueAtLine, 2)
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 1024), 1024*1024)
	lineNumber := 0
	for scanner.Scan() {
		lineNumber++
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		name, rest, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		name = strings.TrimSpace(name)
		if name != "MemTotal" && name != "MemAvailable" {
			continue
		}
		if _, duplicate := values[name]; duplicate {
			return MemoryUsage{}, fmt.Errorf("line %d: duplicate %s", lineNumber, name)
		}
		fields := strings.Fields(rest)
		if len(fields) != 2 {
			return MemoryUsage{}, fmt.Errorf("line %d: %s must contain a value and kB unit", lineNumber, name)
		}
		if fields[1] != "kB" {
			return MemoryUsage{}, fmt.Errorf("line %d: %s has unsupported unit %q", lineNumber, name, fields[1])
		}
		kilobytes, err := strconv.ParseUint(fields[0], 10, 64)
		if err != nil {
			return MemoryUsage{}, fmt.Errorf("line %d: parse %s: %w", lineNumber, name, err)
		}
		if kilobytes > math.MaxUint64/1024 {
			return MemoryUsage{}, fmt.Errorf("line %d: %s overflows bytes", lineNumber, name)
		}
		values[name] = valueAtLine{bytes: kilobytes * 1024, line: lineNumber}
	}
	if err := scanner.Err(); err != nil {
		return MemoryUsage{}, fmt.Errorf("line %d: read meminfo: %w", lineNumber+1, err)
	}

	total, ok := values["MemTotal"]
	if !ok {
		return MemoryUsage{}, fmt.Errorf("line 0: missing MemTotal")
	}
	available, ok := values["MemAvailable"]
	if !ok {
		return MemoryUsage{}, fmt.Errorf("line 0: missing MemAvailable")
	}
	if total.bytes == 0 {
		return MemoryUsage{}, fmt.Errorf("line %d: MemTotal must be greater than zero", total.line)
	}
	if available.bytes > total.bytes {
		return MemoryUsage{}, fmt.Errorf("line %d: MemAvailable exceeds MemTotal", available.line)
	}

	used := total.bytes - available.bytes
	return MemoryUsage{
		TotalBytes:     total.bytes,
		AvailableBytes: available.bytes,
		UsedBytes:      used,
		UsedPercent:    float64(used) / float64(total.bytes) * 100,
	}, nil
}
