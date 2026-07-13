# CPA Monitor DingTalk Alert Integration Plan

> 状态：代码实现和自动化验证已完成；Task 14 真实钉钉群验收需在目标环境配置凭证后执行。
>
> 基线：钉钉自定义群机器人作为告警主通道，SMTP 可作为失败后备；同一监控 scope 的新增告警和恢复分别聚合发送，避免触发机器人限流。

## Goal

为 `cpa-monitor` 增加安全、可测试、向后兼容的钉钉通知能力，使监控条件从健康变为异常时能够通过钉钉自定义群机器人主动推送 Markdown 摘要，并可选 `@` 指定成员；条件恢复时遵循现有 `alerts.send_recovery` 语义发送恢复通知。

实施完成后应支持：

- 钉钉作为唯一告警通道；
- 钉钉作为主通道、SMTP 作为失败后备；
- 保持现有纯 SMTP 配置和行为可升级；
- 健康报告显式选择钉钉或 SMTP；
- 使用独立测试命令验证通知配置，不运行监控、不修改告警状态；
- 钉钉及可选后备通道都失败时不推进告警状态，后续周期仍可重试；
- 同一 scope 的告警聚合成摘要，控制每分钟消息数量。

## Official Platform Constraints

本计划依据 2026-07-13 核对的钉钉官方文档，实施时仍需再次检查接口是否有更新：

- 自定义机器人只能向群聊发送消息，不支持单聊：<https://open.dingtalk.com/document/dingstart/custom-bot-creation-and-installation>
- 安全设置支持关键词、HMAC-SHA256 加签和 IPv4/IP 段白名单；签名时间戳单位为毫秒，与调用时间误差不能超过一小时：<https://open.dingtalk.com/document/dingstart/customize-robot-security-settings>
- 发送端点为 `POST https://oapi.dingtalk.com/robot/send`，成功响应需满足 `errcode == 0`；每个机器人每分钟最多 20 条，超过后限流 10 分钟；监控类场景应聚合为 Markdown 摘要：<https://open.dingtalk.com/document/development/custom-robots-send-group-messages>
- Webhook 支持 Text、Markdown、ActionCard、FeedCard、Link 及 `@` 数据：<https://open.dingtalk.com/document/development/robot-message-type>
- 企业内部应用工作通知可用于个人通知，但需要应用凭证、AgentID、UserID 和异步结果查询，暂不纳入本阶段：<https://open.dingtalk.com/document/development/asynchronous-sending-of-enterprise-session-messages>

## Scope

### Included

- 自定义群机器人 Webhook 出站通知；
- 加签安全模式；
- `at_user_ids`、`at_mobiles`、`at_all`；
- 钉钉 Markdown 告警、恢复和健康报告；
- scope 级批量摘要；
- 主通道/后备通道路由；
- 条件化 SMTP 配置与向后兼容；
- 安全的环境变量覆盖；
- CLI 测试通知；
- systemd 安装器、示例配置和 README 更新；
- 单元、集成、安装和发布前验证。

### Explicitly Excluded

- 钉钉单聊或企业内部应用工作通知；
- Stream 模式、接收群消息、聊天命令和事件订阅；
- 告警确认、静默、指派、升级和互动卡片；
- 保证每个已配置渠道都必须收到同一条消息的多通道 fan-out；
- 新增数据库或外部消息队列；
- 修改 CLIProxyAPI 仓库或依赖其 Go module。

## Existing Baseline

当前实现的重要边界：

- `internal/alerter.Manager` 的 `Sender` 直接接收 `mailer.Event`，发送成功后才写入 active state；失败会在下一周期重试。
- `internal/healthreport.Manager` 直接依赖 `mailer.HealthReport` 和 `SendHealth`。
- `internal/config.Config` 无条件包含并校验 SMTP。
- `internal/app.buildRuntime` 无条件创建 SMTP mailer。
- `internal/alerter.Manager` 当前按告警 key 分别发送；大量磁盘或账户异常可能在单轮产生超过 20 条消息。
- state schema v2 已保存 active records 和健康报告调度时间；本计划不改变“成功投递后才 active”的核心语义。
- `install.sh` 当前在首次安装时强制收集 SMTP 配置，并将 SMTP 密码存入权限受限的 systemd EnvironmentFile。

## Architecture

最终数据流：

