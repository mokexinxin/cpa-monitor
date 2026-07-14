package dingtalk

import (
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/mokexinxin/cpa-monitor/internal/notification"
)

const (
	maxFieldRunes             = 1500
	maxMarkdownRunes          = 18000
	quotaAttentionPercent     = 75.0
	compactQuotaProgressSteps = 10
)

var beijingTime = time.FixedZone("Beijing Time", 8*60*60)

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
	if language == "zh-CN" {
		return compactChineseBatchPayload(batch, maxItems, at), nil
	}

	kind, scope := localizedKind(batch.Kind, language), localizedScope(batch.Scope, language)
	title := fmt.Sprintf("CPA Monitor · %s · %s", kind, scope)
	var text strings.Builder
	writeMentions(&text, at)
	text.WriteString("## " + escapeMarkdown(title) + "\n\n")
	writeMarkdownField(&text, "Host", batch.Hostname)
	writeMarkdownField(&text, "Time", batch.Timestamp.UTC().Format("2006-01-02 15:04:05 UTC"))

	shown := len(batch.Events)
	if shown > maxItems {
		shown = maxItems
	}
	for i, event := range batch.Events[:shown] {
		text.WriteString(fmt.Sprintf("\n### %d. %s\n\n", i+1, escapeMarkdown(event.Object)))
		writeMarkdownField(&text, "Current", event.Current)
		writeMarkdownField(&text, "Threshold", event.Threshold)
		writeMarkdownField(&text, "Alert key", event.Key)
		writeMarkdownField(&text, "Details", event.Details)
	}
	if omitted := len(batch.Events) - shown; omitted > 0 {
		text.WriteString(fmt.Sprintf("\n> %d additional item(s) omitted; see monitor logs.\n", omitted))
	}
	if baseURL := firstBaseURL(batch.Events); baseURL != "" {
		writeMarkdownField(&text, "CLIProxyAPI base URL", baseURL)
	}
	return markdownPayload{MessageType: "markdown", Markdown: markdownContent{Title: truncateRunes(title, 100), Text: fitMarkdown(text.String(), language)}, At: at}, nil
}

func compactChineseBatchPayload(batch notification.Batch, maxItems int, at atContent) markdownPayload {
	title := compactChineseBatchTitle(batch)
	var text strings.Builder
	writeMentions(&text, at)
	text.WriteString("## " + title + "\n\n")
	text.WriteString("> " + compactChineseBatchSummary(batch) + "\n\n")
	text.WriteString("**" + escapeMarkdown(batch.Hostname) + "**\n")

	shown := len(batch.Events)
	if shown > maxItems {
		shown = maxItems
	}
	if batch.Scope == "test" {
		writeCompactChineseTest(&text)
	} else {
		for i := range batch.Events[:shown] {
			text.WriteString("\n")
			if batch.Kind == notification.Recovery {
				writeCompactChineseRecoveryEvent(&text, batch.Scope, batch.Events[i])
			} else {
				writeCompactChineseAlertEvent(&text, batch.Scope, batch.Events[i])
			}
		}
	}
	if omitted := len(batch.Events) - shown; omitted > 0 {
		text.WriteString(fmt.Sprintf("\n> 另有 %d 项未展开，请查看监控日志。\n", omitted))
	}
	if baseURL := firstBaseURL(batch.Events); baseURL != "" {
		text.WriteString("\n" + markdownLink("管理面板", baseURL) + "\n")
	}
	timeLabel := "告警于"
	if batch.Kind == notification.Recovery {
		timeLabel = "恢复于"
	} else if batch.Scope == "test" {
		timeLabel = "测试于"
	}
	text.WriteString(fmt.Sprintf("\n%s %s（北京时间）\n", timeLabel, formatBeijingDateTime(batch.Timestamp)))
	return markdownPayload{
		MessageType: "markdown",
		Markdown: markdownContent{
			Title: truncateRunes(title, 100),
			Text:  fitMarkdown(text.String(), "zh-CN"),
		},
		At: at,
	}
}

