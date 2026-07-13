package dingtalk

import (
	"errors"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/mokexinxin/cpa-monitor/internal/notification"
)

const (
	maxFieldRunes    = 1500
	maxMarkdownRunes = 18000
)

type markdownPayload struct {
	MessageType string          `json:"msgtype"`
	Markdown    markdownContent `json:"markdown"`
	At          atContent       `json:"at"`
}

type markdownContent struct {
	Title string `json:"title"`
	Text  string `json:"text"`
}

type atContent struct {
	Mobiles []string `json:"atMobiles,omitempty"`
	UserIDs []string `json:"atUserIds,omitempty"`
	AtAll   bool     `json:"isAtAll"`
}

func alertPayload(batch notification.Batch, language string, maxItems int, at atContent) (markdownPayload, error) {
	if err := notification.ValidateBatch(batch); err != nil {
		return markdownPayload{}, err
	}
	if err := validateLanguage(language); err != nil {
		return markdownPayload{}, err
	}
	if maxItems <= 0 {
		return markdownPayload{}, errors.New("DingTalk max items must be greater than zero")
	}

	kind, scope := localizedKind(batch.Kind, language), localizedScope(batch.Scope, language)
	title := fmt.Sprintf("CPA Monitor · %s · %s", kind, scope)
	var text strings.Builder
	writeMentions(&text, at)
	text.WriteString("## " + escapeMarkdown(title) + "\n\n")
	if language == "zh-CN" {
		writeMarkdownField(&text, "主机", batch.Hostname)
		writeMarkdownField(&text, "时间", batch.Timestamp.UTC().Format("2006-01-02 15:04:05 UTC"))
	} else {
		writeMarkdownField(&text, "Host", batch.Hostname)
		writeMarkdownField(&text, "Time", batch.Timestamp.UTC().Format("2006-01-02 15:04:05 UTC"))
	}

	shown := len(batch.Events)
	if shown > maxItems {
		shown = maxItems
	}
	for i, event := range batch.Events[:shown] {
		text.WriteString(fmt.Sprintf("\n### %d. %s\n\n", i+1, escapeMarkdown(event.Object)))
		if language == "zh-CN" {
			writeMarkdownField(&text, "当前值", event.Current)
			writeMarkdownField(&text, "阈值", event.Threshold)
			writeMarkdownField(&text, "告警键", event.Key)
			writeMarkdownField(&text, "详情", event.Details)
		} else {
			writeMarkdownField(&text, "Current", event.Current)
			writeMarkdownField(&text, "Threshold", event.Threshold)
			writeMarkdownField(&text, "Alert key", event.Key)
			writeMarkdownField(&text, "Details", event.Details)
		}
	}
	if omitted := len(batch.Events) - shown; omitted > 0 {
		if language == "zh-CN" {
			text.WriteString(fmt.Sprintf("\n> 另有 %d 项未在消息中展开，请查看监控日志。\n", omitted))
		} else {
			text.WriteString(fmt.Sprintf("\n> %d additional item(s) omitted; see monitor logs.\n", omitted))
		}
	}
	if baseURL := firstBaseURL(batch.Events); baseURL != "" {
		if language == "zh-CN" {
			writeMarkdownField(&text, "CLIProxyAPI 地址", baseURL)
		} else {
			writeMarkdownField(&text, "CLIProxyAPI base URL", baseURL)
		}
	}
	return markdownPayload{MessageType: "markdown", Markdown: markdownContent{Title: truncateRunes(title, 100), Text: fitMarkdown(text.String(), language)}, At: at}, nil
}