```text
monitor runner
  -> rule.Batch
  -> alerter.Manager
       -> compare active state
       -> build notification.Batch (alert/recovery, one scope)
       -> notification.Router
            -> primary sender (dingtalk or smtp)
            -> optional fallback sender when primary fails
       -> update active state only when route succeeds

healthreport.Manager
  -> notification.HealthReport
  -> configured health-report sender

notification senders
  |- dingtalk: Markdown + HMAC signature + HTTPS JSON
  `- mailer: multipart email + SMTP TLS
```

Package responsibilities：

- `internal/notification`：传输无关的事件、批次、健康报告、发送接口和主/后备路由。
- `internal/dingtalk`：Webhook 签名、Markdown 渲染、`@`、HTTP 调用、响应分类和限流冷却。
- `internal/mailer`：保留 SMTP transport，把输入类型改为 `notification` model，并支持批量邮件摘要。
- `internal/alerter`：继续负责状态机，同时把逐 key 发送改为按 scope/kind 聚合发送。
- `internal/healthreport`：继续负责调度，不认识具体通道。
- `internal/config`：决定哪些 sender 被引用并执行条件化校验。
- `internal/app`：只构造实际被引用的 sender，并组装 router。

## Decisions Included in This Approval

1. **群而非单聊。** 第一阶段要求用户创建专用内部告警群并添加自定义机器人；必须个人工作通知时另立后续项目。
2. **加签为默认安全模式。** 产品实现签名；IP 白名单由用户在钉钉侧按需叠加，程序不管理钉钉机器人配置。
3. **不接受任意 Webhook URL。** 配置只接收 Webhook `access_token` 和签名 secret，生产端点固定为钉钉官方 HTTPS 地址，避免 SSRF 和误把凭证发送到第三方域名。
4. **主通道优先、失败后备。** `alerts.primary_channel` 先发送；只有失败才调用 `alerts.fallback_channel`。任一成功即视为本次告警已送达并推进 state。
5. **后备成功不补发主通道。** 若钉钉失败而 SMTP 后备成功，告警进入 active，不在后续周期再次补发钉钉；日志必须记录主通道失败及后备成功。这是“至少一个通道送达”的语义，不是 all-channel fan-out。
6. **默认保持 SMTP。** 缺省 `alerts.primary_channel` 为 `smtp`，旧 YAML 无需新增字段即可保持当前行为。
7. **健康报告默认跟随主通道。** `health_report.channel` 为空时解析为 `alerts.primary_channel`；显式配置时使用指定通道，不使用 alert fallback。
8. **SMTP 条件化校验。** 只有告警主/后备或启用的健康报告引用 SMTP 时才要求完整 SMTP 配置。钉钉-only 模式不要求伪造 SMTP host/from/to。
9. **scope 级聚合。** 每次 `Reconcile` 对新增告警最多发送一条 alert batch，对恢复最多发送一条 recovery batch；一个完整周期理论上最多 10 条异常/恢复通知。
10. **摘要而非无限拆分。** 每条消息展示的 condition 数量受 `dingtalk.max_items` 限制，超出部分显示“另有 N 项”；batch 成功时全部 condition 都推进 state，避免为了完整展示而触发消息洪峰。
11. **成功判定严格。** 只有 HTTP 2xx、JSON 可解析且 `errcode == 0` 才算钉钉发送成功。
12. **限流冷却。** 收到 `410100` 时 sender 在内存中进入 10 分钟冷却；冷却期间直接返回 typed rate-limit error，不继续访问钉钉。重启后冷却不持久化。
13. **不做激进即时重试。** 单次发送最多发起一次 HTTP 请求；失败交给现有监控周期重试，避免扩大限流。交付语义为 at-least-once，网络在“服务端接收后、客户端读响应前”中断时仍可能产生一次重复，这是已知残余风险。
14. **v1 不使用 `msgUuid` 冒充 exactly-once。** 没有持久化 outbox 时，随机 UUID 无法跨周期解决歧义；若未来要求 exactly-once，需要单独设计冻结 payload 的持久化 outbox，再复用稳定 `msgUuid`。
15. **动态 Markdown 必须转义。** 账号状态、错误文本、挂载点、URL 等数据不得创建非预期标题、链接、图片或 `@`。
16. **Secret 不落普通 YAML。** 示例只写 env 名；安装器将 Token/Secret 写入权限受限的 EnvironmentFile，日志和错误不得包含凭证或完整签名 URL。
17. **健康报告可以发钉钉。** 但告警活跃时现有 runner 不发送健康报告，因此不会与告警洪峰叠加。
18. **state schema 保持 v2。** 本阶段不增加 per-channel delivery state；路由成功仍视为一个原子投递结果。

## Proposed Configuration

```yaml
alerts:
  primary_channel: dingtalk
  fallback_channel: smtp
  send_recovery: true
  state_file: state/alerts.json

