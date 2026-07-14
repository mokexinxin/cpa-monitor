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
	for _, want := range []string{"服务器状态报告", "服务器四项监控检查均已通过", "账号用量", "暂不可用", "不影响服务器状态报告", "CLIProxyAPI 版本", "版本检查失败", "github.com/router-for-me/CLIProxyAPI/releases"} {
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