func compactChineseBatchTitle(batch notification.Batch) string {
	if batch.Kind == notification.Recovery {
		if batch.Scope == "auth" {
			return "CPA Monitor · 账号恢复"
		}
		return "CPA Monitor · 恢复通知"
	}
	switch batch.Scope {
	case "test":
		return "CPA Monitor · 通知测试"
	case "health":
		return "CPA Monitor · 服务不可用"
	case "memory":
		return "CPA Monitor · 内存告警"
	case "disk":
		return "CPA Monitor · 磁盘告警"
	case "network":
		return "CPA Monitor · TCP 告警"
	case "auth":
		return "CPA Monitor · 账号告警"
	default:
		return "CPA Monitor · " + localizedScope(batch.Scope, "zh-CN") + "告警"
	}
}

func compactChineseBatchSummary(batch notification.Batch) string {
	if batch.Kind == notification.Recovery {
		if batch.Scope == "auth" {
			return "状态：账号已经恢复正常"
		}
		return fmt.Sprintf("状态：已恢复 · %d 项告警已经解除", len(batch.Events))
	}
	switch batch.Scope {
	case "test":
		return "状态：钉钉发送链路正常"
	case "health":
		return "状态：需要处理 · CLIProxyAPI 健康检查失败"
	case "memory":
		return "状态：需要处理 · 内存使用率超过阈值"
	case "disk":
		return fmt.Sprintf("状态：需要处理 · %d 个挂载点超过阈值", len(batch.Events))
	case "network":
		return fmt.Sprintf("状态：需要处理 · %d 项连接指标超过阈值", len(batch.Events))
	case "auth":
		return "状态：账号异常 · 不影响服务器状态报告"
	default:
		return fmt.Sprintf("状态：需要处理 · %d 项异常", len(batch.Events))
	}
}

func writeCompactChineseTest(text *strings.Builder) {
	text.WriteString("\n### 发送链路\n\n")
	text.WriteString("Webhook 请求成功\n\n")
	text.WriteString("签名验证通过\n\n")
	text.WriteString("消息格式解析正常\n\n")
	text.WriteString("这是一条手动发送的测试通知。\n")
}

func writeCompactChineseAlertEvent(text *strings.Builder, scope string, event notification.Event) {
	details := parseEventDetails(event.Details)
	switch scope {
	case "health":
		text.WriteString("### [告警] CLIProxyAPI\n\n")
		writeCompactField(text, "当前状态", "无法访问")
		if reason := strings.TrimSpace(details["error"]); reason != "" {
			writeCompactField(text, "失败原因", truncateRunes(reason, 500))
		}
		writeCompactField(text, "影响范围", "服务器健康检查未通过")
	case "memory":
		writeCompactResourceAlert(text, "内存", event, details)
	case "disk":
		label := firstNonEmptyDetail(details, "mount_point")
		if label == "" {
			label = event.Object
		}
		writeCompactResourceAlert(text, label, event, details)
	case "network":
		text.WriteString("### [告警] " + escapeMarkdown(compactNetworkLabel(event, details)) + "\n\n")
		writeCompactCurrentThreshold(text, event.Current, event.Threshold)
	case "auth":
		writeCompactChineseAuthAlert(text, event, details)
	default:
		text.WriteString("### [告警] " + escapeMarkdown(event.Object) + "\n\n")
		writeCompactCurrentThreshold(text, event.Current, event.Threshold)
	}
}

func writeCompactResourceAlert(text *strings.Builder, label string, event notification.Event, details map[string]string) {
	heading := "### [告警] " + escapeMarkdown(label)
	if strings.TrimSpace(event.Current) != "" {
		heading += "　" + escapeMarkdown(event.Current)
	}
	text.WriteString(heading + "\n\n")
	if percent, ok := parsePercentValue(event.Current); ok {
		text.WriteString(quotaProgressBar(percent) + "\n\n")
	}
	writeCompactCurrentThreshold(text, event.Current, event.Threshold)
	used, total := compactByteValue(details["used_bytes"]), compactByteValue(details["total_bytes"])
	if used != "" && total != "" {
		line := used + " / 总计 " + total
		if filesystem := strings.TrimSpace(details["filesystem_type"]); filesystem != "" {
			line += " · " + filesystem
		}
		writeCompactField(text, "已用", line)
	}
}