dingtalk:
  webhook_token: ""
  webhook_token_env: CPA_DINGTALK_WEBHOOK_TOKEN
  signing_secret: ""
  signing_secret_env: CPA_DINGTALK_SIGNING_SECRET
  language: zh-CN
  timeout: 10s
  max_items: 10
  at_user_ids:
    - "user-id-placeholder"
  at_mobiles: []
  at_all: false

health_report:
  enabled: true
  channel: smtp
  interval: 24h
  retry_interval: 15m

smtp:
  host: smtp.example.com
  port: 587
  language: zh-CN
  username: ""
  username_env: CPA_SMTP_USERNAME
  password: ""
  password_env: CPA_SMTP_PASSWORD
  from: cpa-monitor@example.com
  to:
    - admin@example.com
  starttls: true
  tls: false
  timeout: 10s
```

Validation rules：

- channel 只能是 `smtp` 或 `dingtalk`；fallback 可为空，但不得等于 primary；
- 引用钉钉时 token 和 secret 都必须非空且不得包含控制字符；
- `dingtalk.language` 只能是 `zh-CN` 或 `en`；
- `dingtalk.timeout > 0`；`max_items` 建议限制在 `[1, 50]`；
- `at_all=true` 时拒绝同时配置 user IDs 或 mobiles，避免意外扩大通知范围；
- user IDs/mobiles trim、去空、去重，错误信息不回显个人值；
- webhook token 的 inline/env 规则与现有 secret 一致：环境变量只要被设置，即使为空也覆盖 inline，然后进入正常校验；
- 未引用某通道时允许省略该通道的非默认必填项；
- `health_report.enabled=false` 时不因 `health_report.channel` 引用而强制初始化 sender。

## Message Contract

告警标题：

```text
🔴 CPA Monitor 告警（3 项）
```

恢复标题：

```text
🟢 CPA Monitor 恢复（2 项）
```

健康报告标题：

```text
✅ CPA Monitor 健康报告
```

告警 Markdown 最少包含：

- 主机名；
- scope；
- 检查时间（UTC RFC3339，与邮件/state 保持一致）；
- 每项 summary、key、current、threshold；
- 经过排序和截断的 details；
- CLIProxyAPI base URL；
- 总 condition 数和被省略的数量。

Rendering rules：

- condition 按 key 排序；details 按 key 排序；
- 动态值使用独立 escape 函数，处理反斜杠、反引号、`[]()#!*_>` 和换行；
- 单个动态字段设置 byte/rune 上限，截断时使用明确后缀；
- 不在消息中放管理密钥、SMTP 凭证、Webhook Token、signing secret 或最终请求 URL；
- `@` 通过结构化 `at` 字段发送，不从 condition 文本解析；
- 钉钉使用 `dingtalk.language`，SMTP 继续使用现有 `smtp.language`；两个 renderer 都接收同一份 transport-neutral model，但各自负责本通道的本地化，不迁移或废弃旧 SMTP 字段。

## Task 1: Introduce Transport-Neutral Notification Models

**Files:**

- Create: `internal/notification/types.go`
- Create: `internal/notification/types_test.go`
- Modify: `internal/mailer/message.go`
- Modify: `internal/mailer/message_test.go`
- Modify: `internal/mailer/smtp.go`
- Modify: `internal/mailer/smtp_test.go`
- Modify: `internal/alerter/manager.go`
- Modify: `internal/alerter/manager_test.go`
- Modify: `internal/healthreport/manager.go`
- Modify: `internal/healthreport/manager_test.go`

**Steps:**

1. 先写 compile-time assertions 和 model validation 测试，定义：
   - `notification.Kind`：`ALERT`、`RECOVERY`；
   - `notification.Event`：现有 event 字段，增加 `Scope`；
   - `notification.Batch`：Kind、Scope、Hostname、Timestamp、Events；
   - `notification.HealthReport`：迁移现有健康报告字段；
   - `AlertSender.SendBatch(context.Context, notification.Batch) error`；
   - `HealthSender.SendHealth(context.Context, notification.HealthReport) error`。
