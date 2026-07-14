package mailer

import (
	"bytes"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net/mail"
	"strings"
	"testing"
	"time"

	"github.com/mokexinxin/cpa-monitor/internal/notification"
)

func TestBuildMessageCreatesRFCMessage(t *testing.T) {
	t.Parallel()

	timestamp := time.Date(2026, time.July, 9, 3, 4, 5, 0, time.FixedZone("CST", 8*60*60))
	event := Event{
		Kind:      Alert,
		Object:    "账户 codex-user@example.com 配额不可用",
		Hostname:  "monitor-01",
		Timestamp: timestamp,
		Key:       "auth:account-7",
		Current:   "unavailable",
		Threshold: "active",
		Details:   "email=codex-user@example.com\nprovider=codex\nstatus_message=额度已耗尽",
		BaseURL:   "http://127.0.0.1:8317",
	}

	raw, err := BuildMessage(
		"CPA 监控 <monitor@example.com>",
		[]string{"Admin One <one@example.com>", "two@example.com"},
		event,
	)
	if err != nil {
		t.Fatalf("BuildMessage() error = %v", err)
	}
	assertOnlyCRLF(t, raw)
	if !bytes.HasSuffix(raw, []byte("\r\n")) {
		t.Fatal("message does not end with CRLF")
	}

	msg, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("mail.ReadMessage() error = %v\n%s", err, raw)
	}
	if got := msg.Header.Get("From"); !strings.Contains(got, "monitor@example.com") {
		t.Fatalf("From = %q, want monitor@example.com", got)
	}
	if got := msg.Header.Get("To"); !strings.Contains(got, "one@example.com") || !strings.Contains(got, "two@example.com") {
		t.Fatalf("To = %q, want both recipients", got)
	}
	if got := msg.Header.Get("Message-ID"); !strings.HasPrefix(got, "<") || !strings.HasSuffix(got, "@cpa-monitor.local>") {
		t.Fatalf("Message-ID = %q, want cpa-monitor.local message id", got)
	}
	parsedDate, err := mail.ParseDate(msg.Header.Get("Date"))
	if err != nil {
		t.Fatalf("Date %q is invalid: %v", msg.Header.Get("Date"), err)
	}
	_, offset := parsedDate.Zone()
	if !parsedDate.Equal(timestamp) || offset != 0 {
		t.Fatalf("Date = %v, want timestamp represented in UTC", parsedDate)
	}
	if got, want := msg.Header.Get("MIME-Version"), "1.0"; got != want {
		t.Fatalf("MIME-Version = %q, want %q", got, want)
	}
	mediaType, params, err := mime.ParseMediaType(msg.Header.Get("Content-Type"))
	if err != nil || mediaType != "multipart/alternative" || params["boundary"] == "" {
		t.Fatalf("Content-Type = %q, err=%v, want multipart/alternative", msg.Header.Get("Content-Type"), err)
	}

	decodedSubject, err := new(mime.WordDecoder).DecodeHeader(msg.Header.Get("Subject"))
	if err != nil {
		t.Fatalf("decode Subject: %v", err)
	}
	if got, want := decodedSubject, "[CPA Monitor] 告警：账号 codex-user@example.com 状态：不可用"; got != want {
		t.Fatalf("Subject = %q, want %q", got, want)
	}
	if !strings.Contains(string(raw), "=?utf-8?") && !strings.Contains(string(raw), "=?UTF-8?") {
		t.Fatalf("raw UTF-8 Subject is not encoded: %s", raw)
	}

	parts := readAlternative(t, msg)
	body := parts["text/plain"]
	for _, want := range []string{
		"事件: 告警",
		"主机: monitor-01",
		"时间: 2026-07-08T19:04:05Z",
		"告警键: auth:account-7",
		"当前值: 不可用",
		"阈值: 活动",
		"技术详情:\r\nemail=codex-user@example.com\r\nprovider=codex\r\nstatus_message=额度已耗尽",
		"CLIProxyAPI 地址: http://127.0.0.1:8317",
	} {
		if !bytes.Contains(body, []byte(want)) {
			t.Errorf("body does not contain %q:\n%s", want, body)
		}
	}
	htmlBody := string(parts["text/html"])
	for _, want := range []string{"CPA MONITOR", "告警", "monitor-01", "auth:account-7", "CLIProxyAPI"} {
		if !strings.Contains(htmlBody, want) {
			t.Errorf("HTML body does not contain %q", want)
		}
	}
}