func writeCompactCurrentThreshold(text *strings.Builder, current, threshold string) {
	current, threshold = strings.TrimSpace(current), strings.TrimSpace(threshold)
	if current == "" && threshold == "" {
		return
	}
	var line strings.Builder
	if current != "" {
		line.WriteString("**当前** " + escapeMarkdown(current))
	}
	if threshold != "" {
		if line.Len() > 0 {
			line.WriteString(" · ")
		}
		line.WriteString("**阈值** " + escapeMarkdown(threshold))
	}
	text.WriteString(line.String() + "\n\n")
}

func writeCompactField(text *strings.Builder, label, value string) {
	value = strings.TrimSpace(value)
	if value == "" {
		return
	}
	text.WriteString("**" + escapeMarkdown(label) + "** " + escapeMarkdown(value) + "\n\n")
}

func writeCompactChineseAuthAlert(text *strings.Builder, event notification.Event, details map[string]string) {
	label := firstNonEmptyDetail(details, "email", "account", "name")
	if label == "" {
		label = "账号"
	}
	text.WriteString("### [异常] " + escapeMarkdown(label) + "\n\n")
	reasons := localizedAuthReasons(event.Current)
	for _, reason := range reasons {
		text.WriteString("**[异常]** " + escapeMarkdown(reason) + "\n\n")
	}
	if provider := firstNonEmptyDetail(details, "provider", "type"); provider != "" {
		writeCompactField(text, "提供商", provider)
	}
	if status := strings.TrimSpace(details["status"]); status != "" {
		writeCompactField(text, "当前状态", status)
	}
	if statusMessage := strings.TrimSpace(details["status_message"]); statusMessage != "" {
		writeCompactField(text, "服务返回", truncateRunes(statusMessage, 500))
	}
	writeCompactField(text, "建议", "检查账号额度、凭据或重新登录账号")
}

func writeCompactChineseRecoveryEvent(text *strings.Builder, scope string, event notification.Event) {
	details := parseEventDetails(event.Details)
	label := compactRecoveryLabel(scope, event, details)
	text.WriteString("### [恢复] " + escapeMarkdown(label) + "\n\n")
	switch scope {
	case "health":
		writeCompactField(text, "原异常", "CLIProxyAPI 健康检查失败")
		if reason := strings.TrimSpace(details["error"]); reason != "" {
			writeCompactField(text, "原失败原因", truncateRunes(reason, 500))
		}
	case "auth":
		reasons := localizedAuthReasons(details["reason"])
		if len(reasons) > 0 {
			writeCompactField(text, "原异常", strings.Join(reasons, " · "))
		}
	default:
		if original := compactOriginalValue(scope, details); original != "" {
			line := original
			if threshold := strings.TrimSpace(event.Threshold); threshold != "" {
				line += " · 阈值 " + threshold
			}
			writeCompactField(text, "原告警", line)
		}
	}
	writeCompactField(text, "当前状态", "已恢复正常")
}

func compactRecoveryLabel(scope string, event notification.Event, details map[string]string) string {
	switch scope {
	case "health":
		return "CLIProxyAPI"
	case "memory":
		return "内存"
	case "disk":
		if value := firstNonEmptyDetail(details, "mount_point"); value != "" {
			return value
		}
	case "network":
		return compactNetworkLabel(event, details)
	case "auth":
		if value := firstNonEmptyDetail(details, "email", "account", "name"); value != "" {
			return value
		}
		return "账号"
	}
	value := strings.TrimSpace(strings.TrimSuffix(event.Object, " recovered"))
	if value == "" {
		return localizedScope(scope, "zh-CN")
	}
	return value
}

func compactNetworkLabel(event notification.Event, details map[string]string) string {
	if details["kind"] == "service_port_tcp" {
		if port := strings.TrimSpace(details["service_port"]); port != "" {
			return "端口 " + port
		}
		return "服务端口连接"
	}
	if details["kind"] == "total_tcp" {
		return "TCP 总连接"
	}
	if strings.TrimSpace(event.Object) != "" {
		return event.Object
	}
	return "TCP 连接"
}

