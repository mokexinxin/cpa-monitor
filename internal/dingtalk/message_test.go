package dingtalk

import (
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/mokexinxin/cpa-monitor/internal/notification"
)

func TestAlertPayloadLocalizesAndLimitsItems(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 13, 1, 2, 3, 0, time.UTC)
	batch := notification.Batch{Kind: notification.Alert, Scope: "disk", Hostname: "host-a", Timestamp: now}
	for _, key := range []string{"disk:/", "disk:/data"} {
		batch.Events = append(batch.Events, notification.Event{
			Kind: notification.Alert, Scope: "disk", Hostname: "host-a", Timestamp: now,
			Key: key, Object: "disk *value*", Current: "90%", Threshold: "80%", Details: "line\n# heading", BaseURL: "http://127.0.0.1:8317",
		})
	}
	payload, err := alertPayload(batch, "zh-CN", 1, atContent{Mobiles: []string{"13800000000"}})
	if err != nil {
		t.Fatal(err)
	}
	if payload.MessageType != "markdown" || payload.Markdown.Title != "CPA Monitor · 告警 · 磁盘" {
		t.Fatalf("payload = %#v", payload)
	}
	for _, want := range []string{"@13800000000", "disk \\*value\\*", "另有 1 项", "CLIProxyAPI"} {
		if !strings.Contains(payload.Markdown.Text, want) {
			t.Errorf("markdown missing %q:\n%s", want, payload.Markdown.Text)
		}
	}
	if !strings.Contains(payload.Markdown.Text, "line<br>\\# heading") {
		t.Fatalf("multiline details were not rendered safely:\n%s", payload.Markdown.Text)
	}
}

func TestAlertPayloadEnforcesMarkdownBudget(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()
	batch := notification.Batch{Kind: notification.Alert, Scope: "accounts", Hostname: "host", Timestamp: now}
	for i := 0; i < 50; i++ {
		batch.Events = append(batch.Events, notification.Event{
			Kind: notification.Alert, Scope: batch.Scope, Hostname: batch.Hostname, Timestamp: now,
			Key: fmt.Sprintf("auth:%d", i), Object: strings.Repeat("x", 1500), Details: strings.Repeat("y", 1500),
		})
	}
	payload, err := alertPayload(batch, "zh-CN", 50, atContent{})
	if err != nil {
		t.Fatal(err)
	}
	if got := utf8.RuneCountInString(payload.Markdown.Text); got > maxMarkdownRunes {
		t.Fatalf("markdown runes = %d, limit %d", got, maxMarkdownRunes)
	}
	if !strings.Contains(payload.Markdown.Text, "消息内容已截断") {
		t.Fatal("truncated payload has no explanation")
	}
}

func TestHealthPayload(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 13, 1, 2, 3, 0, time.UTC)
	payload, err := healthPayload(notification.HealthReport{
		Hostname: "host-a", Timestamp: now, NextScheduledAt: now.Add(time.Hour), BaseURL: "http://127.0.0.1:8317",
		MemoryUsedPercent: 10, MemoryThreshold: 80, DiskMountCount: 2, HighestDiskUsedPercent: 20, DiskThreshold: 80,
		TotalTCPConnections: 3, TotalTCPThreshold: 3000, ServicePort: 8317, ServicePortConnections: 2,
		ServicePortThreshold: 800, AccountUsageAvailable: true, AccountCount: 4, EnabledAccountCount: 2,
		VersionCheckAvailable: true, CurrentVersion: "v7.2.70", LatestVersion: "v7.2.74", VersionComparable: true, UpdateAvailable: true,
		ReleaseURL: "https://github.com/router-for-me/CLIProxyAPI/releases",
		AccountUsages: []notification.AccountUsage{
			{Label: "one@example.test", Provider: "codex", PlanType: "plus", QuotaSupported: true, QuotaAvailable: true, QuotaWindows: []notification.QuotaWindow{
				{Kind: "five_hour", UsedPercent: testPercent(12.5), ResetAfter: 10 * time.Minute},
				{Kind: "weekly", UsedPercent: testPercent(47.25), ResetAt: time.Unix(1784000000, 0).UTC()},
			}, Success: 12, Failed: 3, RecentSuccess: 2, RecentFailed: 1},
			{Label: "team-two", Provider: "claude", Success: 4},
		},
	}, "en", atContent{})
	if err != nil {
		t.Fatal(err)
	}
	if payload.Markdown.Title != "CPA Monitor · Server Status" || !strings.Contains(payload.Markdown.Text, "All four server checks passed") || !strings.Contains(payload.Markdown.Text, "2 enabled / 4 checked") || !strings.Contains(payload.Markdown.Text, "one@example.test (codex)") || !strings.Contains(payload.Markdown.Text, "5-hour limit: 12.5% used, 87.5% remaining") || !strings.Contains(payload.Markdown.Text, "Weekly limit: 47.2% used, 52.8% remaining") || !strings.Contains(payload.Markdown.Text, "process total 15 (success 12 / failed 3); recent 3 (success 2 / failed 1)") || !strings.Contains(payload.Markdown.Text, "Current version**：v7.2.70") || !strings.Contains(payload.Markdown.Text, "Latest version**：v7.2.74") || !strings.Contains(payload.Markdown.Text, "newer version is available") {
		t.Fatalf("payload = %#v", payload)
	}
}

