# CPA Monitor Implementation Plan

> 状态：等待审批。审批前只创建本计划，不创建 Go module、不编写产品代码。
>
> 说明：当前会话没有提供 `$superpowers:writing-plans`，本计划采用等价的细粒度、测试先行写法。每项任务都先写失败测试，再写最小实现，最后运行局部与全量门禁。

## Goal

在独立的 `cpa-monitor` 仓库建立 Go 项目，通过公开 HTTP 接口监控 CLIProxyAPI 健康和账户状态，并直接从 Linux `/proc`/`statfs` 采集资源数据。项目不得修改、导入或通过 `replace`/相对路径依赖 CLIProxyAPI 源码或其 module。

## Architecture

数据流保持单向：

```text
config
  ├─> collector (memory / disk / TCP)
  ├─> cliproxy (health / auth-files HTTP)
  └─> monitor runner
          └─> rule conditions
                  └─> alerter reconciliation
                          ├─> mailer
                          └─> state JSON

stdout/stderr <─ slog ─> optional bounded logfile writer
```

- `cmd/cpa-monitor` 只做参数、信号、依赖组装和退出码。
- `internal/app` 提供可测试的 CLI/运行模式入口。
- `internal/monitor` 编排一轮检查和串行 daemon 循环。
- `internal/collector` 公开纯解析器及 Linux 实现；非 Linux 提供明确 unsupported 错误，保证 macOS 可构建和运行非 Linux 测试。
- `internal/cliproxy` 只认识 HTTP JSON wire contract，不引用 CLIProxyAPI 源码。
- `internal/rule` 把可信事实变成稳定告警条件，不发送、不持久化。
- `internal/alerter` 实现 scope-aware 状态机，确保 unknown 不被解释为 recovery。
- `internal/state` 保存带 schema version 的 active alert JSON，并采用同目录临时文件加原子 rename。
- `internal/mailer` 负责 RFC 兼容消息和 SMTP STARTTLS/direct TLS。
- `internal/logfile` 实现严格的单文件、备份数和总大小限制。

## Tech Stack and Boundaries

- Go module：`module github.com/mokexinxin/cpa-monitor`，`go 1.26.0`；构建环境使用 Go 1.26 或更新版本。
- YAML：固定稳定的 `go.yaml.in/yaml/v3 v3.0.4`，不采用仍处于 RC 的 v4。
- Linux `statfs`：固定 `golang.org/x/sys v0.47.0`，使用其 `unix` package。
- HTTP、日志、JSON、SMTP、TLS、测试均优先使用 Go 标准库；不引入 SMTP/轮转框架。
- 不初始化 Git 仓库，除非用户另行要求；当前目录不是 Git 仓库，因此计划不包含 commit 步骤。
- 不新增数据库；唯一持久化是告警状态 JSON 和可选日志文件。
- 已记录 CLIProxyAPI 实施前只读基线：`main...origin/main [ahead 1]`，worktree/index/untracked 均为空；实施结束只做同样的只读对照。

所有命令先执行：

```bash
cd /path/to/cpa-monitor
go version
export GOWORK=off
```

## Decisions Included in This Approval