2. 验证 nil context、空 kind/scope/hostname、空 events、event kind/scope 不一致和重复 key 会失败。
3. 把 mailer 的公开输入改为 notification types；必要时短暂使用 type alias 保持单个提交可编译，最终内部包不得再从 alerter/healthreport import `mailer` model。
4. 此任务中每个现有 event 先包装为只含一项的 batch，完成接口解耦但不改变 SMTP 内容、state 或发送次数；真正的 scope 聚合留到 Task 6。
5. 运行：

```bash
go test ./internal/notification ./internal/mailer ./internal/alerter ./internal/healthreport -count=1
go test ./... -count=1
```

**Expected:** 行为不变，领域模型不再由 SMTP package 所有。

## Task 2: Extend Configuration with Channels and DingTalk Secrets

**Files:**

- Modify: `internal/config/config.go`
- Modify: `internal/config/config_test.go`
- Modify: `internal/config/testdata/minimal.yaml`
- Modify: `internal/config/testdata/invalid.yaml`

**Steps:**

1. 先写失败测试覆盖：
   - 旧 minimal/example YAML 未声明 channel 时 primary 解析为 `smtp`；
   - health channel 为空时解析为 primary；
   - 钉钉-only 配置不要求 SMTP；
   - 引用 SMTP 时仍执行所有原有 SMTP 校验；
   - primary/fallback 合法值、相同值、unknown 值；
   - token/secret inline、env 未设置、env 非空覆盖、env 空值覆盖；
   - language、timeout、max_items、at_all 冲突、列表 trim/去重；
   - strict YAML unknown field；
   - 所有验证错误不包含 management key、SMTP credentials、DingTalk token/secret 或 @ 的个人值。
2. 增加 channel resolution helper，避免 app、installer 和 tests 各自解释默认值。
3. SMTP 校验拆为 `validateSMTPIfReferenced`，钉钉校验拆为 `validateDingTalkIfReferenced`。
4. 钉钉语言和 SMTP 语言分别校验；现有 `smtp.language` 的默认值和行为保持不变。
5. 运行：

```bash
go test ./internal/config -count=1
```

**Expected:** 旧配置兼容，钉钉-only 和混合通道配置均有明确校验路径。

## Task 3: Implement DingTalk Signing

**Files:**

- Create: `internal/dingtalk/sign.go`
- Create: `internal/dingtalk/sign_test.go`

**Steps:**

1. 使用固定 timestamp/secret 写官方算法的已知向量测试。
2. 测试签名字符串精确为 `timestamp + "\n" + secret`，算法为 HMAC-SHA256，随后 Base64 和 query escaping。
3. 测试 Unicode secret、特殊 Base64 字符、毫秒 timestamp、空/控制字符输入。
4. 构造 request URL 时使用 `net/url`，不得字符串拼接；产品 host/path 固定。
5. 错误仅描述字段和阶段，不包含 secret、token 或已签名 URL。
6. 运行：

```bash
go test ./internal/dingtalk -run TestSign -count=1
```

## Task 4: Render Safe DingTalk Markdown Batches

**Files:**

- Create: `internal/dingtalk/message.go`
- Create: `internal/dingtalk/message_test.go`

**Steps:**

1. 先写 alert、recovery、health report golden/structural 测试。
2. 断言批次按 key 排序、details 按 key 排序，重复运行产生相同正文。
3. 覆盖中文、英文、空 details、磁盘路径、邮箱、账号状态、换行、反引号、Markdown 链接/图片/标题注入文本。
4. 测试 `max_items`：总数保留，只展示前 N 项，并显示省略数量；不把一个 batch 自动拆成多条消息。
5. 测试单字段和整体消息预算，截断不会破坏 UTF-8。
6. 测试 `at_user_ids`、`at_mobiles`、`at_all` JSON，不把 `@` 值插入日志。
7. body 使用 typed struct 后由 `encoding/json` 编码，消息类型固定为 `markdown`，标题和正文分离。
8. 运行：

```bash
go test ./internal/dingtalk -run 'TestRender|TestMarkdown|TestAt' -count=1
```

## Task 5: Implement the DingTalk HTTP Sender

**Files:**

- Create: `internal/dingtalk/client.go`
- Create: `internal/dingtalk/client_test.go`

**Steps:**

