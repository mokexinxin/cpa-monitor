# CPA Monitor Design

## Overview

`cpa-monitor` is a standalone Go monitoring service for Linux hosts running CLIProxyAPI. It lives in its own repository, does not import CLIProxyAPI packages, and does not require changes to CLIProxyAPI.

The monitor checks host resources, network connection counts, CLIProxyAPI liveness, and CLIProxyAPI account status. It sends notifications through a signed DingTalk custom group robot, SMTP, or an ordered primary/fallback route. Alerts are deduplicated so a condition sends only when it changes from healthy to unhealthy; the same condition can alert again only after it has recovered.

## Existing CLIProxyAPI Interfaces

CLIProxyAPI exposes a lightweight unauthenticated liveness endpoint:

```text
GET  /healthz
HEAD /healthz
```

`GET /healthz` returns HTTP 200 with `{"status":"ok"}`. `HEAD /healthz` returns HTTP 200 with no body. This endpoint confirms the HTTP server is alive, but it does not expose memory, disk, network, or account quota details.

Account state is available through the Management API:

```text
GET /v0/management/auth-files
Authorization: Bearer <management-key>
```

The response contains a `files` array. Each entry may include `auth_index`, `name`, `type`, `provider`, `email`, `account`, `status`, `status_message`, `disabled`, `unavailable`, `success`, `failed`, and `recent_requests`.

The monitor will use this Management API response to detect account quota or availability problems. It will not rely on CLIProxyAPI internal packages, persisted `.cds` cooldown files, or the internal Redis RESP error subscription channel in the first version.

## Goals

- Monitor CLIProxyAPI liveness through `/healthz`.
- Monitor Linux memory usage and alert when usage exceeds 80% by default.
- Monitor real disk mount usage and alert when any mount exceeds 80% by default.
- Monitor total host TCP connections and CLIProxyAPI service-port TCP connections.
- Monitor account availability through `/v0/management/auth-files`.
- Send transport-neutral notifications with the affected resource or account clearly identified.
- Support a long-running daemon mode and a `--once` mode for cron or systemd timers.
- Keep all behavior configurable through YAML with environment variable overrides for secrets.
- Write optional local logs with strict size limits and rotation.

## Non-Goals

- Do not modify CLIProxyAPI.
- Do not import CLIProxyAPI code or depend on its Go module.
- Do not implement a dashboard or web UI.
- Do not perform remediation actions, such as resetting quota or restarting services.
- Do not subscribe to CLIProxyAPI's Redis-style `errors` channel in the first version.

## Project Layout

The new project will be created at:

```text
cpa-monitor/
```

Proposed layout:

```text
cpa-monitor/
  cmd/cpa-monitor/main.go
  internal/alerter/
  internal/cliproxy/
  internal/collector/
  internal/config/
  internal/logfile/
  internal/mailer/
  internal/dingtalk/
  internal/notification/
  internal/rule/
  internal/state/
  config.example.yaml
  README.md
  go.mod
  go.sum
```

## Runtime Modes

The binary supports both long-running and one-shot execution:

```text
cpa-monitor --config config.yaml
cpa-monitor --config config.yaml --once
```

Default mode is long-running. It loads configuration, initializes logging and alert state, then runs checks every configured interval.

`--once` runs one full check cycle and exits. This mode is suitable for cron or `systemd timer`.

## Configuration

The default config path is `config.yaml` in the current working directory. `--config` overrides it.

Example shape:

```yaml
interval: 60s

cliproxy:
  base_url: http://127.0.0.1:8317
  management_key: ""
  management_key_env: CPA_MANAGEMENT_KEY
  service_port: 0

thresholds:
  memory_percent: 80
  disk_percent: 80
  total_tcp_connections: 3000
  service_port_connections: 800

alerts:
  send_recovery: false
  state_file: state/alerts.json
  primary_channel: dingtalk
  fallback_channel: smtp

dingtalk:
  webhook_token_env: CPA_DINGTALK_WEBHOOK_TOKEN
  signing_secret_env: CPA_DINGTALK_SIGNING_SECRET

smtp:
  host: smtp.example.com
  port: 587
  username: ""
  username_env: CPA_SMTP_USERNAME
  password: ""
  password_env: CPA_SMTP_PASSWORD
  from: cpa-monitor@example.com
  to:
    - admin@example.com
  starttls: true
  tls: false

logging:
  level: info
  file:
    enabled: true
    path: logs/monitor.log
    max_size_mb: 20
    max_files: 5
    max_total_size_mb: 80
```