func TestBuildHealthMessageCreatesAccessibleHTMLAndPlainFallback(t *testing.T) {
	t.Parallel()
	report := validHealthReport()
	report.Hostname = `host-01<script>alert("x")</script>`
	raw, err := BuildHealthMessage("monitor@example.com", []string{"admin@example.com"}, report)
	if err != nil {
		t.Fatal(err)
	}
	assertOnlyCRLF(t, raw)
	msg, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	decodedSubject, err := new(mime.WordDecoder).DecodeHeader(msg.Header.Get("Subject"))
	if err != nil {
		t.Fatal(err)
	}
	if decodedSubject != "[CPA Monitor] 服务器状态报告："+report.Hostname {
		t.Fatalf("Subject = %q", decodedSubject)
	}
	parts := readAlternative(t, msg)
	plain := string(parts["text/plain"])
	for _, want := range []string{"状态: 健康 - 服务器四项检查均已通过", "内存: 已使用 42.5%", "服务端口 8317: 11 个连接", "账号: 已启用 2 个 / 已检查 3 个", "账号用量 one@example.test (codex): 套餐 plus；5 小时限额：已用 12.5%", "周限额：已用 47.2%，剩余 52.8%", "请求统计：进程累计 15 次（成功 12 / 失败 3）"} {
		if !strings.Contains(plain, want) {
			t.Errorf("plain body does not contain %q:\n%s", want, plain)
		}
	}
	htmlBody := string(parts["text/html"])
	for _, want := range []string{"健康", "服务器系统运行正常", "内存", "最高磁盘使用率", "TCP 连接总数", "端口 8317", "已启用账号用量", "one@example.test (codex)", "5 小时限额", "周限额", "进程累计 15 次", "下次计划报告"} {
		if !strings.Contains(htmlBody, want) {
			t.Errorf("HTML body does not contain %q", want)
		}
	}
	if strings.Contains(htmlBody, `<script>alert`) || !strings.Contains(htmlBody, `&lt;script&gt;`) {
		t.Fatalf("dynamic hostname was not HTML escaped:\n%s", htmlBody)
	}
	if !strings.Contains(htmlBody, "#166534") || !strings.Contains(htmlBody, "健康") {
		t.Fatal("health status must use both visible text and accessible high-contrast color")
	}
	if !strings.Contains(htmlBody, `<html lang="zh-CN">`) {
		t.Fatal("Chinese HTML language metadata is missing")
	}
}

func TestBuildHealthMessageAllowsUnavailableAccountUsage(t *testing.T) {
	t.Parallel()
	report := validHealthReport()
	report.AccountUsageAvailable = false
	report.AccountCount = 0
	report.EnabledAccountCount = 0
	report.AccountUsages = nil

	raw, err := BuildHealthMessage("monitor@example.com", []string{"admin@example.com"}, report)
	if err != nil {
		t.Fatal(err)
	}
	msg, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	parts := readAlternative(t, msg)
	for contentType, body := range parts {
		if !strings.Contains(string(body), "账号用量") || !strings.Contains(string(body), "暂不可用") {
			t.Fatalf("%s body did not explain unavailable account usage", contentType)
		}
	}
}

func TestBuildHealthMessageKeepsServerReportWhenCodexQuotaFails(t *testing.T) {
	t.Parallel()
	report := validHealthReport()
	report.AccountUsages[0].PlanType = ""
	report.AccountUsages[0].QuotaAvailable = false
	report.AccountUsages[0].QuotaWindows = nil

	raw, err := BuildHealthMessage("monitor@example.com", []string{"admin@example.com"}, report)
	if err != nil {
		t.Fatal(err)
	}
	msg, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	parts := readAlternative(t, msg)
	for contentType, body := range parts {
		text := string(body)
		if !strings.Contains(text, "套餐额度获取失败") || !strings.Contains(text, "请求统计") {
			t.Fatalf("%s body did not preserve account diagnostics after quota failure", contentType)
		}
	}
}