func compactOriginalValue(scope string, details map[string]string) string {
	switch scope {
	case "memory", "disk":
		return strings.TrimSpace(details["used_percent"])
	case "network":
		return strings.TrimSpace(details["connections"])
	default:
		return ""
	}
}

func parseEventDetails(value string) map[string]string {
	details := make(map[string]string)
	for _, line := range strings.Split(value, "\n") {
		key, item, ok := strings.Cut(line, "=")
		key = strings.TrimSpace(key)
		if !ok || key == "" {
			continue
		}
		details[key] = strings.TrimSpace(item)
	}
	return details
}

func firstNonEmptyDetail(details map[string]string, keys ...string) string {
	for _, key := range keys {
		if value := strings.TrimSpace(details[key]); value != "" {
			return value
		}
	}
	return ""
}

func localizedAuthReasons(value string) []string {
	localized := make([]string, 0, 3)
	seen := make(map[string]struct{}, 3)
	for _, raw := range strings.Split(value, ",") {
		reason := strings.TrimSpace(raw)
		if reason == "" {
			continue
		}
		switch reason {
		case "unavailable":
			reason = "账号不可用"
		case "quota-like status message":
			reason = "额度状态异常"
		case "non-active status":
			reason = "账号状态不是 active"
		}
		if _, exists := seen[reason]; exists {
			continue
		}
		seen[reason] = struct{}{}
		localized = append(localized, reason)
	}
	return localized
}

func parsePercentValue(value string) (float64, bool) {
	value = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(value), "%"))
	percent, err := strconv.ParseFloat(value, 64)
	if err != nil || math.IsNaN(percent) || math.IsInf(percent, 0) || percent < 0 || percent > 100 {
		return 0, false
	}
	return percent, true
}

func compactByteValue(value string) string {
	bytes, err := strconv.ParseUint(strings.TrimSpace(value), 10, 64)
	if err != nil {
		return ""
	}
	units := [...]string{"B", "KiB", "MiB", "GiB", "TiB", "PiB", "EiB"}
	size, unit := float64(bytes), 0
	for size >= 1024 && unit < len(units)-1 {
		size /= 1024
		unit++
	}
	return formatCompactPercent(size) + " " + units[unit]
}

func healthPayload(report notification.HealthReport, language string, at atContent) (markdownPayload, error) {
	if err := validateHealthReport(report); err != nil {
		return markdownPayload{}, err
	}
	if err := validateLanguage(language); err != nil {
		return markdownPayload{}, err
	}
	title := "CPA Monitor · Server Status"
	var text strings.Builder
	writeMentions(&text, at)
	if language == "zh-CN" {
		title = "CPA Monitor · 运行正常"
		writeCompactChineseHealth(&text, report, title)
	} else {
		text.WriteString("## " + title + "\n\n")
		text.WriteString("> All four server checks passed; account status and alerts are handled independently.\n\n")
		writeMarkdownField(&text, "Host", report.Hostname)
		writeMarkdownField(&text, "Checked at", report.Timestamp.UTC().Format("2006-01-02 15:04:05 UTC"))
		writeMarkdownField(&text, "Memory", fmt.Sprintf("%.1f%% used (threshold %.1f%%)", report.MemoryUsedPercent, report.MemoryThreshold))
		writeMarkdownField(&text, "Disk", fmt.Sprintf("%.1f%% highest across %d mount(s) (threshold %.1f%%)", report.HighestDiskUsedPercent, report.DiskMountCount, report.DiskThreshold))
		writeMarkdownField(&text, "TCP", fmt.Sprintf("%d total connections (threshold %d)", report.TotalTCPConnections, report.TotalTCPThreshold))
		writeMarkdownField(&text, fmt.Sprintf("Service port %d", report.ServicePort), fmt.Sprintf("%d connections (threshold %d)", report.ServicePortConnections, report.ServicePortThreshold))
		if report.AccountUsageAvailable {
			writeMarkdownField(&text, "Accounts", fmt.Sprintf("%d enabled / %d checked", report.EnabledAccountCount, report.AccountCount))
			writeAccountUsages(&text, report.AccountUsages, language, report.Timestamp)
		} else {
			writeMarkdownField(&text, "Account usage", "temporarily unavailable (account check failed; server status reporting is unaffected)")
		}
		writeMarkdownField(&text, "CLIProxyAPI base URL", report.BaseURL)
		writeMarkdownField(&text, "Next scheduled report", report.NextScheduledAt.UTC().Format("2006-01-02 15:04:05 UTC"))
		writeVersionStatus(&text, report, language)
	}
	return markdownPayload{MessageType: "markdown", Markdown: markdownContent{Title: truncateRunes(title, 100), Text: fitMarkdown(text.String(), language)}, At: at}, nil
}