1. 用注入的 `http.RoundTripper` 或测试专用 endpoint 写失败测试，生产构造函数仍固定官方 endpoint。
2. 覆盖：
   - POST、HTTPS endpoint、query token/timestamp/sign、`Content-Type: application/json`；
   - request body 为 Task 4 结果；
   - HTTP 2xx + `errcode=0` 成功；
   - HTTP 2xx + 非零 errcode 失败；
   - 非 2xx、空 body、畸形 JSON、超大 body；
   - context cancel、deadline、connection failure；
   - response body 总是关闭，响应读取设置小型硬上限；
   - `410100` 建立 10 分钟冷却，固定 clock 下冷却前不再次调用 transport，冷却后允许重试；
   - `310000`、`400101`、`400102`、`400106` 等错误转换为带 code 的 typed error；
   - 错误和日志不包含 token、secret、sign、完整 URL 或 request body。
3. HTTP client 使用 context + 配置 timeout；TLS 证书校验不得关闭。
4. 单次 `SendBatch` 不做内部 retry；将 retry 交给 alerter/healthreport 周期。
5. v1 不发送 `msgUuid`：当前没有跨周期持久化 outbox，也没有单次调用内 retry，随机 UUID 不能解决“服务端已接收但客户端未读到响应”的歧义；此限制必须写入 README。
6. 运行：

```bash
go test ./internal/dingtalk -count=1
go test -race ./internal/dingtalk -count=1
```

## Task 6: Batch Alert and Recovery Reconciliation

**Files:**

- Modify: `internal/alerter/manager.go`
- Modify: `internal/alerter/manager_test.go`
- Modify: `internal/mailer/message.go`
- Modify: `internal/mailer/message_test.go`

**Steps:**

1. 先把现有逐 key 测试扩展为 batch sender 测试：
   - 同一 scope 多个新增 key 只调用一次 alert batch；
   - 同一 scope 多个恢复 key 只调用一次 recovery batch；
   - 同轮既有新增又有恢复时最多两次，alert 和 recovery 分开；
   - batch 中 events 按 key 排序；
   - alert batch 成功后所有 key active；失败后没有 key active；
   - recovery batch 成功后所有 key 删除；失败后所有 key 保留；
   - `Complete=false` 可以发送已知新增 batch，但绝不恢复缺失 key；
   - ongoing conditions 不产生 batch；
   - state 每个 Reconcile 最多 Save 一次，失败语义保持当前行为。
2. 由 Manager 构造统一 UTC timestamp，一个 batch 内所有 events 使用相同时间。
3. SMTP mailer 增加 batch message renderer，使其可作为 router fallback；邮件正文列出所有 events，不逐 key 创建多封邮件。
4. 更新原有 subject/body 测试，单项 batch 仍保持清晰标题，多项显示数量和 scope。
5. 不修改 state schema，不改变 unknown/recovery 语义。
6. 运行：

```bash
go test ./internal/alerter ./internal/mailer -count=1
```

## Task 7: Add Primary/Fallback Routing

**Files:**

- Create: `internal/notification/router.go`
- Create: `internal/notification/router_test.go`

**Steps:**

1. 用 recording/failing senders 写失败测试：
   - primary 成功时不调用 fallback；
   - primary 失败、fallback 成功时整体成功；
   - 两者失败时返回 `errors.Join` 语义；
   - 无 fallback 时原样返回 primary error；
   - context 取消后不启动 fallback；
   - fallback 成功时写结构化 warn，包含 channel 名和 scope，不包含消息正文或 secret；
   - nil/重复 channel 构造失败。
2. Router 不并发调用两个通道，确保行为确定且避免重复通知。
3. channel 名由构造时注入，不从 sender 的错误字符串猜测。
4. 运行：

```bash
go test ./internal/notification -count=1
```

## Task 8: Make Health Reports Transport-Neutral

**Files:**

- Modify: `internal/healthreport/manager.go`
- Modify: `internal/healthreport/manager_test.go`
- Modify: `internal/dingtalk/message.go`
- Modify: `internal/dingtalk/message_test.go`

**Steps:**

1. 将 Manager sender 改为 `notification.HealthSender`，保留调度/state 逻辑。
2. 验证钉钉和 SMTP sender 都能发送同一个 `notification.HealthReport`。
3. 继续只在所有五个 scope 完整、无错误且无 active condition 时 eligible。
4. 发送失败更新 `LastAttemptAt` 但不更新 `LastSentAt`；重试间隔行为不变。
5. 钉钉健康消息同样应用安全转义和消息预算。
6. 运行：

