package dingtalk

import (
	"errors"
	"fmt"
	"math"
	"sort"
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