func writeCompactChineseHealth(text *strings.Builder, report notification.HealthReport, title string) {
	accounts := compactAccounts(report.AccountUsages)
	attentionCount := compactAttentionCount(accounts)
	text.WriteString("## " + title + "\n\n")
	text.WriteString(fmt.Sprintf("> 服务器 4/4 正常 · 账号 %d/%d 启用\n", report.EnabledAccountCount, report.AccountCount))
	switch {
	case !report.AccountUsageAvailable:
		text.WriteString("> 账号用量暂不可用，不影响服务器状态报告\n\n")
	case len(accounts) == 0:
		text.WriteString("> 当前没有已启用账号\n\n")
	case attentionCount > 0:
		text.WriteString(fmt.Sprintf("> 用量提醒：%d 个账号周用量达到 %.0f%%\n\n", attentionCount, quotaAttentionPercent))
	default:
		text.WriteString(fmt.Sprintf("> 账号周用量均低于 %.0f%%\n\n", quotaAttentionPercent))
	}

	text.WriteString("### 服务器概览\n\n")
	text.WriteString("**" + escapeMarkdown(report.Hostname) + "**\n\n")
	text.WriteString(fmt.Sprintf("**内存** %.1f%% / %.1f%%　　**磁盘** %.1f%% / %.1f%%\n\n",
		report.MemoryUsedPercent, report.MemoryThreshold, report.HighestDiskUsedPercent, report.DiskThreshold))
	text.WriteString(fmt.Sprintf("**TCP** %d / %d　　**端口 %d** %d / %d\n",
		report.TotalTCPConnections, report.TotalTCPThreshold, report.ServicePort,
		report.ServicePortConnections, report.ServicePortThreshold))

	if report.AccountUsageAvailable {
		writeCompactChineseAccounts(text, accounts, report.Timestamp)
	}
	writeCompactChineseVersion(text, report)
	text.WriteString("\n" + markdownLink("管理面板", report.BaseURL) + "　　" + markdownLink("版本发布", report.ReleaseURL) + "\n\n")
	text.WriteString(fmt.Sprintf("检查于 %s · 下次 %s（北京时间）\n",
		formatBeijingDateTime(report.Timestamp), formatNextBeijingTime(report.Timestamp, report.NextScheduledAt)))
}

func validateHealthReport(report notification.HealthReport) error {
	if strings.TrimSpace(report.Hostname) == "" || report.Timestamp.IsZero() || report.NextScheduledAt.IsZero() {
		return errors.New("DingTalk health report requires hostname and schedule timestamps")
	}
	if strings.TrimSpace(report.BaseURL) == "" {
		return errors.New("DingTalk health report base URL is required")
	}
	if report.AccountCount < 0 || report.EnabledAccountCount < 0 || report.EnabledAccountCount > report.AccountCount {
		return errors.New("DingTalk health report account counts are invalid")
	}
	if report.AccountUsageAvailable && report.EnabledAccountCount != len(report.AccountUsages) {
		return errors.New("DingTalk health report enabled account count is invalid")
	}
	if !report.AccountUsageAvailable && (report.AccountCount != 0 || report.EnabledAccountCount != 0 || len(report.AccountUsages) != 0) {
		return errors.New("DingTalk unavailable account usage must not contain counters")
	}
	for _, usage := range report.AccountUsages {
		if notification.ValidateAccountUsage(usage) != nil {
			return errors.New("DingTalk health report account usage is invalid")
		}
	}
	if notification.ValidateVersionInfo(report) != nil {
		return errors.New("DingTalk health report version status is invalid")
	}
	return nil
}