Defaults:

- `cliproxy.base_url`: `http://127.0.0.1:8317`
- `interval`: `60s`
- `thresholds.memory_percent`: `80`
- `thresholds.disk_percent`: `80`
- `thresholds.total_tcp_connections`: `3000`
- `thresholds.service_port_connections`: `800`
- `alerts.send_recovery`: `false`
- `alerts.primary_channel`: `smtp` for backward compatibility
- `alerts.fallback_channel`: empty
- `logging.file.enabled`: `false`

Secret `*_env` fields override inline values when their environment variables are set. DingTalk token and signing secret should remain in the restricted environment file, not YAML.

If `cliproxy.service_port` is `0`, the monitor derives the service port from `cliproxy.base_url`.

## Collection Design

### CLIProxyAPI Health

Each cycle requests:

```text
GET {cliproxy.base_url}/healthz
```

Any request failure or non-200 response creates a `health:cliproxy_down` alert.

### Memory

The Linux collector reads `/proc/meminfo` and uses:

```text
used_percent = (MemTotal - MemAvailable) / MemTotal * 100
```

An alert is raised when usage is greater than or equal to `thresholds.memory_percent`.

### Disk

The Linux collector reads mount information from `/proc/self/mountinfo` or `/proc/mounts`, filters pseudo filesystems, and calls `statfs` for each real mount point.

The default skip list is `proc`, `sysfs`, `tmpfs`, `devtmpfs`, `devpts`, `cgroup`, `cgroup2`, `overlay`, `squashfs`, `securityfs`, `debugfs`, `tracefs`, `fusectl`, `mqueue`, `pstore`, `autofs`, `binfmt_misc`, `bpf`, `configfs`, `hugetlbfs`, and `nsfs`. Mounts with other filesystem types are monitored.

An alert is raised for each mount whose usage is greater than or equal to `thresholds.disk_percent`. The notification lists mount point, filesystem type, used bytes, total bytes, and usage percent.

### Network Connections

The Linux collector reads `/proc/net/tcp` and `/proc/net/tcp6`.

It computes:

- total TCP entries across the host
- TCP entries whose local port equals the CLIProxyAPI service port

Alerts are raised when either count reaches its configured threshold:

- `thresholds.total_tcp_connections`
- `thresholds.service_port_connections`

The default thresholds are sized for a roughly 200-user deployment:

- total host TCP connections: `3000`
- CLIProxyAPI service-port connections: `800`

### Account Status

Each cycle requests:

```text
GET {cliproxy.base_url}/v0/management/auth-files
Authorization: Bearer <management-key>
```

If the Management API request fails, the monitor records an error. The first version treats this as a monitor check failure and logs it. It does not infer that all accounts are exhausted.

Each file entry is considered unhealthy when any condition is true:

- `unavailable == true`
- `status_message` contains quota-like text, case-insensitive:
  - `quota`
  - `usage limit`
  - `limit reached`
  - `exhausted`
  - `额度`
  - `限额`
- `status` is non-empty and not active, while `disabled != true`

Disabled accounts do not trigger quota alerts.

The account alert identifies:

- `auth_index`
- `name`
- `provider` or `type`
- `email` or `account`
- `status`
- `status_message`

## Rule and Alert Keys

The rule engine converts collected facts into stable alert keys:

```text
health:cliproxy_down
resource:memory
resource:disk:<mount-point>
network:total_tcp
network:service_port:<port>
auth:<auth_index>
```

Stable keys allow the alert state store to suppress repeated notifications for the same ongoing condition.

## Alert State

The monitor persists alert state in JSON, defaulting to:

```text
state/alerts.json
```

State writes use a temporary file followed by atomic rename. If both the primary and optional fallback delivery fail, the alert is not marked active, so the next cycle retries.

Default behavior:

- send a notification when an alert key changes from healthy to unhealthy
- do not send repeatedly while it remains unhealthy
- remove the key after recovery
- do not send recovery notifications by default

If `alerts.send_recovery` is true, the monitor also sends one recovery notification when a previously active alert becomes healthy.

## Notification Delivery Design

The alerter emits transport-neutral alert/recovery batches. All new conditions
of the same scope and kind in one cycle are delivered together, reducing
DingTalk traffic while preserving per-key state. The primary sender is tried
first. Fallback is attempted only after primary failure; fallback success is a
completed occurrence and is not replayed to the primary later.

DingTalk delivery uses the fixed custom robot endpoint, HMAC-SHA256 signature
security, Markdown messages, and optional user/mobile/all mentions. Only HTTP
2xx with API `errcode: 0` is success. A `410100` response starts a ten-minute
local cooldown. `dingtalk.max_items` limits rendered detail, not state updates.

Health reports default to the alert primary channel or select an explicit
channel; they do not use the alert fallback.

### SMTP Email

SMTP settings are configurable. The mailer supports:

- plain SMTP with STARTTLS
- direct TLS
- username and password authentication when configured
- multiple recipients

Subject examples:

```text
[CPA Monitor] ALERT memory usage 84.2% on host
[CPA Monitor] ALERT disk / usage 91.0% on host
[CPA Monitor] ALERT auth codex-user@example.com quota unavailable
[CPA Monitor] ALERT CLIProxyAPI health check failed
```

Email bodies include:

- host name
- timestamp
- alert key
- current value and threshold
- relevant account or mount details
- configured CLIProxyAPI base URL

## Logging

The monitor always writes to stdout/stderr.

Local file logging is optional. When enabled, the logger enforces:

- maximum single file size
- maximum number of rotated files
- maximum total log size

Rotation deletes the oldest files until all limits are satisfied. The monitor checks limits during writes and startup so logs cannot grow without bound.

## Error Handling

- Configuration errors fail fast with clear messages.
- Linux-only collectors return clear unsupported-platform errors outside Linux.
- Health and Management API errors are logged with context.
- notification send failures are logged and leave the alert unsent in state.
- State file write failures are logged; the program continues, but deduplication may be less reliable.
- One failed collector does not block other collectors from running in the same cycle.

## Testing Strategy

Unit tests:

- parse and default configuration
- environment variable override behavior
- memory parser for `/proc/meminfo`
- TCP parser for `/proc/net/tcp` and `/proc/net/tcp6`
- disk mount filtering
- Management API response parsing and account rule matching
- alert state transitions and atomic state persistence
- SMTP message construction
- DingTalk signing, Markdown rendering, API errors, redaction, and cooldown
- primary/fallback routing and per-scope batching
- log rotation limits

Integration-style tests:

- fake CLIProxyAPI HTTP server for `/healthz` and `/v0/management/auth-files`
- `--once` execution with mocked collectors and notification senders
- failed primary and fallback delivery keeps alerts eligible for retry

Linux-specific tests will be isolated with build tags or filesystem fixtures so development on macOS can still run non-Linux tests.

## Deployment Notes

The README will include:

- build command:

```text
go build -o cpa-monitor ./cmd/cpa-monitor
```

- test command:

```text
go test ./...
```

- long-running systemd service example
- `systemd timer` example using `--once`
- cron example using `--once`
- DingTalk-only, SMTP-only, and primary/fallback examples
- CLIProxyAPI Management API key setup notes

## Acceptance Criteria

- The project is created in its own `cpa-monitor` repository.
- It builds as an independent Go module.
- It does not import packages from CLIProxyAPI.
- It can run continuously or once.
- It checks `/healthz` and sends one alert on service failure.
- It checks `/v0/management/auth-files` and sends account-specific alerts for unavailable or quota-like accounts.
- It alerts when memory or any real disk mount reaches 80% by default.
- It alerts when total TCP connections reach 3000 or service-port connections reach 800 by default.
- It sends DingTalk and/or SMTP notifications using YAML settings with environment variable overrides.
- It deduplicates alerts until recovery.
- It supports optional local file logging with strict size limits.