func TestChineseHealthPayloadIsCompactSortedAndEmojiFree(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 14, 13, 53, 37, 0, time.UTC)
	weekly := func(used float64, resetAt time.Time) []notification.QuotaWindow {
		return []notification.QuotaWindow{{Kind: "weekly", UsedPercent: testPercent(used), ResetAt: resetAt}}
	}
	payload, err := healthPayload(notification.HealthReport{
		Hostname: "easy-bird-2.localdomain", Timestamp: now, NextScheduledAt: now.Add(time.Hour), BaseURL: "https://ai.harrycloud.top",
		MemoryUsedPercent: 9.6, MemoryThreshold: 80, DiskMountCount: 2, HighestDiskUsedPercent: 5.6, DiskThreshold: 80,
		TotalTCPConnections: 156, TotalTCPThreshold: 3000, ServicePort: 443, ServicePortConnections: 27,
		ServicePortThreshold: 800, AccountUsageAvailable: true, AccountCount: 5, EnabledAccountCount: 4,
		VersionCheckAvailable: true, CurrentVersion: "7.2.74", LatestVersion: "v7.2.74", VersionComparable: true,
		ReleaseURL: "https://github.com/router-for-me/CLIProxyAPI/releases",
		AccountUsages: []notification.AccountUsage{
			{Label: "arnie.ai@icloud.com", Provider: "codex", QuotaSupported: true, QuotaAvailable: true, QuotaWindows: weekly(27, time.Date(2026, 7, 21, 6, 11, 0, 0, time.UTC)), RecentSuccess: 6068, RecentFailed: 54},
			{Label: "logan.wu.ai@gmail.com", Provider: "codex", QuotaSupported: true, QuotaAvailable: true, QuotaWindows: weekly(78, time.Date(2026, 7, 21, 5, 1, 0, 0, time.UTC)), RecentSuccess: 13},
			{Label: "mokexinxin@icloud.com", Provider: "codex", QuotaSupported: true, QuotaAvailable: true, QuotaWindows: weekly(5, time.Date(2026, 7, 20, 3, 32, 0, 0, time.UTC))},
			{Label: "mokexinxin@gmail.com", Provider: "codex", QuotaSupported: true, QuotaAvailable: true, QuotaWindows: weekly(75, time.Date(2026, 7, 20, 0, 57, 0, 0, time.UTC))},
		},
	}, "zh-CN", atContent{})
	if err != nil {
		t.Fatal(err)
	}
	if payload.Markdown.Title != "CPA Monitor · 运行正常" {
		t.Fatalf("title = %q", payload.Markdown.Title)
	}
	for _, want := range []string{
		"服务器 4/4 正常 · 账号 4/5 启用",
		"用量提醒：2 个账号周用量达到 75%",
		"**内存** 9.6% / 80.0%　　**磁盘** 5.6% / 80.0%",
		"**TCP** 156 / 3000　　**端口 443** 27 / 800",
		"**[关注] logan.wu.ai@gmail.com　78%**",
		"████████░░　剩余 22% · 07-21 13:01 重置",
		"近期请求 13 · 成功率 100%",
		"**[正常] arnie.ai@icloud.com　27%**",
		"近期请求 6122 · 成功率 99.1%",
		"当前 **v7.2.74** · 已是最新版本",
		"[管理面板](https://ai.harrycloud.top)",
		"[版本发布](https://github.com/router-for-me/CLIProxyAPI/releases)",
		"检查于 07-14 21:53 · 下次 22:53（北京时间）",
	} {
		if !strings.Contains(payload.Markdown.Text, want) {
			t.Errorf("markdown missing %q:\n%s", want, payload.Markdown.Text)
		}
	}
	positions := []int{
		strings.Index(payload.Markdown.Text, "logan.wu.ai@gmail.com"),
		strings.Index(payload.Markdown.Text, "mokexinxin@gmail.com"),
		strings.Index(payload.Markdown.Text, "arnie.ai@icloud.com"),
		strings.Index(payload.Markdown.Text, "mokexinxin@icloud.com"),
	}
	for i, position := range positions {
		if position < 0 || (i > 0 && position <= positions[i-1]) {
			t.Fatalf("accounts are not sorted by weekly usage: positions=%v\n%s", positions, payload.Markdown.Text)
		}
	}
	if strings.ContainsAny(payload.Markdown.Text, "✅⚠️🟡🟢🖥👤🔄") {
		t.Fatalf("compact report contains an emoji:\n%s", payload.Markdown.Text)
	}
	for _, old := range []string{"进程累计", "CLIProxyAPI 地址", "下次计划报告"} {
		if strings.Contains(payload.Markdown.Text, old) {
			t.Fatalf("compact report retained old verbose field %q:\n%s", old, payload.Markdown.Text)
		}
	}
}