type compactAccount struct {
	usage  notification.AccountUsage
	window *notification.QuotaWindow
	order  int
}

func compactAccounts(usages []notification.AccountUsage) []compactAccount {
	accounts := make([]compactAccount, len(usages))
	for i, usage := range usages {
		accounts[i] = compactAccount{usage: usage, window: preferredQuotaWindow(usage.QuotaWindows), order: i}
	}
	sort.SliceStable(accounts, func(i, j int) bool {
		left, leftOK := compactUsedPercent(accounts[i])
		right, rightOK := compactUsedPercent(accounts[j])
		switch {
		case leftOK != rightOK:
			return leftOK
		case leftOK && left != right:
			return left > right
		default:
			return accounts[i].order < accounts[j].order
		}
	})
	return accounts
}

func preferredQuotaWindow(windows []notification.QuotaWindow) *notification.QuotaWindow {
	priority := map[string]int{
		"weekly": 0, "monthly": 1, "secondary": 2, "five_hour": 3, "primary": 4,
	}
	best, bestPriority := -1, len(priority)+1
	for i := range windows {
		value, ok := priority[windows[i].Kind]
		if !ok {
			value = len(priority)
		}
		if value < bestPriority {
			best, bestPriority = i, value
		}
	}
	if best < 0 {
		return nil
	}
	return &windows[best]
}

func compactUsedPercent(account compactAccount) (float64, bool) {
	if account.window == nil || account.window.UsedPercent == nil {
		return 0, false
	}
	return *account.window.UsedPercent, true
}

func compactAttentionCount(accounts []compactAccount) int {
	count := 0
	for _, account := range accounts {
		if used, ok := compactUsedPercent(account); ok && used >= quotaAttentionPercent {
			count++
		}
	}
	return count
}

func writeCompactChineseAccounts(text *strings.Builder, accounts []compactAccount, checkedAt time.Time) {
	text.WriteString("\n### 账号周用量（高 → 低）\n\n")
	if len(accounts) == 0 {
		text.WriteString("暂无已启用账号\n")
		return
	}
	for i, account := range accounts {
		if i > 0 {
			text.WriteString("\n")
		}
		status := "未知"
		percentText := ""
		if used, ok := compactUsedPercent(account); ok {
			status = "正常"
			if used >= quotaAttentionPercent {
				status = "关注"
			}
			percentText = "　" + formatCompactPercent(used) + "%"
		}
		text.WriteString(fmt.Sprintf("**[%s] %s%s**\n\n", status, escapeMarkdown(account.usage.Label), percentText))
		text.WriteString(compactQuotaLine(account, checkedAt) + "\n\n")
		text.WriteString(compactRequestLine(account.usage) + "\n")
	}
}

func compactQuotaLine(account compactAccount, checkedAt time.Time) string {
	if account.window != nil && account.window.UsedPercent != nil {
		used := *account.window.UsedPercent
		line := quotaProgressBar(used) + "　剩余 " + formatCompactPercent(100-used) + "%"
		if resetAt := quotaResetAt(*account.window, checkedAt); !resetAt.IsZero() {
			line += " · " + formatBeijingDateTime(resetAt) + " 重置"
		}
		return line
	}
	if !account.usage.QuotaSupported {
		return "套餐额度暂不支持"
	}
	if !account.usage.QuotaAvailable {
		return "套餐额度暂不可用"
	}
	if account.window == nil {
		return "套餐额度暂无数据"
	}
	line := quotaWindowLabel(account.window.Kind, "zh-CN") + "用量暂不可用"
	if resetAt := quotaResetAt(*account.window, checkedAt); !resetAt.IsZero() {
		line += " · " + formatBeijingDateTime(resetAt) + " 重置"
	}
	return line
}

func compactRequestLine(usage notification.AccountUsage) string {
	recent := usage.RecentSuccess + usage.RecentFailed
	if recent == 0 {
		return "近期暂无请求"
	}
	successRate := float64(usage.RecentSuccess) / float64(recent) * 100
	return fmt.Sprintf("近期请求 %d · 成功率 %s%%", recent, formatCompactPercent(successRate))
}