func healthPayload(report notification.HealthReport, language string, at atContent) (markdownPayload, error) {
	if err := validateHealthReport(report); err != nil {
		return markdownPayload{}, err
	}
	if err := validateLanguage(language); err != nil {
		return markdownPayload{}, err
	}
	title := "CPA Monitor · Healthy"
	var text strings.Builder
	writeMentions(&text, at)
	if language == "zh-CN" {
		title = "CPA Monitor · 健康报告"
		text.WriteString("## " + title + "\n\n")
		text.WriteString("> 所有监控检查均已通过，当前没有活动告警。\n\n")
		writeMarkdownField(&text, "主机", report.Hostname)
		writeMarkdownField(&text, "检查时间", report.Timestamp.UTC().Format("2006-01-02 15:04:05 UTC"))
		writeMarkdownField(&text, "内存", fmt.Sprintf("已使用 %.1f%%（阈值 %.1f%%）", report.MemoryUsedPercent, report.MemoryThreshold))
		writeMarkdownField(&text, "磁盘", fmt.Sprintf("%d 个挂载点，最高 %.1f%%（阈值 %.1f%%）", report.DiskMountCount, report.HighestDiskUsedPercent, report.DiskThreshold))
		writeMarkdownField(&text, "TCP", fmt.Sprintf("共 %d 个连接（阈值 %d）", report.TotalTCPConnections, report.TotalTCPThreshold))
		writeMarkdownField(&text, fmt.Sprintf("服务端口 %d", report.ServicePort), fmt.Sprintf("%d 个连接（阈值 %d）", report.ServicePortConnections, report.ServicePortThreshold))
		writeMarkdownField(&text, "账号", fmt.Sprintf("已检查 %d 个", report.AccountCount))
		writeMarkdownField(&text, "CLIProxyAPI 地址", report.BaseURL)
		writeMarkdownField(&text, "下次计划报告", report.NextScheduledAt.UTC().Format("2006-01-02 15:04:05 UTC"))
	} else {
		text.WriteString("## " + title + "\n\n")
		text.WriteString("> All monitoring checks passed and there are no active alerts.\n\n")
		writeMarkdownField(&text, "Host", report.Hostname)
		writeMarkdownField(&text, "Checked at", report.Timestamp.UTC().Format("2006-01-02 15:04:05 UTC"))
		writeMarkdownField(&text, "Memory", fmt.Sprintf("%.1f%% used (threshold %.1f%%)", report.MemoryUsedPercent, report.MemoryThreshold))
		writeMarkdownField(&text, "Disk", fmt.Sprintf("%.1f%% highest across %d mount(s) (threshold %.1f%%)", report.HighestDiskUsedPercent, report.DiskMountCount, report.DiskThreshold))
		writeMarkdownField(&text, "TCP", fmt.Sprintf("%d total connections (threshold %d)", report.TotalTCPConnections, report.TotalTCPThreshold))
		writeMarkdownField(&text, fmt.Sprintf("Service port %d", report.ServicePort), fmt.Sprintf("%d connections (threshold %d)", report.ServicePortConnections, report.ServicePortThreshold))
		writeMarkdownField(&text, "Accounts", fmt.Sprintf("%d checked", report.AccountCount))
		writeMarkdownField(&text, "CLIProxyAPI base URL", report.BaseURL)
		writeMarkdownField(&text, "Next scheduled report", report.NextScheduledAt.UTC().Format("2006-01-02 15:04:05 UTC"))
	}
	return markdownPayload{MessageType: "markdown", Markdown: markdownContent{Title: truncateRunes(title, 100), Text: fitMarkdown(text.String(), language)}, At: at}, nil
}

func validateHealthReport(report notification.HealthReport) error {
	if strings.TrimSpace(report.Hostname) == "" || report.Timestamp.IsZero() || report.NextScheduledAt.IsZero() {
		return errors.New("DingTalk health report requires hostname and schedule timestamps")
	}
	if strings.TrimSpace(report.BaseURL) == "" {
		return errors.New("DingTalk health report base URL is required")
	}
	return nil
}

func validateLanguage(language string) error {
	if language != "zh-CN" && language != "en" {
		return errors.New("DingTalk language must be zh-CN or en")
	}
	return nil
}

func localizedKind(kind notification.Kind, language string) string {
	if language == "zh-CN" {
		if kind == notification.Recovery {
			return "恢复"
		}
		return "告警"
	}
	return string(kind)
}

func localizedScope(scope, language string) string {
	if language != "zh-CN" {
		return scope
	}
	if value, ok := map[string]string{
		"health": "健康检查", "memory": "内存", "disk": "磁盘", "network": "TCP 连接", "auth": "账号",
	}[scope]; ok {
		return value
	}
	return scope
}

func writeMarkdownField(builder *strings.Builder, label, value string) {
	value = truncateRunes(strings.TrimSpace(value), maxFieldRunes)
	lines := strings.Split(value, "\n")
	for i := range lines {
		lines[i] = escapeMarkdown(lines[i])
	}
	builder.WriteString("- **" + escapeMarkdown(label) + "**：" + strings.Join(lines, "<br>") + "\n")
}

func writeMentions(builder *strings.Builder, at atContent) {
	if at.AtAll {
		builder.WriteString("@all\n\n")
	}
	for _, mobile := range at.Mobiles {
		builder.WriteString("@" + escapeMarkdown(mobile) + " ")
	}
	if len(at.Mobiles) > 0 {
		builder.WriteString("\n\n")
	}
}

func firstBaseURL(events []notification.Event) string {
	for _, event := range events {
		if value := strings.TrimSpace(event.BaseURL); value != "" {
			return value
		}
	}
	return ""
}

func escapeMarkdown(value string) string {
	value = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) && r != '\n' && r != '\t' {
			return -1
		}
		return r
	}, value)
	replacer := strings.NewReplacer(
		"\\", "\\\\", "`", "\\`", "*", "\\*", "_", "\\_", "[", "\\[", "]", "\\]",
		"#", "\\#", "<", "&lt;", ">", "&gt;",
	)
	return replacer.Replace(value)
}

func truncateRunes(value string, limit int) string {
	if utf8.RuneCountInString(value) <= limit {
		return value
	}
	runes := []rune(value)
	return string(runes[:limit-1]) + "…"
}

func fitMarkdown(value, language string) string {
	if utf8.RuneCountInString(value) <= maxMarkdownRunes {
		return value
	}
	suffix := "\n\n> Message truncated; see monitor logs for complete details."
	if language == "zh-CN" {
		suffix = "\n\n> 消息内容已截断，请查看监控日志获取完整详情。"
	}
	suffixRunes := []rune(suffix)
	valueRunes := []rune(value)
	return string(valueRunes[:maxMarkdownRunes-len(suffixRunes)]) + suffix
}