1. daemon 启动后立即检查一次，随后按 `interval` 串行执行；慢检查不会并发重叠。`--once` 恰好执行一次。
2. `--once` 在完成所有可执行检查后，对 collector、Management API、SMTP 或 state 运行错误返回非零；发现异常且告警处理成功不算程序执行失败。daemon 记录同类错误并继续下一轮。
3. 配置新增 `cliproxy.timeout` 和 `smtp.timeout`，默认均为 `10s`，避免 HTTP/SMTP 永久阻塞。
4. management key 必须非空；SMTP host/from/to 必须有效。SMTP auth 可不配置，但 username/password 必须同时出现或同时为空。`starttls` 与 `tls` 必须二选一，不提供明文 SMTP，也不会在明文连接上传 AUTH。
5. 环境变量只要被设置（即使值为空）就覆盖 inline secret；空值随后接受正常校验，不在错误或日志中打印 secret。
6. `service_port: 0` 从 base URL 推导；显式端口优先，无端口的 HTTP/HTTPS 分别为 80/443，其他 scheme 配置失败。
7. `/healthz` 网络失败或非 200 是可信的 down condition；Management API 或 host collector 失败是 unknown，不得清除相应 scope 的 active alert。
8. 磁盘 statfs 部分失败时仍处理成功挂载点的新告警，但把 disk batch 标为 incomplete，因此这一轮不发任何缺失 disk key 的恢复邮件。
9. `unavailable == true` 不受 disabled 影响；disabled 只屏蔽 quota-like `status_message` 和 non-active `status` 条件。status trim 后只把大小写不敏感的 `active` 视为健康，不扩展 `ready`/`ok` 同义词。
10. `auth_index` 接受 JSON string 或 number 并规范化为字符串。异常账户缺失 index 时记录 entry error、标记 auth batch incomplete 并跳过该条，不制造不符合设计的 fallback key。
11. TCP 按设计统计 `/proc/net/tcp` 和 `/proc/net/tcp6` 中所有状态条目；磁盘使用量为 `(Blocks-Bfree)/Blocks`。
12. 告警首次发送成功后才 active；恢复邮件开启时，恢复发送成功后才删除 active。发送失败下轮重试。
13. state 落盘失败不回滚内存状态，进程内仍去重；重启后可能重发。损坏/不可读 state 记录错误并从空内存状态继续，优先保持监控可用；daemon 继续运行，`--once` 完成检查后返回非零。
14. state/log 相对路径按进程当前工作目录解析；部署示例会显式设置 `WorkingDirectory`。
15. `max_files` 表示轮转备份数，不含当前日志；总大小包含当前文件和所有受管理备份。
16. 同一轮多个账户或挂载异常按稳定 key 分别发送邮件，并按 key 排序以保证确定性。
17. 示例中未列入正式 Defaults 段落的值在本项目中明确采用为默认值：SMTP port 587、STARTTLS true，文件日志 single/backups/total 为 `20 MiB/5/80 MiB`（文件日志仍默认 disabled）。

## Task 1: Establish the Independent Module and Dependency Guard

**Files:**

- Create: `go.mod`
- Create: `cmd/cpa-monitor/main.go`

**Steps:**

1. 先在 `GOWORK=off` 下运行 `go env GOMOD`，确认当前输出 `/dev/null`，即尚无 module，也不会受父目录 Go workspace 注入影响。
2. 创建只含 canonical GitHub module path 和 Go version 的最小 `go.mod`，不添加 `replace` 或业务依赖；创建可编译空入口。
3. 运行 `go test ./...`，确认最小 module 可独立构建。
4. 运行：

```bash
go mod tidy
go list -m all
go mod edit -json
```

**Expected:** module graph 此时只有 `cpa-monitor`，`Replace` 为空；后续依赖只在实际 import 它们的任务中固定。

## Task 2: Parse, Default, Override, and Validate Configuration

**Files:**

- Create: `internal/config/config.go`
- Create: `internal/config/duration.go`
- Create: `internal/config/config_test.go`
- Create: `internal/config/testdata/minimal.yaml`
- Create: `internal/config/testdata/invalid.yaml`

**Steps:**

1. 先写表驱动失败测试，覆盖：
   - `interval: 60s` 及两个 timeout 的 duration 解析。
   - 设计文档全部默认值，以及日志 `20/5/80`、SMTP port 587、STARTTLS true。
   - YAML unknown field 失败并指出字段路径。
   - 三个 secret 的 inline、env 未设置、env 非空覆盖、env 空值覆盖。
   - base URL 尾斜杠、IPv4、IPv6、显式端口、HTTP/HTTPS 默认端口。
   - interval > 0；memory/disk 阈值 `(0,100]`；TCP 阈值 > 0；port 范围有效。
   - management key、SMTP host/from/to、auth pair、TLS 二选一模式、日志 level/size 校验；启用文件日志时三个上限均为正数，且 total size 不小于 single-file size。
   - 错误字符串不包含 management key、SMTP username/password 的实际值。
