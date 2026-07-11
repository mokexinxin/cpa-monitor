package mailer

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"html"
	"math"
	"mime"
	"mime/quotedprintable"
	"net/mail"
	"strings"
	"time"
)

// Kind identifies whether a message announces a newly active alert or a
// recovery from one.
type Kind string

const (
	Alert    Kind = "ALERT"
	Recovery Kind = "RECOVERY"
)

// Event contains transport-independent alert content.
type Event struct {
	Kind      Kind
	Object    string
	Hostname  string
	Timestamp time.Time
	Key       string
	Current   string
	Threshold string
	Details   string
	BaseURL   string
}

// HealthReport is the complete data model for a healthy-status email.
type HealthReport struct {
	Hostname               string
	Timestamp              time.Time
	NextScheduledAt        time.Time
	BaseURL                string
	MemoryUsedPercent      float64
	MemoryThreshold        float64
	HighestDiskUsedPercent float64
	DiskMountCount         int
	DiskThreshold          float64
	TotalTCPConnections    int
	TotalTCPThreshold      int
	ServicePort            int
	ServicePortConnections int
	ServicePortThreshold   int
	AccountCount           int
}

// BuildMessage constructs a multipart alert or recovery message with both
// plain-text and HTML alternatives.
func BuildMessage(from string, to []string, event Event) ([]byte, error) {
	if err := validateEvent(event); err != nil {
		return nil, err
	}
	timestamp := event.Timestamp.UTC()
	var plain strings.Builder
	writeBodyField(&plain, "Event", string(event.Kind))
	writeBodyField(&plain, "Host", event.Hostname)
	writeBodyField(&plain, "Timestamp", timestamp.Format(time.RFC3339))
	writeBodyField(&plain, "Alert key", event.Key)
	writeBodyField(&plain, "Current", event.Current)
	writeBodyField(&plain, "Threshold", event.Threshold)
	plain.WriteString("Details:\r\n")
	plain.WriteString(normalizeCRLF(event.Details))
	plain.WriteString("\r\n")
	writeBodyField(&plain, "CLIProxyAPI base URL", event.BaseURL)

	statusColor := "#b42318"
	statusBackground := "#fee4e2"
	statusBorder := "#fecdca"
	if event.Kind == Recovery {
		statusColor = "#166534"
		statusBackground = "#dcfce7"
		statusBorder = "#bbf7d0"
	}
	body := renderAlertHTML(event, timestamp, statusColor, statusBackground, statusBorder)
	return buildAlternative(from, to, timestamp, "[CPA Monitor] "+string(event.Kind)+" "+event.Object, plain.String(), body)
}

// BuildHealthMessage constructs the scheduled healthy-status report.
func BuildHealthMessage(from string, to []string, report HealthReport) ([]byte, error) {
	if err := validateHealthReport(report); err != nil {
		return nil, err
	}
	timestamp := report.Timestamp.UTC()
	var plain strings.Builder
	plain.WriteString("CPA Monitor health report\r\n\r\n")
	writeBodyField(&plain, "Status", "HEALTHY - all checks passed")
	writeBodyField(&plain, "Host", report.Hostname)
	writeBodyField(&plain, "Checked at", timestamp.Format(time.RFC3339))
	writeBodyField(&plain, "CLIProxyAPI", "reachable")
	writeBodyField(&plain, "Memory", fmt.Sprintf("%.1f%% used (alert at %.1f%%)", report.MemoryUsedPercent, report.MemoryThreshold))
	writeBodyField(&plain, "Disk", fmt.Sprintf("%.1f%% highest across %d mount(s) (alert at %.1f%%)", report.HighestDiskUsedPercent, report.DiskMountCount, report.DiskThreshold))
	writeBodyField(&plain, "TCP", fmt.Sprintf("%d total (alert at %d)", report.TotalTCPConnections, report.TotalTCPThreshold))
	writeBodyField(&plain, fmt.Sprintf("Service port %d", report.ServicePort), fmt.Sprintf("%d connections (alert at %d)", report.ServicePortConnections, report.ServicePortThreshold))
	writeBodyField(&plain, "Accounts", fmt.Sprintf("%d checked", report.AccountCount))
	writeBodyField(&plain, "CLIProxyAPI base URL", report.BaseURL)
	writeBodyField(&plain, "Next scheduled report", report.NextScheduledAt.UTC().Format(time.RFC3339))

	body := renderHealthHTML(report)
	return buildAlternative(from, to, timestamp, "[CPA Monitor] HEALTHY "+report.Hostname, plain.String(), body)
}

