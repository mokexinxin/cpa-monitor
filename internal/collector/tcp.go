package collector

import (
	"bufio"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// ParseTCP counts every data row in a Linux /proc/net/tcp or tcp6 table,
// regardless of TCP state, and separately counts rows on servicePort.
func ParseTCP(r io.Reader, servicePort int) (TCPUsage, error) {
	if servicePort < 0 || servicePort > 65535 {
		return TCPUsage{}, fmt.Errorf("line 0: service port %d is outside 0..65535", servicePort)
	}
	if r == nil {
		return TCPUsage{}, fmt.Errorf("line 0: read TCP table: nil reader")
	}

	var result TCPUsage
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 4096), 4*1024*1024)
	lineNumber := 0
	for scanner.Scan() {
		lineNumber++
		fields := strings.Fields(scanner.Text())
		if len(fields) == 0 {
			continue
		}
		if len(fields) >= 2 && fields[0] == "sl" && fields[1] == "local_address" {
			continue
		}
		if len(fields) < 4 {
			return TCPUsage{}, fmt.Errorf("line %d: malformed TCP row: expected at least 4 fields", lineNumber)
		}
		slot := strings.TrimSuffix(fields[0], ":")
		if slot == fields[0] || slot == "" {
			return TCPUsage{}, fmt.Errorf("line %d: malformed TCP slot %q", lineNumber, fields[0])
		}
		if _, err := strconv.ParseUint(slot, 10, 64); err != nil {
			return TCPUsage{}, fmt.Errorf("line %d: parse TCP slot %q: %w", lineNumber, fields[0], err)
		}

		address, portText, ok := strings.Cut(fields[1], ":")
		if !ok || address == "" || portText == "" || strings.Contains(portText, ":") {
			return TCPUsage{}, fmt.Errorf("line %d: malformed local address %q", lineNumber, fields[1])
		}
		if !isHex(address) {
			return TCPUsage{}, fmt.Errorf("line %d: malformed local address %q", lineNumber, fields[1])
		}
		port, err := strconv.ParseUint(portText, 16, 16)
		if err != nil {
			return TCPUsage{}, fmt.Errorf("line %d: parse local port %q: %w", lineNumber, portText, err)
		}
		if len(fields[3]) != 2 {
			return TCPUsage{}, fmt.Errorf("line %d: malformed TCP state %q", lineNumber, fields[3])
		}
		if _, err := strconv.ParseUint(fields[3], 16, 8); err != nil {
			return TCPUsage{}, fmt.Errorf("line %d: parse TCP state %q: %w", lineNumber, fields[3], err)
		}

		result.TotalConnections++
		if int(port) == servicePort {
			result.ServicePortConnections++
		}
	}
	if err := scanner.Err(); err != nil {
		return TCPUsage{}, fmt.Errorf("line %d: read TCP table: %w", lineNumber+1, err)
	}
	return result, nil
}

func isHex(value string) bool {
	for i := 0; i < len(value); i++ {
		c := value[i]
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
			return false
		}
	}
	return value != ""
}