2. 运行 `go test ./internal/config -count=1`，确认 undefined API/断言失败。
3. 在 `go.mod` 固定 `go.yaml.in/yaml/v3 v3.0.4`，实现 `Load(path)`、`LoadWithEnv(path, lookupEnv)`、strict YAML decoder、默认值、环境覆盖、校验和 `ServicePort()`。
4. 再运行局部测试并执行 `go test ./...`。

**Expected:** 配置错误 fail fast；example 所需默认值全部集中在一个位置。

## Task 3: Implement Pure Memory Parsing and the Host Collector Boundary

**Files:**

- Create: `internal/collector/types.go`
- Create: `internal/collector/memory.go`
- Create: `internal/collector/memory_test.go`
- Create: `internal/collector/testdata/meminfo-valid.txt`
- Create: `internal/collector/testdata/meminfo-invalid.txt`

**Steps:**

1. 先写 `ParseMemInfo(io.Reader)` 失败测试：正常值、字段乱序、额外字段/空白、缺字段、错误单位、零 total、available > total、数值溢出。
2. 精确断言 bytes 和 `(total-available)/total*100`，不依赖宿主 `/proc`。
3. 运行：

```bash
go test ./internal/collector -run 'TestParseMemInfo' -count=1
```

4. 写最小 parser 和 `MemoryUsage` model；错误包含字段名和行号。
5. 定义可替换的 `HostCollector` 接口，为后续 Linux/unsupported 实现建立边界。
6. 重跑局部测试。

## Task 4: Parse mountinfo and Collect Real Disk Usage

**Files:**

- Create: `internal/collector/mountinfo.go`
- Create: `internal/collector/disk.go`
- Create: `internal/collector/disk_test.go`
- Create: `internal/collector/statfs_linux.go`
- Create: `internal/collector/statfs_unsupported.go`
- Create: `internal/collector/testdata/mountinfo-valid.txt`
- Create: `internal/collector/testdata/mountinfo-invalid.txt`

**Steps:**

1. 先写失败测试：
   - 找到 mountinfo 的 ` - ` 分隔符并读取 mount point/filesystem type。
   - 解码 `\\040`、`\\011`、`\\012`、`\\134`。
   - 文档列出的每一种 pseudo filesystem 都被过滤，真实块设备和网络文件系统保留。
   - 重复 mount point 去重且输出排序稳定。
   - 注入 fake statfs 后正确计算 total/used/percent；zero blocks 明确报错。
   - 单个 statfs 失败返回其他成功结果、挂载级错误和 `Complete=false`。
2. 运行 disk/mount 测试，确认失败。
3. 实现纯 parser、skip set 和注入式 statfs collector。
4. 在 `go.mod` 固定 `golang.org/x/sys v0.47.0`；在 `//go:build linux` 文件中接入 `unix.Statfs`，非 Linux adapter 返回 `ErrUnsupportedPlatform`。
5. 运行：

```bash
go test ./internal/collector -run 'TestParseMountInfo|TestCollectDisks' -count=1
GOOS=linux GOARCH=amd64 go test -c -o /tmp/cpa-monitor-collector.test ./internal/collector
```

**Expected:** macOS 可测试纯逻辑；Linux 实现可交叉编译；部分错误不会制造假恢复。

## Task 5: Parse IPv4/IPv6 TCP Tables

**Files:**

- Create: `internal/collector/tcp.go`
- Create: `internal/collector/tcp_test.go`
- Create: `internal/collector/testdata/tcp4.txt`
- Create: `internal/collector/testdata/tcp6.txt`
- Create: `internal/collector/host_linux.go`
- Create: `internal/collector/host_unsupported.go`

**Steps:**