func buildAlternative(from string, to []string, timestamp time.Time, subject, plain, htmlBody string) ([]byte, error) {
	fromAddress, recipients, err := parseEnvelope(from, to)
	if err != nil {
		return nil, err
	}
	random := make([]byte, 18)
	if _, err := rand.Read(random); err != nil {
		return nil, fmt.Errorf("generate SMTP message ID: %w", err)
	}
	id := hex.EncodeToString(random[:12])
	boundary := "cpa-monitor-" + hex.EncodeToString(random[12:])

	var message strings.Builder
	writeHeader(&message, "From", fromAddress.String())
	recipientHeaders := make([]string, 0, len(recipients))
	for _, recipient := range recipients {
		recipientHeaders = append(recipientHeaders, recipient.String())
	}
	writeHeader(&message, "To", strings.Join(recipientHeaders, ", "))
	writeHeader(&message, "Date", timestamp.UTC().Format(time.RFC1123Z))
	writeHeader(&message, "Message-ID", fmt.Sprintf("<%d.%s@cpa-monitor.local>", timestamp.UnixNano(), id))
	writeHeader(&message, "Subject", mime.QEncoding.Encode("utf-8", subject))
	writeHeader(&message, "MIME-Version", "1.0")
	writeHeader(&message, "Content-Type", fmt.Sprintf("multipart/alternative; boundary=%q", boundary))
	message.WriteString("\r\n")
	if err := writePart(&message, boundary, "text/plain; charset=UTF-8", plain); err != nil {
		return nil, err
	}
	if err := writePart(&message, boundary, "text/html; charset=UTF-8", htmlBody); err != nil {
		return nil, err
	}
	message.WriteString("--" + boundary + "--\r\n")
	return []byte(message.String()), nil
}

func writePart(message *strings.Builder, boundary, contentType, body string) error {
	message.WriteString("--" + boundary + "\r\n")
	writeHeader(message, "Content-Type", contentType)
	writeHeader(message, "Content-Transfer-Encoding", "quoted-printable")
	message.WriteString("\r\n")
	encoder := quotedprintable.NewWriter(message)
	if _, err := encoder.Write([]byte(normalizeCRLF(body))); err != nil {
		return fmt.Errorf("encode SMTP message body: %w", err)
	}
	if err := encoder.Close(); err != nil {
		return fmt.Errorf("finish SMTP message body: %w", err)
	}
	message.WriteString("\r\n")
	return nil
}

func renderAlertHTML(event Event, timestamp time.Time, color, background, border string) string {
	return emailShell(string(event.Kind)+" · CPA Monitor", fmt.Sprintf(`
<table role="presentation" width="100%%" cellspacing="0" cellpadding="0" style="border-collapse:separate;border-spacing:0 12px">
<tr><td><span style="display:inline-block;padding:7px 12px;border:1px solid %s;border-radius:999px;background:%s;color:%s;font-size:12px;font-weight:700;letter-spacing:.08em">%s</span></td></tr>
<tr><td style="font-size:24px;line-height:1.3;font-weight:700;color:#172033">%s</td></tr>
</table>
%s
<div style="margin-top:16px;padding:16px;border:1px solid #dbe3ef;border-radius:10px;background:#f8fafc">
<div style="font-size:12px;font-weight:700;letter-spacing:.06em;color:#475467;text-transform:uppercase">Details</div>
<pre style="margin:8px 0 0;white-space:pre-wrap;word-break:break-word;font:14px/1.55 ui-monospace,SFMono-Regular,Consolas,monospace;color:#344054">%s</pre>
</div>`, color, background, color, html.EscapeString(string(event.Kind)), html.EscapeString(event.Object), detailTable([][2]string{
		{"Host", event.Hostname}, {"Timestamp", timestamp.Format(time.RFC3339)}, {"Alert key", event.Key},
		{"Current", event.Current}, {"Threshold", event.Threshold}, {"CLIProxyAPI", event.BaseURL},
	}), html.EscapeString(event.Details)))
}

