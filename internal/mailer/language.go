package mailer

import (
	"fmt"
	"strings"
)

const (
	LanguageChinese = "zh-CN"
	LanguageEnglish = "en"
)

func validLanguage(language string) bool {
	return language == LanguageChinese || language == LanguageEnglish
}

func localizedKind(kind Kind, language string) string {
	if language == LanguageEnglish {
		return string(kind)
	}
	if kind == Recovery {
		return "恢复"
	}
	return "告警"
}

func localizedObject(event Event, language string) string {
	if language == LanguageEnglish {
		return event.Object
	}
	recovered := event.Kind == Recovery
	var object string
	switch {
	case event.Key == "health:cliproxy_down":
		object = "CLIProxyAPI 服务状态"
	case event.Key == "resource:memory":
		object = "内存使用率"
	case strings.HasPrefix(event.Key, "resource:disk:"):
		object = "磁盘 " + strings.TrimPrefix(event.Key, "resource:disk:") + " 使用率"
	case event.Key == "network:total_tcp":
		object = "TCP 连接总数"
	case strings.HasPrefix(event.Key, "network:service_port:"):
		object = "服务端口 " + strings.TrimPrefix(event.Key, "network:service_port:") + " TCP 连接数"
	case strings.HasPrefix(event.Key, "auth:"):
		identity := firstDetailValue(event.Details, "email", "account", "name", "auth_index")
		if identity == "" {
			identity = strings.TrimPrefix(event.Key, "auth:")
		}
		object = "账号 " + identity + " 状态"
	default:
		object = event.Object
		if recovered {
			object = strings.TrimSuffix(object, " recovered")
		}
	}
	if recovered {
		return object + "已恢复"
	}
	if event.Current != "" {
		return fmt.Sprintf("%s：%s", object, localizedCurrent(event.Current, language))
	}
	return object
}

func localizedCurrent(value, language string) string {
	if language == LanguageEnglish {
		return value
	}
	replacer := strings.NewReplacer(
		"quota-like status message", "疑似配额耗尽",
		"non-active status", "非活动状态",
		"unavailable", "不可用",
		"disabled", "已禁用",
		"recovered", "已恢复",
		"down", "宕机",
	)
	return replacer.Replace(value)
}

func localizedThreshold(value, language string) string {
	if language == LanguageEnglish {
		return value
	}
	switch value {
	case "healthy":
		return "健康"
	case "active":
		return "活动"
	case "active and available":
		return "活动且可用"
	default:
		return value
	}
}

func firstDetailValue(details string, keys ...string) string {
	values := make(map[string]string)
	for _, line := range strings.Split(details, "\n") {
		key, value, ok := strings.Cut(strings.TrimSuffix(line, "\r"), "=")
		if ok && value != "" && !containsHeaderControl(value) {
			values[key] = value
		}
	}
	for _, key := range keys {
		if values[key] != "" {
			return values[key]
		}
	}
	return ""
}