func quotaProgressBar(used float64) string {
	filled := int(math.Round(used / 100 * compactQuotaProgressSteps))
	if filled < 0 {
		filled = 0
	}
	if filled > compactQuotaProgressSteps {
		filled = compactQuotaProgressSteps
	}
	return strings.Repeat("█", filled) + strings.Repeat("░", compactQuotaProgressSteps-filled)
}

func quotaResetAt(window notification.QuotaWindow, checkedAt time.Time) time.Time {
	if !window.ResetAt.IsZero() {
		return window.ResetAt
	}
	if window.ResetAfter > 0 {
		return checkedAt.Add(window.ResetAfter)
	}
	return time.Time{}
}

func formatCompactPercent(value float64) string {
	return strings.TrimSuffix(fmt.Sprintf("%.1f", value), ".0")
}

func formatBeijingDateTime(value time.Time) string {
	return value.In(beijingTime).Format("01-02 15:04")
}

func formatNextBeijingTime(checkedAt, next time.Time) string {
	checked, scheduled := checkedAt.In(beijingTime), next.In(beijingTime)
	checkedYear, checkedMonth, checkedDay := checked.Date()
	nextYear, nextMonth, nextDay := scheduled.Date()
	if checkedYear == nextYear && checkedMonth == nextMonth && checkedDay == nextDay {
		return scheduled.Format("15:04")
	}
	return scheduled.Format("01-02 15:04")
}

func writeCompactChineseVersion(text *strings.Builder, report notification.HealthReport) {
	text.WriteString("\n### CLIProxyAPI\n\n")
	if !report.VersionCheckAvailable {
		text.WriteString("版本检查暂不可用，不影响服务器状态报告\n")
		return
	}
	current, latest := displayVersion(report.CurrentVersion), displayVersion(report.LatestVersion)
	switch {
	case !report.VersionComparable:
		text.WriteString(fmt.Sprintf("当前 **%s** · 最新 **%s** · 请打开发布页面确认\n", escapeMarkdown(current), escapeMarkdown(latest)))
	case report.UpdateAvailable:
		text.WriteString(fmt.Sprintf("发现新版本：**%s → %s** · 请安排升级\n", escapeMarkdown(current), escapeMarkdown(latest)))
	default:
		text.WriteString(fmt.Sprintf("当前 **%s** · 已是最新版本\n", escapeMarkdown(current)))
	}
}

func displayVersion(value string) string {
	value = strings.TrimSpace(value)
	if value != "" && value[0] >= '0' && value[0] <= '9' {
		return "v" + value
	}
	return value
}

func markdownLink(label, target string) string {
	target = strings.NewReplacer("\\", "%5C", "(", "%28", ")", "%29", " ", "%20").Replace(strings.TrimSpace(target))
	return "[" + escapeMarkdown(label) + "](" + target + ")"
}

func writeVersionStatus(text *strings.Builder, report notification.HealthReport, language string) {
	if language == "zh-CN" {
		text.WriteString("\n### CLIProxyAPI 版本\n\n")
		if report.VersionCheckAvailable {
			writeMarkdownField(text, "当前版本", report.CurrentVersion)
			writeMarkdownField(text, "最新版本", report.LatestVersion)
			switch {
			case !report.VersionComparable:
				writeMarkdownField(text, "更新状态", "无法比较版本，请打开发布页面确认")
			case report.UpdateAvailable:
				writeMarkdownField(text, "更新状态", "发现新版本，请安排升级")
			default:
				writeMarkdownField(text, "更新状态", "已是最新版本")
			}
		} else {
			writeMarkdownField(text, "当前版本", "暂不可用")
			writeMarkdownField(text, "最新版本", "暂不可用")
			writeMarkdownField(text, "更新状态", "版本检查失败，不影响服务器状态报告")
		}
		writeMarkdownField(text, "发布地址", report.ReleaseURL)
		return
	}

	text.WriteString("\n### CLIProxyAPI version\n\n")
	if report.VersionCheckAvailable {
		writeMarkdownField(text, "Current version", report.CurrentVersion)
		writeMarkdownField(text, "Latest version", report.LatestVersion)
		switch {
		case !report.VersionComparable:
			writeMarkdownField(text, "Update status", "versions could not be compared; check the releases page")
		case report.UpdateAvailable:
			writeMarkdownField(text, "Update status", "a newer version is available")
		default:
			writeMarkdownField(text, "Update status", "up to date")
		}
	} else {
		writeMarkdownField(text, "Current version", "unavailable")
		writeMarkdownField(text, "Latest version", "unavailable")
		writeMarkdownField(text, "Update status", "version check failed; server status reporting is unaffected")
	}
	writeMarkdownField(text, "Releases", report.ReleaseURL)
}