func renderHealthHTML(report HealthReport) string {
	timestamp := report.Timestamp.UTC()
	next := report.NextScheduledAt.UTC()
	memoryCard := metricCard("Memory", fmt.Sprintf("%.1f%%", report.MemoryUsedPercent), fmt.Sprintf("Alert at %.1f%%", report.MemoryThreshold))
	diskCard := metricCard("Highest disk", fmt.Sprintf("%.1f%%", report.HighestDiskUsedPercent), fmt.Sprintf("%d mount(s) · alert at %.1f%%", report.DiskMountCount, report.DiskThreshold))
	tcpCard := metricCard("Total TCP", fmt.Sprintf("%d", report.TotalTCPConnections), fmt.Sprintf("Alert at %d", report.TotalTCPThreshold))
	portCard := metricCard(fmt.Sprintf("Port %d", report.ServicePort), fmt.Sprintf("%d", report.ServicePortConnections), fmt.Sprintf("Alert at %d", report.ServicePortThreshold))
	return emailShell("Healthy · CPA Monitor", fmt.Sprintf(`
<table role="presentation" width="100%%" cellspacing="0" cellpadding="0"><tr>
<td><span style="display:inline-block;padding:7px 12px;border:1px solid #bbf7d0;border-radius:999px;background:#dcfce7;color:#166534;font-size:12px;font-weight:700;letter-spacing:.08em">HEALTHY</span></td>
</tr><tr><td style="padding-top:14px;font-size:26px;line-height:1.25;font-weight:700;color:#172033">All systems are operating normally</td></tr>
<tr><td style="padding-top:8px;font-size:15px;line-height:1.6;color:#475467">All five monitoring scopes completed successfully with no active conditions.</td></tr></table>
<table role="presentation" width="100%%" cellspacing="0" cellpadding="0" style="margin-top:20px;border-collapse:collapse"><tr>%s%s</tr><tr>%s%s</tr></table>
<div style="margin-top:20px">%s</div>
<div style="margin-top:18px;padding:14px 16px;border-left:4px solid #1d4ed8;background:#eff6ff;color:#1e3a8a;font-size:14px;line-height:1.55"><strong>Next scheduled report</strong><br>%s</div>`, memoryCard, diskCard, tcpCard, portCard, detailTable([][2]string{
		{"Host", report.Hostname}, {"Checked at", timestamp.Format(time.RFC3339)}, {"CLIProxyAPI", "Reachable"},
		{"Accounts checked", fmt.Sprintf("%d", report.AccountCount)}, {"Base URL", report.BaseURL},
	}), html.EscapeString(next.Format(time.RFC3339))))
}

func emailShell(preheader, content string) string {
	return `<!doctype html><html><head><meta charset="UTF-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>CPA Monitor</title></head>` +
		`<body style="margin:0;padding:0;background:#f4f7fb;color:#172033;font-family:-apple-system,BlinkMacSystemFont,'Segoe UI',Arial,sans-serif">` +
		`<div style="display:none;max-height:0;overflow:hidden;opacity:0">` + html.EscapeString(preheader) + `</div>` +
		`<table role="presentation" width="100%" cellspacing="0" cellpadding="0"><tr><td align="center" style="padding:24px 12px">` +
		`<table role="presentation" width="100%" cellspacing="0" cellpadding="0" style="max-width:640px;background:#fff;border:1px solid #dbe3ef;border-radius:14px;box-shadow:0 8px 24px rgba(16,24,40,.06)">` +
		`<tr><td style="padding:18px 24px;border-bottom:1px solid #e7edf5;font-size:14px;font-weight:700;letter-spacing:.04em;color:#1d4ed8">CPA MONITOR</td></tr>` +
		`<tr><td style="padding:26px 24px">` + content + `</td></tr>` +
		`<tr><td style="padding:16px 24px;border-top:1px solid #e7edf5;font-size:12px;line-height:1.5;color:#667085">Automated infrastructure status from CPA Monitor. A plain-text version is included for compatibility.</td></tr>` +
		`</table></td></tr></table></body></html>`
}

func metricCard(label, value, note string) string {
	return fmt.Sprintf(`<td width="50%%" valign="top" style="padding:4px"><div style="min-height:86px;padding:14px 12px;border:1px solid #dbe3ef;border-radius:10px;background:#f8fafc"><div style="font-size:13px;line-height:1.3;color:#667085">%s</div><div style="margin-top:5px;font-size:22px;line-height:1.2;font-weight:700;color:#172033">%s</div><div style="margin-top:5px;font-size:12px;line-height:1.35;color:#667085">%s</div></div></td>`, html.EscapeString(label), html.EscapeString(value), html.EscapeString(note))
}