1. 先写失败测试：跳过 header、解析 local hex port、合并 v4/v6、统计所有 TCP states、只计指定 local port、空表、0/65535 端口、畸形行带行号错误。
2. 运行 TCP 测试确认失败。
3. 实现 `ParseTCP(io.Reader, servicePort)`，再完成 `HostCollector` 的 Linux 文件读取：`/proc/meminfo`、`/proc/self/mountinfo`、`/proc/net/tcp`、`/proc/net/tcp6`。非 Linux 构造出的 collector 对每项返回 `ErrUnsupportedPlatform`；任何所需文件/格式失败使相应 batch unknown。
4. 运行：

```bash
go test ./internal/collector -run 'TestParseTCP|TestCollectTCP' -count=1
```

## Task 6: Build the Standalone CLIProxyAPI HTTP Client

**Files:**

- Create: `internal/cliproxy/types.go`
- Create: `internal/cliproxy/client.go`
- Create: `internal/cliproxy/client_test.go`

**Steps:**

1. 用 `httptest.Server` 先写失败测试：
   - `GET /healthz` 只有 200 健康；不要求解析 body。
   - 非 200、timeout、connection error 返回可用于 down condition 的安全错误。
   - Management 请求准确发送 `Authorization: Bearer <key>`。
   - 解析顶层 `files`，容忍未知 JSON 字段；`auth_index` string/number 规范化。
   - base URL 尾斜杠及可选 path prefix 不产生双斜杠/错误路径。
   - 禁止 redirect，避免 management key 被带到非预期 endpoint。
   - response body 总是关闭；context cancel 生效；Management body 有 8 MiB 上限。
   - Management 非 200、畸形 JSON、缺失 `files`、`files` 类型错误均返回 unknown/check error；空数组才是可信的“没有账户”。
   - HTTP/JSON 错误绝不包含 key。
2. 运行 `go test ./internal/cliproxy -count=1` 确认失败。
3. 实现只依赖 `net/http`/`encoding/json` 的 client 和 DTO；不用 CLIProxyAPI package 或复制其内部类型。
4. 重跑测试。

## Task 7: Convert Facts into Stable Rule Conditions

**Files:**

- Create: `internal/rule/types.go`
- Create: `internal/rule/rule.go`
- Create: `internal/rule/rule_test.go`

**Steps:**

1. 先写表驱动失败测试：
   - health 固定 key `health:cliproxy_down`。
   - memory/disk/TCP 在 `>=` 阈值时触发，低于时不触发。
   - disk key 为 `resource:disk:<mount-point>`，detail 含 type/used/total/percent。
   - network key 精确为 `network:total_tcp` 和 `network:service_port:<port>`。
   - auth 覆盖 unavailable、`quota`、`usage limit`、`limit reached`、`exhausted`、`额度`、`限额`、非 active status；英文关键词额外覆盖全大写、首字母大写、混合大小写和嵌入文本。
   - disabled + unavailable 仍告警；disabled + 仅 quota/non-active status 无 condition；active 比较 trim + case-insensitive。
   - auth detail 包含设计要求身份字段；key 精确为 `auth:<auth_index>`。
   - 缺失/重复 auth index 返回 entry error 并把 batch 标为 incomplete，不发送碰撞邮件。
   - 条件按 key 排序；同一 batch 不允许重复 key。
2. 运行 rule 测试确认失败。
3. 实现 `Condition`、`Batch{Scope, Complete, Conditions}` 及纯规则函数。
4. 重跑局部测试。

**Expected:** healthy/unhealthy/unknown 三态通过 `Complete` 显式表达，不能靠“空 slice”猜测。

## Task 8: Persist Versioned Alert State Atomically

**Files:**

- Create: `internal/state/file.go`
- Create: `internal/state/file_test.go`

**Steps:**