func writeAccountUsages(text *strings.Builder, usages []notification.AccountUsage, language string, checkedAt time.Time) {
	if len(usages) == 0 {
		return
	}
	if language == "zh-CN" {
		text.WriteString("\n### 已启用账号用量\n\n")
	} else {
		text.WriteString("\n### Enabled account usage\n\n")
	}
	for _, usage := range usages {
		label := usage.Label
		if provider := strings.TrimSpace(usage.Provider); provider != "" {
			label += " (" + provider + ")"
		}
		writeMarkdownField(text, label, accountUsageText(usage, language, checkedAt))
	}
}

func accountUsageText(usage notification.AccountUsage, language string, checkedAt time.Time) string {
	lines := make([]string, 0, len(usage.QuotaWindows)+2)
	if usage.QuotaSupported {
		if usage.QuotaAvailable {
			if plan := strings.TrimSpace(usage.PlanType); plan != "" {
				if language == "zh-CN" {
					lines = append(lines, "套餐："+plan)
				} else {
					lines = append(lines, "Plan: "+plan)
				}
			}
			for _, window := range usage.QuotaWindows {
				lines = append(lines, quotaWindowText(window, language, checkedAt))
			}
		} else if language == "zh-CN" {
			lines = append(lines, "套餐额度：获取失败（不影响服务器状态报告）")
		} else {
			lines = append(lines, "Plan quota: unavailable (server status reporting is unaffected)")
		}
	}
	total, recent := usage.Success+usage.Failed, usage.RecentSuccess+usage.RecentFailed
	if language == "zh-CN" {
		lines = append(lines, fmt.Sprintf("请求统计：进程累计 %d 次（成功 %d / 失败 %d）；近期 %d 次（成功 %d / 失败 %d）", total, usage.Success, usage.Failed, recent, usage.RecentSuccess, usage.RecentFailed))
	} else {
		lines = append(lines, fmt.Sprintf("Request stats: process total %d (success %d / failed %d); recent %d (success %d / failed %d)", total, usage.Success, usage.Failed, recent, usage.RecentSuccess, usage.RecentFailed))
	}
	return strings.Join(lines, "\n")
}

func quotaWindowText(window notification.QuotaWindow, language string, checkedAt time.Time) string {
	label := quotaWindowLabel(window.Kind, language)
	usageText := "用量未知"
	if language != "zh-CN" {
		usageText = "usage unavailable"
	}
	if window.UsedPercent != nil {
		remaining := 100 - *window.UsedPercent
		if language == "zh-CN" {
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
		if language == "zh-CN" {
			usageText += "，重置 " + resetAt.UTC().Format("2006-01-02 15:04 UTC")
		} else {
			usageText += ", resets " + resetAt.UTC().Format("2006-01-02 15:04 UTC")
		}
	}
	if language == "zh-CN" {
		return label + "：" + usageText
	}
	return label + ": " + usageText
}

func quotaWindowLabel(kind, language string) string {
	labels := map[string][2]string{
		"five_hour": {"5 小时限额", "5-hour limit"},
		"weekly":    {"周限额", "Weekly limit"},
		"monthly":   {"月度限额", "Monthly limit"},
		"primary":   {"短周期限额", "Primary limit"},
		"secondary": {"长周期限额", "Secondary limit"},
	}
	label, ok := labels[kind]
	if !ok {
		label = [2]string{"套餐限额", "Plan limit"}
	}
	if language == "zh-CN" {
		return label[0]
	}
	return label[1]
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
