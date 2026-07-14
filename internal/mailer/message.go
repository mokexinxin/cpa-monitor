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

	"github.com/mokexinxin/cpa-monitor/internal/notification"
)

// These aliases preserve the public mail rendering API while the monitoring
// core uses the transport-neutral notification model.
type Kind = notification.Kind

const (
	Alert    = notification.Alert
	Recovery = notification.Recovery
)

type Event = notification.Event
type HealthReport = notification.HealthReport

// BuildMessage constructs a multipart alert or recovery message with both
// plain-text and HTML alternatives in the default Chinese language.
func BuildMessage(from string, to []string, event Event) ([]byte, error) {
	return BuildMessageInLanguage(from, to, event, LanguageChinese)
}

// BuildMessageInLanguage constructs a localized alert or recovery message.
func BuildMessageInLanguage(from string, to []string, event Event, language string) ([]byte, error) {
	if !validLanguage(language) {
		return nil, fmt.Errorf("mail language must be zh-CN or en")
	}
	if err := validateEvent(event); err != nil {
		return nil, err
	}
	timestamp := event.Timestamp.UTC()
	var plain strings.Builder
	kind := localizedKind(event.Kind, language)
	object := localizedObject(event, language)
	labels := alertLabels(language)
	writeBodyField(&plain, labels.event, kind)
	writeBodyField(&plain, labels.host, event.Hostname)
	writeBodyField(&plain, labels.timestamp, timestamp.Format(time.RFC3339))
	writeBodyField(&plain, labels.key, event.Key)
	current := localizedCurrent(event.Current, language)
	if language == LanguageChinese && event.Kind == Recovery {
		current = "已恢复"
	}
	writeBodyField(&plain, labels.current, current)
	threshold := localizedThreshold(event.Threshold, language)
	writeBodyField(&plain, labels.threshold, threshold)
	plain.WriteString(labels.details + ":\r\n")
	plain.WriteString(normalizeCRLF(event.Details))
	plain.WriteString("\r\n")
	writeBodyField(&plain, labels.baseURL, event.BaseURL)

	statusColor := "#b42318"
	statusBackground := "#fee4e2"
	statusBorder := "#fecdca"
	if event.Kind == Recovery {
		statusColor = "#166534"
		statusBackground = "#dcfce7"
		statusBorder = "#bbf7d0"
	}
	body := renderAlertHTML(event, timestamp, statusColor, statusBackground, statusBorder, language, kind, object, current, threshold)
	subject := "[CPA Monitor] " + string(event.Kind) + " " + event.Object
	if language == LanguageChinese {
		subject = "[CPA Monitor] " + kind + "：" + object
	}
	return buildAlternative(from, to, timestamp, subject, plain.String(), body)
}

// BuildHealthMessage constructs the scheduled healthy-status report.
func BuildHealthMessage(from string, to []string, report HealthReport) ([]byte, error) {
	return BuildHealthMessageInLanguage(from, to, report, LanguageChinese)
}

