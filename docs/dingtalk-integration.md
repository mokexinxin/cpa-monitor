# CPA Monitor 钉钉接入与运维指南

本文说明如何把 CPA Monitor 的告警、恢复通知和健康报告接入钉钉自定义群机器人，并在生产环境中安全地配置、验证、排障、轮换和回滚。

## 1. 接入能力与边界

CPA Monitor 支持以下通知拓扑：

- 仅钉钉；
- 仅 SMTP，兼容已有配置；
- 钉钉主通道、SMTP 故障后备；
- SMTP 主通道、钉钉故障后备；
- 健康报告跟随告警主通道，或显式选择钉钉/SMTP。

钉钉接入使用自定义群机器人 Webhook。它向机器人所在群发送消息，不是个人单聊接口。如果必须向个人工作通知推送，需要另行接入企业内部应用，不属于当前实现范围。

CPA Monitor 固定请求以下官方地址，不允许从配置指定任意 HTTP 地址：

```text
POST https://oapi.dingtalk.com/robot/send
```

请求使用 HMAC-SHA256 加签，消息格式为 Markdown。只有 HTTP 状态为 2xx 且响应 `errcode` 为 `0` 才视为成功。

## 2. 前置条件

准备以下条件：

1. 一个专用的企业内部监控告警群；
2. 有权限向该群添加自定义机器人；
3. CPA Monitor 所在服务器可以通过 HTTPS 访问 `oapi.dingtalk.com:443`；
4. 服务器时间已通过 NTP/chrony 等方式同步；
5. 如果使用 IP 白名单，服务器具有确定的公网出口 IPv4 或网段。

建议使用专用告警群，避免普通聊天消息淹没告警，也便于限制机器人和群成员权限。

## 3. 创建钉钉自定义机器人