```bash
go test ./internal/healthreport ./internal/dingtalk ./internal/mailer -count=1
```

## Task 9: Wire Conditional Senders into the Runtime

**Files:**

- Modify: `internal/app/app.go`
- Modify: `internal/app/app_test.go`
- Modify: `internal/app/integration_test.go`

**Steps:**

1. 先写 runtime factory 测试：
   - 旧 SMTP 配置只创建 SMTP；
   - 钉钉-only 只创建 DingTalk，不触发 SMTP 构造；
   - 钉钉 primary + SMTP fallback 按顺序构造 router；
   - health channel 跟随 primary 或使用显式 channel；
   - 未引用 sender 的无效空配置不影响启动；
   - 被引用 sender 构造失败会阻止启动，错误不泄露 secret。
2. 把 sender 构造拆为小型 factory/helper，避免 `buildRuntime` 继续膨胀。
3. Logger 在 router 和 DingTalk client 中复用现有 `slog.Logger`。
4. `--check-config` 仍不创建 runtime、不访问钉钉/SMTP/API/state。
5. 运行：

```bash
go test ./internal/app -count=1
go test ./... -count=1
```

## Task 10: Add a Safe Notification Test Command

**Files:**

- Modify: `internal/app/runtime.go`
- Modify: `internal/app/runtime_test.go`
- Modify: `cmd/cpa-monitor/main.go`
- Modify: `cmd/cpa-monitor/main_test.go`

**CLI Contract:**

```bash
cpa-monitor --config /etc/cpa-monitor/config.yaml --test-notification primary
cpa-monitor --config /etc/cpa-monitor/config.yaml --test-notification dingtalk
cpa-monitor --config /etc/cpa-monitor/config.yaml --test-notification smtp
```

**Steps:**

1. 先写 flag 测试：允许 `primary|dingtalk|smtp`，与 `--once`/`--check-config` 互斥，unknown value 返回退出码 2。
2. 测试命令只构造目标 sender，发送一条明确标注“测试”的 notification，不创建 monitor runner、不访问 CLIProxyAPI、不打开/保存 state。
3. 目标通道未配置时返回可操作错误，但不打印凭证。
4. `primary` 走真实 primary/fallback 路由；显式 `dingtalk`/`smtp` 只测试指定 sender，不触发 fallback。
5. 成功在 stdout 输出 channel 和成功状态；失败在 stderr 输出安全错误并返回 1。
6. 运行：

```bash
go test ./internal/app ./cmd/cpa-monitor -count=1
```

## Task 11: Update Installer and Systemd Secrets

**Files:**

- Modify: `install.sh`
- Modify: `tests/install_test.sh`
- Modify: `tests/bootstrap_test.sh` only if bootstrap argument coverage changes

**New first-install variables:**

```text
CPA_MONITOR_ALERT_PRIMARY_CHANNEL
CPA_MONITOR_ALERT_FALLBACK_CHANNEL
CPA_MONITOR_HEALTH_REPORT_CHANNEL
CPA_MONITOR_DINGTALK_WEBHOOK_TOKEN
CPA_MONITOR_DINGTALK_SIGNING_SECRET
CPA_MONITOR_DINGTALK_AT_USER_IDS
CPA_MONITOR_DINGTALK_AT_MOBILES
CPA_MONITOR_DINGTALK_AT_ALL
CPA_MONITOR_DINGTALK_LANGUAGE
CPA_MONITOR_DINGTALK_TIMEOUT
CPA_MONITOR_DINGTALK_MAX_ITEMS
```

**Steps:**

1. 先扩展 installer tests：
   - 非交互钉钉-only 安装不要求 SMTP variables；
   - 混合通道正确生成 YAML 和 env file；
   - 旧 SMTP-only automation variables 仍成功；
   - token/secret 只出现在 EnvironmentFile，不出现在 YAML、stdout、stderr 或 unit；
   - env file 继续保持 root-only/受限权限；
   - 逗号列表 trim、去空、去重，换行被拒绝；
   - existing config/env 未 `--force-config` 时继续保留；
   - rollback/backups 覆盖新增字段。
2. 交互式首次安装先询问 primary/fallback，再只询问实际引用通道的配置。
3. secret 使用现有不回显 prompt；脚本入口继续 `set +x` 和 `umask 077`。
4. 生成配置后通过安装 binary 的 `--check-config` 门禁。
5. 可选：安装完成、服务启动前运行显式 opt-in 的 notification test；不得默认向外发送测试消息。
6. 运行：