// BuildHealthMessageInLanguage constructs a localized healthy-status report.
func BuildHealthMessageInLanguage(from string, to []string, report HealthReport, language string) ([]byte, error) {
	if !validLanguage(language) {
		return nil, fmt.Errorf("mail language must be zh-CN or en")
	}
	if err := validateHealthReport(report); err != nil {
		return nil, err
	}
	timestamp := report.Timestamp.UTC()
	var plain strings.Builder
	if language == LanguageChinese {
		plain.WriteString("CPA Monitor 服务器状态报告\r\n\r\n")
		writeBodyField(&plain, "状态", "健康 - 服务器四项检查均已通过")
		writeBodyField(&plain, "主机", report.Hostname)
		writeBodyField(&plain, "检查时间", timestamp.Format(time.RFC3339))
		writeBodyField(&plain, "CLIProxyAPI", "可访问")
		writeBodyField(&plain, "内存", fmt.Sprintf("已使用 %.1f%%（告警阈值 %.1f%%）", report.MemoryUsedPercent, report.MemoryThreshold))
		writeBodyField(&plain, "磁盘", fmt.Sprintf("%d 个挂载点中最高使用率 %.1f%%（告警阈值 %.1f%%）", report.DiskMountCount, report.HighestDiskUsedPercent, report.DiskThreshold))
		writeBodyField(&plain, "TCP", fmt.Sprintf("共 %d 个连接（告警阈值 %d）", report.TotalTCPConnections, report.TotalTCPThreshold))
		writeBodyField(&plain, fmt.Sprintf("服务端口 %d", report.ServicePort), fmt.Sprintf("%d 个连接（告警阈值 %d）", report.ServicePortConnections, report.ServicePortThreshold))
		if report.AccountUsageAvailable {
			writeBodyField(&plain, "账号", fmt.Sprintf("已启用 %d 个 / 已检查 %d 个", report.EnabledAccountCount, report.AccountCount))
			writePlainAccountUsages(&plain, report.AccountUsages, language, report.Timestamp)
		} else {
			writeBodyField(&plain, "账号用量", "暂不可用（账号检查失败，不影响服务器状态报告）")
		}
		writeBodyField(&plain, "CLIProxyAPI 地址", report.BaseURL)
		writeBodyField(&plain, "下次计划报告", report.NextScheduledAt.UTC().Format(time.RFC3339))
	} else {
		plain.WriteString("CPA Monitor server status report\r\n\r\n")
		writeBodyField(&plain, "Status", "HEALTHY - all four server checks passed")
		writeBodyField(&plain, "Host", report.Hostname)
		writeBodyField(&plain, "Checked at", timestamp.Format(time.RFC3339))
		writeBodyField(&plain, "CLIProxyAPI", "reachable")
		writeBodyField(&plain, "Memory", fmt.Sprintf("%.1f%% used (alert at %.1f%%)", report.MemoryUsedPercent, report.MemoryThreshold))
		writeBodyField(&plain, "Disk", fmt.Sprintf("%.1f%% highest across %d mount(s) (alert at %.1f%%)", report.HighestDiskUsedPercent, report.DiskMountCount, report.DiskThreshold))
		writeBodyField(&plain, "TCP", fmt.Sprintf("%d total (alert at %d)", report.TotalTCPConnections, report.TotalTCPThreshold))
		writeBodyField(&plain, fmt.Sprintf("Service port %d", report.ServicePort), fmt.Sprintf("%d connections (alert at %d)", report.ServicePortConnections, report.ServicePortThreshold))
		if report.AccountUsageAvailable {
			writeBodyField(&plain, "Accounts", fmt.Sprintf("%d enabled / %d checked", report.EnabledAccountCount, report.AccountCount))
			writePlainAccountUsages(&plain, report.AccountUsages, language, report.Timestamp)
		} else {
			writeBodyField(&plain, "Account usage", "temporarily unavailable (account check failed; server status reporting is unaffected)")
		}
		writeBodyField(&plain, "CLIProxyAPI base URL", report.BaseURL)
		writeBodyField(&plain, "Next scheduled report", report.NextScheduledAt.UTC().Format(time.RFC3339))
	}

	body := renderHealthHTML(report, language)
	subject := "[CPA Monitor] SERVER STATUS " + report.Hostname
	if language == LanguageChinese {
		subject = "[CPA Monitor] 服务器状态报告：" + report.Hostname
	}
	return buildAlternative(from, to, timestamp, subject, plain.String(), body)
}