func TestBuildMessageRecoverySubject(t *testing.T) {
	t.Parallel()

	event := validEvent()
	event.Kind = Recovery
	event.Object = "disk / recovered"
	raw, err := BuildMessage("monitor@example.com", []string{"admin@example.com"}, event)
	if err != nil {
		t.Fatalf("BuildMessage() error = %v", err)
	}
	msg, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("mail.ReadMessage() error = %v", err)
	}
	decodedSubject, err := new(mime.WordDecoder).DecodeHeader(msg.Header.Get("Subject"))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := decodedSubject, "[CPA Monitor] 恢复：内存使用率已恢复"; got != want {
		t.Fatalf("Subject = %q, want %q", got, want)
	}
}

func TestBuildMessagesInEnglish(t *testing.T) {
	t.Parallel()
	raw, err := BuildMessageInLanguage("monitor@example.com", []string{"admin@example.com"}, validEvent(), LanguageEnglish)
	if err != nil {
		t.Fatal(err)
	}
	msg, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := msg.Header.Get("Subject"), "[CPA Monitor] ALERT memory usage 84.2% on host"; got != want {
		t.Fatalf("Subject = %q, want %q", got, want)
	}
	parts := readAlternative(t, msg)
	if !strings.Contains(string(parts["text/plain"]), "Event: ALERT") || !strings.Contains(string(parts["text/html"]), "Details") {
		t.Fatal("English alert message was not localized")
	}

	raw, err = BuildHealthMessageInLanguage("monitor@example.com", []string{"admin@example.com"}, validHealthReport(), LanguageEnglish)
	if err != nil {
		t.Fatal(err)
	}
	msg, err = mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := msg.Header.Get("Subject"), "[CPA Monitor] SERVER STATUS monitor-01"; got != want {
		t.Fatalf("Subject = %q, want %q", got, want)
	}
	parts = readAlternative(t, msg)
	if !strings.Contains(string(parts["text/plain"]), "Status: HEALTHY - all four server checks passed") || !strings.Contains(string(parts["text/html"]), "Server systems are operating normally") {
		t.Fatal("English health message was not localized")
	}
	if !strings.Contains(string(parts["text/plain"]), "Account usage one@example.test (codex): plan plus; 5-hour limit: 12.5% used, 87.5% remaining") || !strings.Contains(string(parts["text/plain"]), "weekly limit: 47.2% used, 52.8% remaining") || !strings.Contains(string(parts["text/html"]), "Enabled account usage") {
		t.Fatal("English account usage was not rendered")
	}
	if !strings.Contains(string(parts["text/html"]), `<html lang="en">`) {
		t.Fatal("English HTML language metadata is missing")
	}
}

func TestBuildMessagesRejectUnsupportedLanguage(t *testing.T) {
	t.Parallel()
	if _, err := BuildMessageInLanguage("monitor@example.com", []string{"admin@example.com"}, validEvent(), "fr"); err == nil {
		t.Fatal("alert message accepted unsupported language")
	}
	if _, err := BuildHealthMessageInLanguage("monitor@example.com", []string{"admin@example.com"}, validHealthReport(), "fr"); err == nil {
		t.Fatal("health message accepted unsupported language")
	}
}

func TestBuildMessageRejectsHeaderInjection(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		from  string
		to    []string
		event Event
	}{
		{name: "from", from: "monitor@example.com\r\nBcc: stolen@example.com", to: []string{"admin@example.com"}, event: validEvent()},
		{name: "recipient", from: "monitor@example.com", to: []string{"admin@example.com\nBcc: stolen@example.com"}, event: validEvent()},
		{name: "subject object", from: "monitor@example.com", to: []string{"admin@example.com"}, event: func() Event {
			e := validEvent()
			e.Object = "memory\r\nBcc: stolen@example.com"
			return e
		}()},
		{name: "subject tab", from: "monitor@example.com", to: []string{"admin@example.com"}, event: func() Event {
			e := validEvent()
			e.Object = "memory\tusage"
			return e
		}()},
		{name: "localized key", from: "monitor@example.com", to: []string{"admin@example.com"}, event: func() Event {
			e := validEvent()
			e.Key = "resource:disk:/\r\nBcc: stolen@example.com"
			return e
		}()},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if _, err := BuildMessage(tt.from, tt.to, tt.event); err == nil {
				t.Fatal("BuildMessage() error = nil, want header injection error")
			}
		})
	}
}