```bash
bash tests/install_test.sh
bash tests/bootstrap_test.sh
```

## Task 12: Add End-to-End Integration Scenarios

**Files:**

- Modify: `internal/monitor/integration_test.go`
- Modify: `internal/app/integration_test.go`
- Create: `internal/dingtalk/integration_test.go` if HTTP-level scenarios do not fit client tests

**Steps:**

1. 用 fake API、fake collector、真实 alerter/state 和 recording senders 覆盖：
   - 多 scope 同时异常产生每 scope 一个 alert batch；
   - 同 scope 30 个账号异常只发送一条摘要，state 激活全部 30 个 key；
   - 第二轮持续异常不重发；
   - 恢复批次遵守 `send_recovery`；
   - primary 失败/fallback 成功后不重发；
   - 两通道失败后没有 key active，下一轮可重试；
   - incomplete batch 不误恢复；
   - state 跨 runtime 重建仍去重；
   - 健康报告选择钉钉时不初始化 SMTP。
2. HTTP 集成使用 test-only injected endpoint/transport，禁止产品配置暴露任意 endpoint。
3. 测试 `--once`：发现异常且通知成功仍返回 0；通知全部失败返回非零；fallback 成功视为通知成功。
4. 运行：

```bash
go test ./internal/monitor -run Integration -count=1
go test ./internal/app -run Integration -count=1
go test ./internal/dingtalk -run Integration -count=1
```

## Task 13: Update Documentation and Example Configuration

**Files:**

- Modify: `config.example.yaml`
- Modify: `README.md`
- Modify: `docs/2026-07-09-cpa-monitor-design.md`
- Modify: `internal/config/example_test.go`

**Steps:**

1. example config 展示钉钉 primary + SMTP fallback，同时不包含真实 token/secret/UserID/手机号。
2. Example test 注入假的 DingTalk/SMTP env，验证真实 loader、默认值和 channel resolution。
3. README 增加：
   - 专用内部群和自定义机器人创建步骤；
   - 自定义机器人不能单聊；
   - 加签、时间同步、可选 IPv4 白名单；
   - 如何从 Webhook 提取 token；
   - `@` 成员配置；
   - 钉钉-only、SMTP-only、primary/fallback 三组 YAML；
   - `--test-notification`；
   - 每分钟 20 条、10 分钟限流和本项目聚合策略；
   - `310000`、`410100`、robot/token 常见错误排查；
   - fallback 成功后不补发 primary 的明确语义；
   - systemd env file 修改、验证、重启和回滚命令。
4. 设计文档把“SMTP email alerts”更新为 transport-neutral notifications，并保留历史行为说明。
5. 所有官方事实链接到钉钉官方文档，不把第三方教程作为规范依据。
6. 运行：

```bash
go test ./internal/config -run TestExampleConfig -count=1
```

## Task 14: Operational Acceptance on a Real DingTalk Group

此任务需要用户提供或在目标服务器配置真实机器人凭证，自动测试完成后执行：

1. 创建专用内部告警群，添加自定义机器人，安全模式选择加签。
2. 将 token/secret 写入 `/etc/cpa-monitor/cpa-monitor.env`，确认文件权限不宽于现有 installer policy。
3. 使用安装器提供的 config-check unit 验证配置和 EnvironmentFile：

```bash
sudo systemctl start cpa-monitor-check.service
sudo systemctl status cpa-monitor-check.service --no-pager
```

4. 通过临时 systemd unit 加载与正式服务相同的 EnvironmentFile，并发送显式测试消息：

```bash
sudo systemd-run --wait --pipe --collect \
  --property=User=cpa-monitor \
  --property=Group=cpa-monitor \
  --property=WorkingDirectory=/var/lib/cpa-monitor \
  --property=EnvironmentFile=/etc/cpa-monitor/cpa-monitor.env \
  /usr/local/bin/cpa-monitor \
  --config /etc/cpa-monitor/config.yaml \
  --test-notification dingtalk
```

5. 验证群内显示、中文、换行、主机名和 `@` 提醒。
6. 在受控环境制造一个临时阈值异常，确认：首次发送一次、持续异常不重发、恢复按配置发送、再次异常可以重新发送。
7. 暂时使用错误签名或测试用 disabled robot 验证 fallback 和安全日志；测试结束立即恢复正确配置。
8. 观察至少一个健康报告周期或临时缩短 interval 验证调度，然后恢复正式 interval。