1. 先写失败测试：
   - state 不存在时加载空集合。
   - JSON round trip 保留 version、scope、key、原始 condition 和 activated timestamp。
   - 自动创建 `0750` 父目录，state/temp 为 `0600`。
   - temp 与目标同目录；成功 rename 后无 temp 残留。
   - 注入 write/rename failure 时旧 state 完整保留。
   - 成功或失败路径都清理本次创建的 temp file，连续失败不累积垃圾文件。
   - JSON 输出确定、带换行、active keys 排序稳定。
   - 损坏或未知 schema version 返回错误和空 state，由 app 决定记录后继续。
2. 运行 state 测试确认失败。
3. 实现 in-memory store、load 和原子 save；在可支持的平台对文件和目录执行 `Sync`。
4. 运行 `go test ./internal/state -count=1`。

## Task 9: Construct Mail and Send via SMTP

**Files:**

- Create: `internal/mailer/message.go`
- Create: `internal/mailer/smtp.go`
- Create: `internal/mailer/message_test.go`
- Create: `internal/mailer/smtp_test.go`

**Steps:**

1. 先写消息构造失败测试：From/To/Date/Message-ID/Subject/MIME/CRLF、多收件人、UTF-8 subject、header injection 拒绝。
2. 断言 alert/recovery body 包含 hostname、UTC RFC3339 timestamp、key、current、threshold、账户或 mount detail、base URL。
3. 再用本地 fake SMTP server/可注入 dialer 写传输失败测试：STARTTLS、direct TLS、可选 AUTH、每个 RCPT、context/timeout、各协议阶段错误；配置层另测两种 TLS 都关闭时失败。
4. TLS 测试必须验证 ServerName 和证书；产品路径不得使用 `InsecureSkipVerify`。
5. subject 明确断言 `[CPA Monitor]`、`ALERT`/`RECOVERY` 及受影响资源或账户标识。
6. 运行 mailer 测试确认失败。
7. 使用标准库 `net/smtp`、`crypto/tls`、`mime`、`net/mail` 写最小实现；连接建立后设置 deadline，发送结束执行 QUIT/Close。
8. 运行 `go test ./internal/mailer -count=1`。

## Task 10: Reconcile Alerts, Recoveries, and Retries

**Files:**

- Create: `internal/alerter/manager.go`
- Create: `internal/alerter/manager_test.go`

**Steps:**

1. 用 fake sender/store 先写失败测试：
   - healthy→unhealthy 发送一次并 active。
   - active→仍 unhealthy 不重发。
   - active→healthy 删除；`send_recovery=false` 不发邮件。
   - `send_recovery=true` 只发一次 recovery。
   - alert send failure 不 active、下一轮重试。
   - recovery send failure 保留 active、下一轮重试。
   - 一个 key 失败不阻塞其他 key。
   - `Complete=false` 可发送当前已知的新异常，但不恢复任何缺失 key。
   - 一个 scope unknown 不影响另一个 scope。
   - save failure 返回错误但保留内存 mutation，当前进程不重复发送。
   - 所有处理按 key 稳定排序。
2. 运行 alerter 测试确认失败。
3. 实现 `Reconcile(ctx, rule.Batch)`；只有 SMTP 成功后推进状态，每个 batch 至多 save 一次。
4. 运行 `go test ./internal/alerter -count=1`。

## Task 11: Enforce Strict Local Log Rotation

**Files:**

- Create: `internal/logfile/writer.go`
- Create: `internal/logfile/logger.go`
- Create: `internal/logfile/writer_test.go`
- Create: `internal/logfile/logger_test.go`

**Steps:**

1. 使用 temp directory 先写失败测试：
   - 未到单文件上限不轮转；达到上限前轮转。
   - `monitor.log.N` 备份不超过 `max_files`，当前文件不计入该数量。
   - 当前 + 备份总大小不超过 `max_total_size`，最旧备份先删。
   - 启动时立刻修剪既有超大文件/过多备份/总量超限。
   - 单次超大 write 分段轮转且每个文件都不突破硬上限。
   - 无关文件不删除；并发 write 通过 mutex 保持完整。
   - file disabled 时 logger 只输出 stdout/stderr；level 过滤正确。