func TestBuildMessageValidatesEvent(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		mutate func(*Event)
	}{
		{name: "kind", mutate: func(e *Event) { e.Kind = Kind("UNKNOWN") }},
		{name: "object", mutate: func(e *Event) { e.Object = "" }},
		{name: "hostname", mutate: func(e *Event) { e.Hostname = "" }},
		{name: "timestamp", mutate: func(e *Event) { e.Timestamp = time.Time{} }},
		{name: "key", mutate: func(e *Event) { e.Key = "" }},
		{name: "base URL", mutate: func(e *Event) { e.BaseURL = "" }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			event := validEvent()
			tt.mutate(&event)
			if _, err := BuildMessage("monitor@example.com", []string{"admin@example.com"}, event); err == nil {
				t.Fatal("BuildMessage() error = nil, want validation error")
			}
		})
	}
}

func validEvent() Event {
	return Event{
		Kind:      Alert,
		Object:    "memory usage 84.2% on host",
		Hostname:  "monitor-01",
		Timestamp: time.Date(2026, time.July, 9, 3, 4, 5, 0, time.UTC),
		Key:       "resource:memory",
		Current:   "84.2%",
		Threshold: "80.0%",
		Details:   "used=842 MiB total=1000 MiB",
		BaseURL:   "http://127.0.0.1:8317",
	}
}

func validHealthReport() HealthReport {
	return HealthReport{
		Hostname:               "monitor-01",
		Timestamp:              time.Date(2026, time.July, 11, 3, 4, 5, 0, time.UTC),
		NextScheduledAt:        time.Date(2026, time.July, 12, 3, 4, 5, 0, time.UTC),
		BaseURL:                "http://127.0.0.1:8317",
		MemoryUsedPercent:      42.5,
		MemoryThreshold:        80,
		HighestDiskUsedPercent: 51.2,
		DiskMountCount:         2,
		DiskThreshold:          80,
		TotalTCPConnections:    19,
		TotalTCPThreshold:      3000,
		ServicePort:            8317,
		ServicePortConnections: 11,
		ServicePortThreshold:   800,
		AccountUsageAvailable:  true,
		AccountCount:           3,
		EnabledAccountCount:    2,
		AccountUsages: []notification.AccountUsage{
			{Label: "one@example.test", Provider: "codex", PlanType: "plus", QuotaSupported: true, QuotaAvailable: true, QuotaWindows: []notification.QuotaWindow{
				{Kind: "five_hour", UsedPercent: testQuotaPercent(12.5), ResetAfter: 10 * time.Minute},
				{Kind: "weekly", UsedPercent: testQuotaPercent(47.25), ResetAt: time.Unix(1784000000, 0).UTC()},
			}, Success: 12, Failed: 3, RecentSuccess: 3, RecentFailed: 1},
			{Label: "team-two", Provider: "claude", Success: 4, RecentFailed: 1},
		},
	}
}

func testQuotaPercent(value float64) *float64 { return &value }

func readAlternative(t *testing.T, msg *mail.Message) map[string][]byte {
	t.Helper()
	mediaType, params, err := mime.ParseMediaType(msg.Header.Get("Content-Type"))
	if err != nil || mediaType != "multipart/alternative" {
		t.Fatalf("Content-Type = %q, err=%v", msg.Header.Get("Content-Type"), err)
	}
	reader := multipart.NewReader(msg.Body, params["boundary"])
	result := make(map[string][]byte)
	for {
		part, err := reader.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		partType, _, err := mime.ParseMediaType(part.Header.Get("Content-Type"))
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(quotedprintable.NewReader(part))
		if err != nil {
			t.Fatal(err)
		}
		result[partType] = body
	}
	if result["text/plain"] == nil || result["text/html"] == nil {
		t.Fatalf("multipart alternatives = %#v", result)
	}
	return result
}

func assertOnlyCRLF(t *testing.T, data []byte) {
	t.Helper()
	for i, b := range data {
		switch b {
		case '\n':
			if i == 0 || data[i-1] != '\r' {
				t.Fatalf("message contains bare LF at byte %d", i)
			}
		case '\r':
			if i+1 >= len(data) || data[i+1] != '\n' {
				t.Fatalf("message contains bare CR at byte %d", i)
			}
		}
	}
}