func detailTable(rows [][2]string) string {
	var body strings.Builder
	body.WriteString(`<table role="presentation" width="100%" cellspacing="0" cellpadding="0" style="border:1px solid #dbe3ef;border-radius:10px;border-collapse:separate;overflow:hidden">`)
	for index, row := range rows {
		border := ""
		if index > 0 {
			border = "border-top:1px solid #e7edf5;"
		}
		fmt.Fprintf(&body, `<tr><td valign="top" style="%spadding:10px 12px;width:35%%;font-size:13px;font-weight:600;color:#475467">%s</td><td valign="top" style="%spadding:10px 12px;font-size:13px;word-break:break-word;color:#172033">%s</td></tr>`, border, html.EscapeString(row[0]), border, html.EscapeString(row[1]))
	}
	body.WriteString(`</table>`)
	return body.String()
}

func validateEvent(event Event) error {
	if event.Kind != Alert && event.Kind != Recovery {
		return fmt.Errorf("mail event kind must be ALERT or RECOVERY")
	}
	if strings.TrimSpace(event.Object) == "" || containsHeaderControl(event.Object) {
		return fmt.Errorf("mail event object must not be empty or contain header control characters")
	}
	if strings.TrimSpace(event.Hostname) == "" || event.Timestamp.IsZero() || strings.TrimSpace(event.Key) == "" || strings.TrimSpace(event.BaseURL) == "" {
		return fmt.Errorf("mail event hostname, timestamp, key, and base URL are required")
	}
	return nil
}

func validateHealthReport(report HealthReport) error {
	if strings.TrimSpace(report.Hostname) == "" || containsHeaderControl(report.Hostname) {
		return fmt.Errorf("health report hostname must not be empty or contain header control characters")
	}
	if report.Timestamp.IsZero() || report.NextScheduledAt.IsZero() || strings.TrimSpace(report.BaseURL) == "" {
		return fmt.Errorf("health report timestamps and base URL are required")
	}
	for _, percent := range []float64{report.MemoryUsedPercent, report.MemoryThreshold, report.HighestDiskUsedPercent, report.DiskThreshold} {
		if math.IsNaN(percent) || math.IsInf(percent, 0) || percent < 0 || percent > 100 {
			return fmt.Errorf("health report percentages must be between 0 and 100")
		}
	}
	if report.MemoryThreshold <= 0 || report.DiskThreshold <= 0 || report.DiskMountCount < 0 || report.TotalTCPConnections < 0 || report.TotalTCPThreshold <= 0 || report.ServicePort < 1 || report.ServicePort > 65535 || report.ServicePortConnections < 0 || report.ServicePortThreshold <= 0 || report.AccountCount < 0 {
		return fmt.Errorf("health report counters and thresholds are invalid")
	}
	return nil
}

func parseEnvelope(from string, to []string) (*mail.Address, []*mail.Address, error) {
	if containsLineBreak(from) {
		return nil, nil, fmt.Errorf("SMTP From address must not contain line breaks")
	}
	fromAddress, err := mail.ParseAddress(from)
	if err != nil || !validMailboxAddress(fromAddress) {
		return nil, nil, fmt.Errorf("SMTP From address is invalid")
	}
	if len(to) == 0 {
		return nil, nil, fmt.Errorf("SMTP recipient list must not be empty")
	}
	recipients := make([]*mail.Address, 0, len(to))
	for _, value := range to {
		if containsLineBreak(value) {
			return nil, nil, fmt.Errorf("SMTP recipient address must not contain line breaks")
		}
		recipient, err := mail.ParseAddress(value)
		if err != nil || !validMailboxAddress(recipient) {
			return nil, nil, fmt.Errorf("SMTP recipient address is invalid")
		}
		recipients = append(recipients, recipient)
	}
	return fromAddress, recipients, nil
}

func validMailboxAddress(address *mail.Address) bool {
	if address == nil {
		return false
	}
	at := strings.LastIndexByte(address.Address, '@')
	return at > 0 && at < len(address.Address)-1
}

func writeHeader(builder *strings.Builder, name, value string) {
	builder.WriteString(name + ": " + value + "\r\n")
}

func writeBodyField(builder *strings.Builder, name, value string) {
	builder.WriteString(name + ": " + normalizeCRLF(value) + "\r\n")
}

func normalizeCRLF(value string) string {
	value = strings.ReplaceAll(value, "\r\n", "\n")
	value = strings.ReplaceAll(value, "\r", "\n")
	return strings.ReplaceAll(value, "\n", "\r\n")
}

func containsLineBreak(value string) bool { return strings.ContainsAny(value, "\r\n") }

func containsHeaderControl(value string) bool {
	for _, character := range value {
		if character < 0x20 || character == 0x7f {
			return true
		}
	}
	return false
}