2. 运行 logfile 测试确认失败。
3. 实现自有 bounded writer 和 `slog` factory，不引入 rotation library。
4. 运行 `go test ./internal/logfile -count=1`。

## Task 12: Orchestrate One Full Check Cycle

**Files:**

- Create: `internal/monitor/runner.go`
- Create: `internal/monitor/runner_test.go`

**Steps:**

1. 全部依赖通过小接口注入，先写失败测试：
   - 一轮 health、memory、disk、TCP、accounts 各调用一次。
   - 任一 collector 失败，其他检查与告警仍运行。
   - health transport error 形成 complete down batch。
   - health down 邮件成功时 `RunOnce` 不把“发现被监控服务异常”当作 monitor 自身执行错误。
   - Management API failure 形成 auth unknown，不推断全部账户异常、不恢复旧 auth key。
   - disk partial result 仍告警已知超限 mount，但不误恢复其他 disk key。
   - mail/state 错误汇总，后续 scope 继续。
   - 日志含 check/scope 上下文但不含 secret。
2. 运行 `go test ./internal/monitor -run TestRunner -count=1` 确认失败。
3. 实现顺序确定的 `RunOnce(ctx) error`，使用 `errors.Join` 返回所有运行错误。
4. 重跑局部测试。

## Task 13: Implement Daemon Loop and One-Shot Semantics

**Files:**

- Create: `internal/monitor/loop.go`
- Create: `internal/monitor/loop_test.go`

**Steps:**

1. 注入 ticker/fake runner，先写失败测试：立即首轮、每 tick 一轮、单轮错误不终止 daemon、context cancel 停止 ticker、无 goroutine 重入、`--once` 不创建 ticker。
2. 运行 loop 测试确认失败。
3. 实现串行 loop；interval 从一轮开始时间计，ticker 合并慢周期而不堆积并发任务。
4. 运行：

```bash
go test ./internal/monitor -run 'TestLoop|TestOnce' -count=1
```

## Task 14: Wire the CLI and Production Dependencies

**Files:**

- Create: `internal/app/app.go`
- Create: `internal/app/app_test.go`
- Modify: `cmd/cpa-monitor/main.go`
- Create: `cmd/cpa-monitor/main_test.go`

**Steps:**

1. 把参数解析和 runtime factory 做成可注入边界，先写失败测试：默认 `config.yaml`、`--config`、`--once`、`--help`、unknown flag、配置错误、state load error 后仍执行检查且 once 最终非零、daemon 记录错误后继续。
2. 用 fake runtime 验证 `--once` 恰好一轮，运行错误返回非零。
3. 运行 app/cmd 测试确认失败。
4. 实现生产组装：config → logger → state → SMTP → HTTP client → Linux host collector → rules/alerter → runner。
5. `main` 用 `signal.NotifyContext` 处理 SIGINT/SIGTERM，只负责调用 app 并映射退出码。
6. 所有 close error 被记录；日志始终包含 stdout/stderr，启用时再 tee 到 file。
7. 运行：

```bash
go test ./internal/app ./cmd/cpa-monitor -count=1
go run ./cmd/cpa-monitor --help
```

## Task 15: Add Integration Scenarios

**Files:**

- Create: `internal/monitor/integration_test.go`
- Create: `internal/monitor/testdata/auth-files.json`
- Create: `internal/app/integration_test.go`

**Steps:**

1. 用 fake CLIProxyAPI、fake HostCollector、recording/failing mailer 和真实 temp state file 写集成测试：
   - 全健康时不发邮件。
   - health/resource/network/quota 同时异常时得到准确 keys。
   - 第二轮持续异常不重复；恢复清除；再次异常可重发。
   - SMTP 首轮失败、次轮成功，验证 eligibility 保留。
   - Management API 失败时旧 auth alert 不恢复。
   - state 跨 runner 重建后仍去重。