## Final Verification Gate

按顺序执行，任何一步失败先修复并从相关局部测试重跑：

```bash
set -euo pipefail
export GOWORK=off

gofmt -w $(rg --files cmd internal -g '*.go')
go mod tidy
git diff --check
go test -mod=readonly ./...
go test -mod=readonly -race ./...
go vet -mod=readonly ./...
go build -mod=readonly -trimpath -o /tmp/cpa-monitor ./cmd/cpa-monitor
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
  go build -mod=readonly -trimpath -o /tmp/cpa-monitor-linux-amd64 ./cmd/cpa-monitor
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 \
  go build -mod=readonly -trimpath -o /tmp/cpa-monitor-linux-arm64 ./cmd/cpa-monitor
bash tests/install_test.sh
bash tests/bootstrap_test.sh
```

Secret/static checks：

```bash
if rg -n \
  'SEC[A-Za-z0-9_-]{8,}|access_token=[A-Za-z0-9_-]{8,}|CPA_DINGTALK_(WEBHOOK_TOKEN|SIGNING_SECRET)=.+[^"[:space:]]' \
  --glob '!docs/plans/*.md' .; then
  echo 'possible DingTalk secret committed' >&2
  exit 1
fi

if rg -n 'InsecureSkipVerify[[:space:]]*:[[:space:]]*true' --glob '*.go' .; then
  echo 'TLS verification disabled' >&2
  exit 1
fi
```

Manual review：

- 产品路径只能访问固定钉钉官方 endpoint；测试 override 不可由 YAML/env 控制。
- 所有 HTTP response body 都关闭，request/response 均有大小或时间边界。
- 日志、errors、test failure output 不包含 token、secret、sign、完整 Webhook URL 或消息正文。
- 旧 SMTP-only 配置通过真实 loader 和 runtime integration tests。
- 钉钉-only 配置不会构造或验证 SMTP。
- 一个 scope 大量条件不会拆成超过计划数量的钉钉消息。
- primary/fallback、state progression 和 `--once` 退出码语义有自动测试。
- README 中明确说明群机器人不支持单聊及 fallback 成功后的投递语义。

## Rollout and Rollback

### Rollout

1. 先完成 Task 1-2 的无行为变化/兼容层。
2. 完成 DingTalk sender 和单元测试，但默认 channel 保持 SMTP。
3. 完成 batch/router/runtime 后运行全量门禁。
4. 先在测试群或非关键主机将 primary 切到 DingTalk，SMTP 保持 fallback。
5. 观察 24-48 小时的发送成功率、fallback 触发、限流和重复情况。
6. 再逐台切换正式主机；每台执行 `--check-config` 和显式测试通知。

### Rollback

无需回退 state schema。将配置恢复为：

```yaml
alerts:
  primary_channel: smtp
  fallback_channel: ""
```

然后执行：

```bash
sudo systemctl start cpa-monitor-check.service
sudo systemctl restart cpa-monitor.service
sudo journalctl -u cpa-monitor.service -n 100 --no-pager
```

钉钉 token/secret 可留在 root-only env file 等待复用，也可以在钉钉群撤销机器人后从 env file 删除。回滚不会迁移或重写 active alert state，因此已激活告警不会因为换回 SMTP 而立即重复发送。

## Completion Criteria

只有同时满足以下条件才算完成：

- 所有 Task 的测试先于实现补齐并通过；
- Final Verification Gate 全部通过；
- 旧 SMTP-only 用户无需改 YAML；
- 钉钉-only 和钉钉 primary + SMTP fallback 在集成测试中成立；
- 真实钉钉群测试消息能送达并正确 `@`；
- 首次异常、持续异常、恢复、再次异常的状态机行为符合预期；
- 大量条件被聚合，不触发机器人每分钟 20 条限制；
- 任意失败路径均不泄露 DingTalk/SMTP/Management secrets；
- README、example config、installer help 与实际 CLI/config 行为一致。

## Change-Control Gate

计划已获批准并按既定语义实施。后续如需改变以下任一语义，应先暂停并重新确认：

- 从“至少一个通道成功”改为“每个通道都必须成功”；
- 引入 persistent outbox 或修改 state schema；
- 支持个人工作通知/Stream 模式；
- 接受用户配置的任意 Webhook endpoint；
- 默认从 SMTP 切换为 DingTalk；
- 自动向外发送安装测试消息。