按照钉钉官方的[创建自定义机器人](https://open.dingtalk.com/document/dingstart/custom-bot-creation-and-installation)流程操作：

1. 进入目标钉钉群的群设置；
2. 进入机器人管理并添加自定义机器人；
3. 设置容易识别的名称，例如 `CPA Monitor`；
4. 安全设置选择“加签”；
5. 保存 Webhook 和加签密钥。

推荐至少启用“加签”。如果同时使用关键词安全规则，建议把关键词设置为：

```text
CPA Monitor
```

CPA Monitor 生成的钉钉标题均包含该文本。

如果同时启用 IP 白名单，应填写服务器的公网出口地址，而不是服务器的内网地址。经过 NAT、云防火墙或代理访问钉钉时，以钉钉实际看到的出口 IP 为准。

钉钉 Webhook 通常类似：

```text
https://oapi.dingtalk.com/robot/send?access_token=xxxxxxxx
```

只复制 `access_token` 参数值，不要把整个 Webhook URL 写进配置。还需要保存机器人安全设置页面生成的加签密钥，通常以 `SEC` 开头。

## 4. 安全保存凭证

不要把 token 或 secret 提交到 Git，也不要直接写入可被普通用户读取的 YAML。推荐使用 systemd `EnvironmentFile`。

安装后的默认文件是：

```text
/etc/cpa-monitor/cpa-monitor.env
```

示例内容：

```ini
CPA_MANAGEMENT_KEY="replace-with-management-key"
CPA_DINGTALK_WEBHOOK_TOKEN="access-token-value-only"
CPA_DINGTALK_SIGNING_SECRET="SECxxxxxxxx"
```

这是 systemd 环境文件语法，不是 shell 脚本，不要执行 `source /etc/cpa-monitor/cpa-monitor.env`。

设置权限：

```bash
sudo chown root:root /etc/cpa-monitor/cpa-monitor.env
sudo chmod 600 /etc/cpa-monitor/cpa-monitor.env
```

对应 YAML 只保存环境变量名称：

```yaml
dingtalk:
  webhook_token: ""
  webhook_token_env: CPA_DINGTALK_WEBHOOK_TOKEN
  signing_secret: ""
  signing_secret_env: CPA_DINGTALK_SIGNING_SECRET
```

环境变量存在时会覆盖同名的 YAML 内联值，包括环境变量被设置为空字符串的情况。

## 5. 配置方式

### 5.1 仅使用钉钉

```yaml
alerts:
  send_recovery: true
  state_file: /var/lib/cpa-monitor/state/alerts.json
  primary_channel: dingtalk
  fallback_channel: ""

health_report:
  enabled: true
  interval: 24h
  retry_interval: 15m
  channel: "" # 空值表示跟随 dingtalk 主通道

dingtalk:
  webhook_token: ""
  webhook_token_env: CPA_DINGTALK_WEBHOOK_TOKEN
  signing_secret: ""
  signing_secret_env: CPA_DINGTALK_SIGNING_SECRET
  language: zh-CN
  timeout: 10s
  max_items: 10
  at_user_ids: []
  at_mobiles: []
  at_all: false
```

该模式不要求配置 `smtp`。

### 5.2 钉钉主通道，SMTP 后备

```yaml
alerts:
  send_recovery: true
  state_file: /var/lib/cpa-monitor/state/alerts.json
  primary_channel: dingtalk
  fallback_channel: smtp

health_report:
  enabled: true
  interval: 24h
  retry_interval: 15m
  channel: ""

dingtalk:
  webhook_token_env: CPA_DINGTALK_WEBHOOK_TOKEN
  signing_secret_env: CPA_DINGTALK_SIGNING_SECRET
  language: zh-CN
  timeout: 10s
  max_items: 10
  at_user_ids: []
  at_mobiles: []
  at_all: false

smtp:
  host: smtp.example.com
  port: 587
  language: zh-CN
  username_env: CPA_SMTP_USERNAME
  password_env: CPA_SMTP_PASSWORD
  from: cpa-monitor@example.com
  to:
    - admin@example.com
  starttls: true
  tls: false
  timeout: 10s
```

主通道失败后才尝试后备通道。SMTP 后备发送成功即视为本次通知已送达，CPA Monitor 会推进告警状态，不会在钉钉恢复后补发同一次告警。

### 5.3 健康报告单独使用 SMTP

```yaml
alerts:
  primary_channel: dingtalk
  fallback_channel: smtp

health_report:
  enabled: true
  channel: smtp
```

健康报告使用一个明确通道，不继承告警的 fallback。上例中健康报告 SMTP 失败时，会按照 `retry_interval` 重试，但不会转发到钉钉。

### 5.4 `@` 指定成员

```yaml
dingtalk:
  at_user_ids:
    - user-id-1
    - user-id-2
  at_mobiles:
    - "13800000000"
  at_all: false
```

- `at_user_ids`：钉钉用户 ID 列表；
- `at_mobiles`：群成员绑定的手机号列表，建议使用 YAML 字符串；
- `at_all`：是否 `@所有人`。

列表加载时会去除首尾空白、空项和重复项。实际 `@` 是否生效还取决于机器人能力、目标用户是否在群内以及群权限，应使用显式测试通知验收。

不建议默认启用 `at_all`，否则持续故障和恢复通知可能造成不必要打扰。

## 6. 钉钉配置字段

| 字段 | 默认值 | 说明 |
| --- | --- | --- |
| `webhook_token` | 空 | Webhook 的 `access_token` 值；生产环境建议保持为空 |
| `webhook_token_env` | 空 | token 对应的环境变量名称 |
| `signing_secret` | 空 | 加签密钥；生产环境建议保持为空 |
| `signing_secret_env` | 空 | secret 对应的环境变量名称 |
| `language` | `zh-CN` | `zh-CN` 或 `en` |
| `timeout` | `10s` | 单次 HTTP 请求超时，必须大于零 |
| `max_items` | `10` | Markdown 中最多展开的条件数，范围 `1`–`50` |
| `at_user_ids` | `[]` | 要 `@` 的用户 ID |
| `at_mobiles` | `[]` | 要 `@` 的手机号 |
| `at_all` | `false` | 是否 `@所有人` |

`max_items` 只控制消息中展开多少条详情，不影响告警状态。即使同一批有更多条件，成功发送后所有条件的 key 都会进入去重状态。Markdown 总长度还会限制在 18,000 个字符以内，超出部分会明确标记为已截断。

## 7. 安装器接入

首次非交互安装可以使用以下安装器输入变量：

```text
CPA_MONITOR_ALERT_PRIMARY_CHANNEL=dingtalk
CPA_MONITOR_ALERT_FALLBACK_CHANNEL=smtp
CPA_MONITOR_HEALTH_REPORT_CHANNEL=
CPA_MONITOR_DINGTALK_WEBHOOK_TOKEN=...
CPA_MONITOR_DINGTALK_SIGNING_SECRET=...
CPA_MONITOR_DINGTALK_AT_USER_IDS=user-id-1,user-id-2
CPA_MONITOR_DINGTALK_AT_MOBILES=13800000000
CPA_MONITOR_DINGTALK_AT_ALL=false
CPA_MONITOR_DINGTALK_LANGUAGE=zh-CN
CPA_MONITOR_DINGTALK_TIMEOUT=10s
CPA_MONITOR_DINGTALK_MAX_ITEMS=10
```

其中 `CPA_MONITOR_DINGTALK_*` 是安装器输入变量；安装器生成的运行时环境文件使用：

```text
CPA_DINGTALK_WEBHOOK_TOKEN
CPA_DINGTALK_SIGNING_SECRET
```

安装器会做到：

- 钉钉-only 安装不要求 SMTP 参数；
- token/secret 只写入权限为 `0600` 的环境文件；
- token/secret 不写入 YAML、systemd unit 或安装输出；
- 安装前使用实际二进制执行 `--check-config`；
- 不会默认向外发送测试消息。

如果使用配置管理系统，优先准备 YAML 和 root-only 环境文件，然后使用 `--config` 与 `--env-file` 安装，避免把真实密钥写进 shell 历史或命令参数。

## 8. 配置校验

修改 YAML 或环境文件后先校验：

```bash
sudo systemctl start cpa-monitor-check.service
sudo systemctl status cpa-monitor-check.service --no-pager --full
sudo journalctl -u cpa-monitor-check.service -n 100 --no-pager
```

也可以在已经正确设置环境变量的开发终端运行：

```bash
./cpa-monitor --config config.yaml --check-config
```

`--check-config` 只解析和验证配置，不访问钉钉、SMTP、CLIProxyAPI，也不打开或修改告警状态文件。

## 9. 发送测试通知

测试主通道以及后备路由：

```bash
cpa-monitor --config config.yaml --test-notification primary
```

只测试钉钉，不触发 fallback：

```bash
cpa-monitor --config config.yaml --test-notification dingtalk
```

只测试 SMTP：

```bash
cpa-monitor --config config.yaml --test-notification smtp
```

显式通道必须被当前配置引用，否则命令会报“channel is not configured”。测试命令不会访问 CLIProxyAPI，不会运行监控采集，也不会读取或写入告警 state。

在已安装服务器上，为了加载与正式服务相同的 EnvironmentFile，可使用临时 systemd unit：

```bash
sudo systemd-run --wait --pipe --collect \
  --unit=cpa-monitor-notification-test \
  --property=User=cpa-monitor \
  --property=Group=cpa-monitor \
  --property=WorkingDirectory=/var/lib/cpa-monitor \
  --property=EnvironmentFile=/etc/cpa-monitor/cpa-monitor.env \
  /usr/local/bin/cpa-monitor \
  --config /etc/cpa-monitor/config.yaml \
  --test-notification dingtalk
```

该命令会真实向钉钉群发送一条明确标注为测试的消息。不要把它加入自动安装或定时任务。

测试通过后重启正式服务：

```bash
sudo systemctl restart cpa-monitor.service
sudo systemctl status cpa-monitor.service --no-pager --full
sudo journalctl -u cpa-monitor.service -n 100 --no-pager
```

如果使用 timer 模式，重启 `cpa-monitor.timer`，并按需手动启动一次 `cpa-monitor-once.service`。

## 10. 发送、聚合和重试语义

### 告警聚合

每个完整监控周期包含五个 scope：API 健康、内存、磁盘、TCP 和账号。CPA Monitor 将同一 scope、同一类型的事件聚合为一个通知：

- 新增异常聚合成一个 alert batch；
- 恢复项聚合成一个 recovery batch；
- 一个周期最多产生 5 个告警批次和 5 个恢复批次。

这避免大量账号或磁盘同时异常时逐条轰炸钉钉机器人。

### 去重与后备

状态变化规则如下：

1. 新异常不在 active state 时才尝试发送；
2. 主通道成功：写入 active state；
3. 主通道失败、fallback 成功：同样写入 active state；
4. 主通道和 fallback 都失败：不写入 active state，下个周期重试；
5. 持续异常不重复发送；
6. 完整检查确认恢复后，从 active state 删除；
7. `send_recovery: true` 时，先成功发送恢复通知再删除；
8. 不完整或未知的检查结果不会被当作恢复。

当前版本没有持久化 outbox。fallback 成功后不会在主通道恢复时补发。

### 限流保护

钉钉自定义机器人按机器人限制消息频率。CPA Monitor 通过 scope 聚合减少请求量；收到 `410100` 时会进入十分钟本地冷却，在冷却结束前不继续请求该机器人。

冷却期间发送会返回错误。如果配置了 fallback，告警会尝试后备通道；没有 fallback 时，该批次保持可重试状态。

## 11. 常见问题排查

| 现象或错误 | 可能原因 | 排查方法 |
| --- | --- | --- |
| `dingtalk webhook token must not be empty` | 环境变量未加载、名称写错或值为空 | 检查 YAML 的 `webhook_token_env` 和 systemd EnvironmentFile |
| `dingtalk signing secret must not be empty` | secret 未配置 | 检查机器人是否启用加签以及 `signing_secret_env` |
| API `310000` | 关键词、加签、时间戳、IP 白名单等安全规则不满足 | 检查机器人安全设置、系统时钟、出口 IP、token 和 secret |
| API `410100` | 机器人被限流 | 等待十分钟冷却，检查是否存在其他程序共用同一机器人 |
| HTTP 4xx | token 无效、机器人被删除、请求被安全策略拒绝 | 重新获取 Webhook，确认 access token 只复制参数值 |
| HTTP 5xx 或超时 | 钉钉服务、DNS、代理、防火墙或网络异常 | 检查 `oapi.dingtalk.com:443`、DNS 和出站策略；必要时使用 SMTP fallback |
| 测试 `primary` 成功但群里没有消息 | 主通道失败后 SMTP fallback 成功 | 使用 `--test-notification dingtalk` 单独测试钉钉 |
| 消息到群但没有正确 `@` | 用户不在群、ID/手机号不匹配或群权限限制 | 检查 `at_user_ids`/`at_mobiles` 并发送显式测试消息 |
| 持续异常没有重复消息 | 正常去重行为 | 查看 state 和日志；恢复后再次异常才会重新通知 |
| 健康报告没有走 fallback | 设计行为 | `health_report.channel` 是单一通道；调整显式通道或修复该通道 |

查看服务日志：

```bash
sudo journalctl -u cpa-monitor.service -n 200 --no-pager
```

错误日志会保留 HTTP 状态、钉钉业务错误码和必要的阶段信息，但会脱敏 token、secret 及其 URL 编码形式。

## 12. 凭证轮换

建议在以下情况轮换机器人凭证：

- Webhook 或 secret 可能泄漏；
- 群管理员或运维人员发生重大变更；
- 机器人被删除或迁移到新群；
- 安全策略要求定期轮换。

轮换步骤：

1. 在钉钉创建新机器人或重新生成安全配置；
2. 更新 `/etc/cpa-monitor/cpa-monitor.env`；
3. 保持文件 `root:root 0600`；
4. 运行 `cpa-monitor-check.service`；
5. 显式执行一次 `--test-notification dingtalk`；
6. 重启正式服务；
7. 确认新机器人正常后删除旧机器人或旧凭证。

不要在日志、工单、聊天或截图中粘贴完整 Webhook。

## 13. 回滚到 SMTP

如果钉钉暂时不可用，可回滚为 SMTP-only：

```yaml
alerts:
  primary_channel: smtp
  fallback_channel: ""

health_report:
  channel: smtp
```

确认 `smtp` 配置有效，然后执行：

```bash
sudo systemctl start cpa-monitor-check.service
sudo systemctl restart cpa-monitor.service
sudo journalctl -u cpa-monitor.service -n 100 --no-pager
```

切换通道不会重写 active alert state，因此已经处于 active 的持续异常不会因为回滚而立即重复发送。需要保留钉钉配置以便稍后恢复时，可以继续把 token/secret 放在 root-only 环境文件；不再使用时应删除并撤销机器人。

## 14. 上线验收清单

- [ ] 使用专用企业内部告警群；
- [ ] 机器人启用了加签安全模式；
- [ ] token/secret 只存在于受限环境文件或密钥系统；
- [ ] `/etc/cpa-monitor/cpa-monitor.env` 为 `root:root 0600`；
- [ ] 服务器时间同步正常；
- [ ] `cpa-monitor-check.service` 校验通过；
- [ ] `--test-notification dingtalk` 能送达目标群；
- [ ] `at_user_ids`、`at_mobiles` 或 `at_all` 的实际效果符合预期；
- [ ] 主通道失败时 fallback 行为符合预期；
- [ ] 首次异常发送一次，持续异常不重发；
- [ ] 恢复和再次异常的状态机符合 `send_recovery` 配置；
- [ ] 日志中没有 token、secret 或完整 Webhook；
- [ ] 已记录轮换负责人和回滚流程。

## 15. 官方资料

- [创建自定义机器人](https://open.dingtalk.com/document/dingstart/custom-bot-creation-and-installation)
- [自定义机器人安全设置](https://open.dingtalk.com/document/dingstart/customize-robot-security-settings)
- [机器人回复/发送消息](https://open.dingtalk.com/document/dingstart/robot-reply-and-send-messages)
- [自定义机器人发送群消息](https://open.dingtalk.com/document/development/custom-robots-send-group-messages)
- [机器人消息类型](https://open.dingtalk.com/document/development/robot-message-type)

仓库内的完整示例见 [`config.example.yaml`](../config.example.yaml)，实现及测试范围见 [`DingTalk Alert Integration Plan`](plans/2026-07-13-dingtalk-alert-integration.md)。