2. 再写 app 级 `--once` 集成测试：真实解析 `--config <temp-file> --once`，由注入 factory 组装真实 runner/rules/alerter/state 与 fake collectors/mailer，断言完整链只执行一轮、产生预期邮件并映射正确退出码；不能只替换成一个 fake runtime。
3. 运行 integration 测试确认失败，再补齐最小 wiring 缺口。
4. 运行：

```bash
go test ./internal/monitor -run Integration -count=1
go test ./internal/app -run Integration -count=1
```

## Task 16: Provide Example Configuration and Deployment Documentation

**Files:**

- Create: `config.example.yaml`
- Create: `README.md`
- Modify: `internal/config/config_test.go`

**Steps:**

1. 先增加 `TestExampleConfig`，用 `LoadWithEnv` 注入假的 management/SMTP secret 并隔离真实进程环境，要求仓库根目录 example 能通过真实 loader，默认/env 行为与文档一致。
2. 创建包含全部字段、timeout、默认阈值和 secret env names 的 example，不放真实 secret。
3. README 写明：
   - 构建、测试、交叉编译命令。
   - 两种运行模式、立即首轮和 `--once` 退出码。
   - YAML 字段、默认值、env 优先级与 secret 安全。
   - `/healthz`、Management API key、账户判定语义。
   - Linux `/proc`、磁盘公式、TCP 包含所有 states。
   - state unknown/retry/recovery 行为和文件权限。
   - SMTP STARTTLS/direct TLS 示例，以及不支持明文 SMTP 的说明。
   - long-running systemd service、oneshot service + timer、带 `flock` 防重叠的 cron 示例。
   - 日志三个硬限制及 `max_files` 语义。
4. 运行：

```bash
go test ./internal/config -run TestExampleConfig -count=1
```

## Final Verification Gate

按顺序执行；任何一步失败都先修复并从相关局部测试重跑：

```bash
set -euo pipefail
export GOWORK=off

gofmt -w cmd internal
go mod tidy
go test -mod=readonly ./...
go test -mod=readonly -race ./...
go vet -mod=readonly ./...
go build -mod=readonly -trimpath -o /tmp/cpa-monitor ./cmd/cpa-monitor
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -mod=readonly -trimpath -o /tmp/cpa-monitor-linux-amd64 ./cmd/cpa-monitor
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -mod=readonly -trimpath -o /tmp/cpa-monitor-linux-arm64 ./cmd/cpa-monitor
go list -mod=readonly -m all
```

独立性负向门禁：

```bash
assert_no_match() {
  if rg "$@"; then
    echo "forbidden match found" >&2
    return 1
  else
    rc=$?
    if [ "$rc" -eq 1 ]; then
      return 0
    fi
    return "$rc"
  fi
}

verify_tmp="$(mktemp -d)"
trap 'rm -rf "$verify_tmp"' EXIT

go list -mod=readonly -deps -test ./... > "$verify_tmp/deps.txt"
go mod graph > "$verify_tmp/mod-graph.txt"
assert_no_match -n -i 'CLIProxyAPI' "$verify_tmp/deps.txt"
assert_no_match -n -i 'CLIProxyAPI' "$verify_tmp/mod-graph.txt"
assert_no_match -n -i \
  'github.com/router-for-me/CLIProxyAPI|(\.\./)+CLIProxyAPI' \
  --glob '*.go' --glob 'go.mod' --glob 'go.sum' .
assert_no_match -n '^[[:space:]]*replace([[:space:]]|\()' go.mod
```

最终人工/只读核查：

- 独立的 CLIProxyAPI Git 工作树相对实施前基线没有新增变化。
- cpa-monitor 的 module graph 没有 CLIProxyAPI。
- 没有读取 CLIProxyAPI `.cds`、内部 Redis error channel 或内部 package。
- `config.example.yaml` 不含 secret，日志/错误测试确认不泄露 secret。
- 所有设计验收标准均能映射到至少一个自动测试或 README 操作说明。

## Approval Gate

审批本计划后才开始 Task 1。若上述 Decisions 需要调整，应先修改本计划，再进入实现。