func testPercent(value float64) *float64 { return &value }

func TestAccountUsageTextKeepsRequestStatsWhenQuotaFails(t *testing.T) {
	t.Parallel()
	text := accountUsageText(notification.AccountUsage{
		Label: "one@example.test", Provider: "codex", QuotaSupported: true,
		Success: 3, Failed: 1,
	}, "zh-CN", time.Now())
	for _, want := range []string{"套餐额度：获取失败", "不影响服务器状态报告", "请求统计：进程累计 4 次"} {
		if !strings.Contains(text, want) {
			t.Fatalf("account usage text missing %q: %s", want, text)
		}
	}
}

func TestCompactChineseAccountMarksUnavailableQuotaUnknown(t *testing.T) {
	t.Parallel()
	account := compactAccount{usage: notification.AccountUsage{
		Label: "quota-unavailable@example.test", Provider: "codex", QuotaSupported: true,
		RecentSuccess: 9, RecentFailed: 1,
	}}
	var text strings.Builder
	writeCompactChineseAccounts(&text, []compactAccount{account}, time.Now())
	if !strings.Contains(text.String(), "**[未知] quota-unavailable@example.test**") || !strings.Contains(text.String(), "套餐额度暂不可用") || !strings.Contains(text.String(), "成功率 90%") {
		t.Fatalf("unexpected unavailable quota rendering:\n%s", text.String())
	}
	if strings.Contains(text.String(), "[正常]") {
		t.Fatalf("unavailable quota was marked normal:\n%s", text.String())
	}
}

func TestHealthPayloadReportsUnavailableAccountUsageWithoutFailingServerStatus(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 14, 1, 2, 3, 0, time.UTC)
	payload, err := healthPayload(notification.HealthReport{
		Hostname: "host-a", Timestamp: now, NextScheduledAt: now.Add(time.Hour), BaseURL: "http://127.0.0.1:8317",
		MemoryUsedPercent: 10, MemoryThreshold: 80, DiskMountCount: 2, HighestDiskUsedPercent: 20, DiskThreshold: 80,
		TotalTCPConnections: 3, TotalTCPThreshold: 3000, ServicePort: 8317, ServicePortConnections: 2,
		ServicePortThreshold: 800, ReleaseURL: "https://github.com/router-for-me/CLIProxyAPI/releases",
	}, "zh-CN", atContent{})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"运行正常", "服务器 4/4 正常", "账号用量暂不可用", "不影响服务器状态报告", "CLIProxyAPI", "版本检查暂不可用", "github.com/router-for-me/CLIProxyAPI/releases", "北京时间"} {
		if !strings.Contains(payload.Markdown.Text, want) {
			t.Fatalf("markdown missing %q:\n%s", want, payload.Markdown.Text)
		}
	}
}

func TestLocalizedScopeUsesMonitorScopeNames(t *testing.T) {
	t.Parallel()
	for scope, want := range map[string]string{
		"health": "健康检查", "memory": "内存", "disk": "磁盘", "network": "TCP 连接", "auth": "账号",
	} {
		if got := localizedScope(scope, "zh-CN"); got != want {
			t.Errorf("localizedScope(%q) = %q, want %q", scope, got, want)
		}
	}
}