func writePlainAccountUsages(plain *strings.Builder, usages []notification.AccountUsage, language string, checkedAt time.Time) {
	for _, usage := range usages {
		label := accountUsageLabel(usage)
		if language == LanguageChinese {
			writeBodyField(plain, "账号用量 "+label, accountUsageText(usage, language, checkedAt))
		} else {
			writeBodyField(plain, "Account usage "+label, accountUsageText(usage, language, checkedAt))
		}
	}
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

type localizedAlertLabels struct {
	event, host, timestamp, key, current, threshold, details, baseURL string
}

func alertLabels(language string) localizedAlertLabels {
	if language == LanguageChinese {
		return localizedAlertLabels{"事件", "主机", "时间", "告警键", "当前值", "阈值", "技术详情", "CLIProxyAPI 地址"}
	}
	return localizedAlertLabels{"Event", "Host", "Timestamp", "Alert key", "Current", "Threshold", "Details", "CLIProxyAPI base URL"}
}

func renderAlertHTML(event Event, timestamp time.Time, color, background, border, language, kind, object, current, threshold string) string {
	labels := alertLabels(language)
	return emailShell(kind+" · CPA Monitor", fmt.Sprintf(`
<table role="presentation" width="100%%" cellspacing="0" cellpadding="0" style="border-collapse:separate;border-spacing:0 12px">
<tr><td><span style="display:inline-block;padding:7px 12px;border:1px solid %s;border-radius:999px;background:%s;color:%s;font-size:12px;font-weight:700;letter-spacing:.08em">%s</span></td></tr>
<tr><td style="font-size:24px;line-height:1.3;font-weight:700;color:#172033">%s</td></tr>
</table>
%s
<div style="margin-top:16px;padding:16px;border:1px solid #dbe3ef;border-radius:10px;background:#f8fafc">
<div style="font-size:12px;font-weight:700;letter-spacing:.06em;color:#475467;text-transform:uppercase">%s</div>
<pre style="margin:8px 0 0;white-space:pre-wrap;word-break:break-word;font:14px/1.55 ui-monospace,SFMono-Regular,Consolas,monospace;color:#344054">%s</pre>
</div>`, color, background, color, html.EscapeString(kind), html.EscapeString(object), detailTable([][2]string{
		{labels.host, event.Hostname}, {labels.timestamp, timestamp.Format(time.RFC3339)}, {labels.key, event.Key},
		{labels.current, current}, {labels.threshold, threshold}, {labels.baseURL, event.BaseURL},
	}), html.EscapeString(labels.details), html.EscapeString(event.Details)), language)
}

func renderHealthHTML(report HealthReport, language string) string {
	timestamp := report.Timestamp.UTC()
	next := report.NextScheduledAt.UTC()
	memoryLabel, diskLabel, tcpLabel, portLabel := "Memory", "Highest disk", "Total TCP", fmt.Sprintf("Port %d", report.ServicePort)
	memoryNote := fmt.Sprintf("Alert at %.1f%%", report.MemoryThreshold)
	diskNote := fmt.Sprintf("%d mount(s) · alert at %.1f%%", report.DiskMountCount, report.DiskThreshold)
	tcpNote, portNote := fmt.Sprintf("Alert at %d", report.TotalTCPThreshold), fmt.Sprintf("Alert at %d", report.ServicePortThreshold)
	badge, heading, summary, nextLabel := "HEALTHY", "Server systems are operating normally", "All four server scopes completed successfully. Account status and alerts are handled independently.", "Next scheduled report"
	accountSummary := "Temporarily unavailable"
	if report.AccountUsageAvailable {
		accountSummary = fmt.Sprintf("%d enabled / %d checked", report.EnabledAccountCount, report.AccountCount)
	}
	rows := [][2]string{{"Host", report.Hostname}, {"Checked at", timestamp.Format(time.RFC3339)}, {"CLIProxyAPI", "Reachable"}, {"Account usage", accountSummary}, {"Base URL", report.BaseURL}}
	preheader := "Server status · CPA Monitor"
	if language == LanguageChinese {
		memoryLabel, diskLabel, tcpLabel, portLabel = "内存", "最高磁盘使用率", "TCP 连接总数", fmt.Sprintf("端口 %d", report.ServicePort)
		memoryNote = fmt.Sprintf("告警阈值 %.1f%%", report.MemoryThreshold)
		diskNote = fmt.Sprintf("%d 个挂载点 · 告警阈值 %.1f%%", report.DiskMountCount, report.DiskThreshold)
		tcpNote, portNote = fmt.Sprintf("告警阈值 %d", report.TotalTCPThreshold), fmt.Sprintf("告警阈值 %d", report.ServicePortThreshold)
		badge, heading, summary, nextLabel = "健康", "服务器系统运行正常", "服务器四项监控检查均已成功完成；账号状态与告警独立处理。", "下次计划报告"
		accountSummary = "暂不可用"
		if report.AccountUsageAvailable {
			accountSummary = fmt.Sprintf("已启用 %d 个 / 已检查 %d 个", report.EnabledAccountCount, report.AccountCount)
		}
		rows = [][2]string{{"主机", report.Hostname}, {"检查时间", timestamp.Format(time.RFC3339)}, {"CLIProxyAPI", "可访问"}, {"账号用量", accountSummary}, {"服务地址", report.BaseURL}}
		preheader = "服务器状态 · CPA Monitor"
	}
	memoryCard := metricCard(memoryLabel, fmt.Sprintf("%.1f%%", report.MemoryUsedPercent), memoryNote)
	diskCard := metricCard(diskLabel, fmt.Sprintf("%.1f%%", report.HighestDiskUsedPercent), diskNote)
	tcpCard := metricCard(tcpLabel, fmt.Sprintf("%d", report.TotalTCPConnections), tcpNote)
	portCard := metricCard(portLabel, fmt.Sprintf("%d", report.ServicePortConnections), portNote)
	accountUsageSection := ""
	if report.AccountUsageAvailable {
		accountUsageSection = renderAccountUsageHTML(report.AccountUsages, language, report.Timestamp)
	}
	return emailShell(preheader, fmt.Sprintf(`
<table role="presentation" width="100%%" cellspacing="0" cellpadding="0"><tr>
<td><span style="display:inline-block;padding:7px 12px;border:1px solid #bbf7d0;border-radius:999px;background:#dcfce7;color:#166534;font-size:12px;font-weight:700;letter-spacing:.08em">%s</span></td>
</tr><tr><td style="padding-top:14px;font-size:26px;line-height:1.25;font-weight:700;color:#172033">%s</td></tr>
<tr><td style="padding-top:8px;font-size:15px;line-height:1.6;color:#475467">%s</td></tr></table>
<table role="presentation" width="100%%" cellspacing="0" cellpadding="0" style="margin-top:20px;border-collapse:collapse"><tr>%s%s</tr><tr>%s%s</tr></table>
<div style="margin-top:20px">%s</div>
%s
<div style="margin-top:18px;padding:14px 16px;border-left:4px solid #1d4ed8;background:#eff6ff;color:#1e3a8a;font-size:14px;line-height:1.55"><strong>%s</strong><br>%s</div>`, html.EscapeString(badge), html.EscapeString(heading), html.EscapeString(summary), memoryCard, diskCard, tcpCard, portCard, detailTable(rows), accountUsageSection, html.EscapeString(nextLabel), html.EscapeString(next.Format(time.RFC3339))), language)
}

func renderAccountUsageHTML(usages []notification.AccountUsage, language string, checkedAt time.Time) string {
	if len(usages) == 0 {
		return ""
	}
	heading := "Enabled account usage"
	if language == LanguageChinese {
		heading = "已启用账号用量"
	}
	rows := make([][2]string, 0, len(usages))
	for _, usage := range usages {
		rows = append(rows, [2]string{accountUsageLabel(usage), accountUsageText(usage, language, checkedAt)})
	}
	return `<div style="margin-top:20px"><div style="margin-bottom:8px;font-size:15px;font-weight:700;color:#172033">` + html.EscapeString(heading) + `</div>` + detailTable(rows) + `</div>`
}

func accountUsageLabel(usage notification.AccountUsage) string {
	label := strings.TrimSpace(usage.Label)
	if provider := strings.TrimSpace(usage.Provider); provider != "" {
		label += " (" + provider + ")"
	}
	return label
}

func accountUsageText(usage notification.AccountUsage, language string, checkedAt time.Time) string {
	parts := make([]string, 0, len(usage.QuotaWindows)+2)
	if usage.QuotaSupported {
		if usage.QuotaAvailable {
			if plan := strings.TrimSpace(usage.PlanType); plan != "" {
				if language == LanguageChinese {
					parts = append(parts, "套餐 "+plan)
				} else {
					parts = append(parts, "plan "+plan)
				}
			}
			for _, window := range usage.QuotaWindows {
				parts = append(parts, quotaWindowText(window, language, checkedAt))
			}
		} else if language == LanguageChinese {
			parts = append(parts, "套餐额度获取失败（不影响服务器状态报告）")
		} else {
			parts = append(parts, "plan quota unavailable (server status reporting is unaffected)")
		}
	}
	total, recent := usage.Success+usage.Failed, usage.RecentSuccess+usage.RecentFailed
	if language == LanguageChinese {
		parts = append(parts, fmt.Sprintf("请求统计：进程累计 %d 次（成功 %d / 失败 %d），近期 %d 次（成功 %d / 失败 %d）", total, usage.Success, usage.Failed, recent, usage.RecentSuccess, usage.RecentFailed))
		return strings.Join(parts, "；")
	}
	parts = append(parts, fmt.Sprintf("request stats: process total %d (success %d / failed %d), recent %d (success %d / failed %d)", total, usage.Success, usage.Failed, recent, usage.RecentSuccess, usage.RecentFailed))
	return strings.Join(parts, "; ")
}

func quotaWindowText(window notification.QuotaWindow, language string, checkedAt time.Time) string {
	label := quotaWindowLabel(window.Kind, language)
	usageText := "用量未知"
	if language != LanguageChinese {
		usageText = "usage unavailable"
	}
	if window.UsedPercent != nil {
		remaining := 100 - *window.UsedPercent
		if language == LanguageChinese {
			usageText = fmt.Sprintf("已用 %.1f%%，剩余 %.1f%%", *window.UsedPercent, remaining)
		} else {
			usageText = fmt.Sprintf("%.1f%% used, %.1f%% remaining", *window.UsedPercent, remaining)
		}
	}
	resetAt := window.ResetAt
	if resetAt.IsZero() && window.ResetAfter > 0 {
		resetAt = checkedAt.Add(window.ResetAfter)
	}
	if !resetAt.IsZero() {
		if language == LanguageChinese {
			usageText += "，重置 " + resetAt.UTC().Format("2006-01-02 15:04 UTC")
		} else {
			usageText += ", resets " + resetAt.UTC().Format("2006-01-02 15:04 UTC")
		}
	}
	if language == LanguageChinese {
		return label + "：" + usageText
	}
	return label + ": " + usageText
}

func quotaWindowLabel(kind, language string) string {
	labels := map[string][2]string{
		"five_hour": {"5 小时限额", "5-hour limit"},
		"weekly":    {"周限额", "weekly limit"},
		"monthly":   {"月度限额", "monthly limit"},
		"primary":   {"短周期限额", "primary limit"},
		"secondary": {"长周期限额", "secondary limit"},
	}
	label, ok := labels[kind]
	if !ok {
		label = [2]string{"套餐限额", "plan limit"}
	}
	if language == LanguageChinese {
		return label[0]
	}
	return label[1]
}

func emailShell(preheader, content, language string) string {
	footer := "Automated infrastructure status from CPA Monitor. A plain-text version is included for compatibility."
	if language == LanguageChinese {
		footer = "此邮件由 CPA Monitor 自动发送，同时附带纯文本版本以兼容不同邮件客户端。"
	}
	return `<!doctype html><html lang="` + language + `"><head><meta charset="UTF-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>CPA Monitor</title></head>` +
		`<body style="margin:0;padding:0;background:#f4f7fb;color:#172033;font-family:-apple-system,BlinkMacSystemFont,'Segoe UI',Arial,sans-serif">` +
		`<div style="display:none;max-height:0;overflow:hidden;opacity:0">` + html.EscapeString(preheader) + `</div>` +
		`<table role="presentation" width="100%" cellspacing="0" cellpadding="0"><tr><td align="center" style="padding:24px 12px">` +
		`<table role="presentation" width="100%" cellspacing="0" cellpadding="0" style="max-width:640px;background:#fff;border:1px solid #dbe3ef;border-radius:14px;box-shadow:0 8px 24px rgba(16,24,40,.06)">` +
		`<tr><td style="padding:18px 24px;border-bottom:1px solid #e7edf5;font-size:14px;font-weight:700;letter-spacing:.04em;color:#1d4ed8">CPA MONITOR</td></tr>` +
		`<tr><td style="padding:26px 24px">` + content + `</td></tr>` +
		`<tr><td style="padding:16px 24px;border-top:1px solid #e7edf5;font-size:12px;line-height:1.5;color:#667085">` + footer + `</td></tr>` +
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
	if strings.TrimSpace(event.Hostname) == "" || event.Timestamp.IsZero() || strings.TrimSpace(event.Key) == "" || containsHeaderControl(event.Key) || strings.TrimSpace(event.BaseURL) == "" {
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
	if report.MemoryThreshold <= 0 || report.DiskThreshold <= 0 || report.DiskMountCount < 0 || report.TotalTCPConnections < 0 || report.TotalTCPThreshold <= 0 || report.ServicePort < 1 || report.ServicePort > 65535 || report.ServicePortConnections < 0 || report.ServicePortThreshold <= 0 || report.AccountCount < 0 || report.EnabledAccountCount < 0 || report.EnabledAccountCount > report.AccountCount {
		return fmt.Errorf("health report counters and thresholds are invalid")
	}
	if report.AccountUsageAvailable && report.EnabledAccountCount != len(report.AccountUsages) {
		return fmt.Errorf("health report enabled account count is invalid")
	}
	if !report.AccountUsageAvailable && (report.AccountCount != 0 || report.EnabledAccountCount != 0 || len(report.AccountUsages) != 0) {
		return fmt.Errorf("unavailable account usage must not contain counters")
	}
	for _, usage := range report.AccountUsages {
		if notification.ValidateAccountUsage(usage) != nil {
			return fmt.Errorf("health report account usage is invalid")
		}
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
