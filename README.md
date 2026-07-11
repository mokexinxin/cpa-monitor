# cpa-monitor

`cpa-monitor` is a standalone Go service for monitoring Linux hosts that run
CLIProxyAPI. It uses only CLIProxyAPI's HTTP endpoints and does not import or
depend on the CLIProxyAPI Go module.

It checks:

- `GET /healthz` liveness;
- Linux memory usage from `/proc/meminfo`;
- real mount usage from `/proc/self/mountinfo` plus `statfs`;
- all TCP entries in `/proc/net/tcp` and `/proc/net/tcp6`;
- account availability from `GET /v0/management/auth-files`.

Alerts are sent through SMTP once when a condition becomes unhealthy. The
same key is suppressed until it recovers. Optional recovery mail can be
enabled. Scheduled health reports can also confirm that every monitoring scope
is working and healthy; all emails include an HTML view and a plain-text
fallback.

## Quick install (Linux)

Install the latest release on a Linux `amd64` or `arm64` server with one
command:

```sh
curl -fsSL https://raw.githubusercontent.com/mokexinxin/cpa-monitor/main/bootstrap.sh | sudo bash
```

The bootstrap verifies the release SHA-256 checksum, opens the interactive
configuration prompts, installs the systemd units, and starts
`cpa-monitor.service`. The server needs systemd, `curl`, and `flock`; Go is not
required. New generated installations enable a daily health email by default;
the first fully healthy cycle sends one immediately, which also verifies the
SMTP configuration end to end.

After installation:

```sh
sudo systemctl status cpa-monitor.service
sudo journalctl -u cpa-monitor.service -f
```

See [One-command Linux installation](#one-command-linux-installation) for
timer mode, non-interactive setup, version pinning, upgrades, and installed
paths.

## Build and test

The project currently targets Go 1.26 and Linux deployment:

```sh
export GOWORK=off
go test ./...
go build -trimpath -o cpa-monitor ./cmd/cpa-monitor
```

After the repository is published, Go 1.26+ users can also install the command
directly:

```sh
go install github.com/mokexinxin/cpa-monitor/cmd/cpa-monitor@latest
```

Build a static Linux binary from macOS:

```sh
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
  go build -trimpath -o cpa-monitor-linux-amd64 ./cmd/cpa-monitor
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 \
  go build -trimpath -o cpa-monitor-linux-arm64 ./cmd/cpa-monitor
```

## Run

Long-running mode executes a check immediately and then executes serially at
the configured interval:

```sh
./cpa-monitor --config config.yaml
```

One-shot mode executes exactly one full cycle and is suitable for cron or a
systemd timer:

```sh
./cpa-monitor --config config.yaml --once
```

Validate configuration without creating the runtime, touching alert state, or
accessing CLIProxyAPI/SMTP:

```sh
./cpa-monitor --config config.yaml --check-config
```

The default configuration path is `config.yaml` in the current working
directory. Relative state and log paths are also resolved from the current
working directory.

In `--once` mode, detecting an unhealthy monitored condition is not itself a
process failure when alert processing succeeds. Collector, Management API,
SMTP, state, or other monitor execution errors produce a non-zero exit after
the remaining independent checks have run. The daemon logs the same errors and
continues with the next cycle.

## Configuration

Copy [`config.example.yaml`](config.example.yaml) and adjust the SMTP
addresses. The important defaults are:

| Setting | Default |
| --- | --- |
| `interval` | `60s` |
| `cliproxy.base_url` | `http://127.0.0.1:8317` |
| `cliproxy.timeout` | `10s` |
| `thresholds.memory_percent` | `80` |
| `thresholds.disk_percent` | `80` |
| `thresholds.total_tcp_connections` | `3000` |
| `thresholds.service_port_connections` | `800` |
| `alerts.send_recovery` | `false` |
| `alerts.state_file` | `state/alerts.json` |
| `health_report.enabled` | `false` for omitted/existing configs; installer default `true` |
| `health_report.interval` | `24h` |
| `health_report.retry_interval` | `15m` |
| `smtp.port` | `587` |
| `smtp.starttls` | `true` |
| `smtp.timeout` | `10s` |
| `logging.file.enabled` | `false` |
| `logging.file.max_size_mb` | `20` |
| `logging.file.max_files` | `5` rotated backups |
| `logging.file.max_total_size_mb` | `80` including active log |

Configuration is decoded strictly: unknown YAML fields and invalid values
stop startup with a field-specific error.

### Secrets

`management_key_env`, `username_env`, and `password_env` name environment
variables that override their inline values whenever the variables are set.
Keep inline secret values empty in configuration committed to source control.

For the example configuration:

```sh
export CPA_MANAGEMENT_KEY='replace-with-management-key'
export CPA_SMTP_USERNAME='smtp-user'
export CPA_SMTP_PASSWORD='smtp-password'
```

The Management key is required because account monitoring is always enabled.
Create or retrieve it using CLIProxyAPI's normal Management API configuration;
the monitor sends it only as `Authorization: Bearer ...` to the configured
base URL. Redirects are refused so the credential is not forwarded to another
endpoint. Plain HTTP is accepted only for loopback hosts (`localhost`,
`127.0.0.0/8`, or `::1`); remote hosts must use HTTPS.

SMTP authentication is optional, but username and password must either both
be set or both be empty. Exactly one of `smtp.starttls` and `smtp.tls` must be
true. Unencrypted SMTP is deliberately unsupported. Standard certificate and
server-name verification remains enabled.

Direct TLS, commonly used on port 465, is configured as:

```yaml
smtp:
  host: smtp.example.com
  port: 465
  starttls: false
  tls: true
```

### Scheduled health email

Enable periodic health reports with:

```yaml
health_report:
  enabled: true
  interval: 24h
  retry_interval: 15m
```

A report is eligible only after all five scopes—CLIProxyAPI health, memory,
disk, TCP, and accounts—finish successfully with no active condition. The
first eligible cycle sends immediately. Later reports follow `interval`; a
failed SMTP attempt waits for `retry_interval` before retrying. Delivery times
are stored with alert state, so restarting the service does not send a
duplicate message.

The HTML report uses an email-client-safe responsive card layout, high-contrast
status labels, and escaped dynamic content. A plain-text alternative is always
included. Alert and recovery emails use the same multipart HTML/text format.

To enable it on an existing systemd installation:

```sh
sudoedit /etc/cpa-monitor/config.yaml
sudo systemctl start cpa-monitor-check.service
sudo systemctl restart cpa-monitor.service
sudo journalctl -u cpa-monitor.service -n 100 --no-pager
```

After the restart, wait for a complete healthy cycle. The first health email
should arrive immediately; if any scope is unhealthy or unknown, CPA Monitor
suppresses the health message instead of reporting a false success. A
successful delivery writes `healthy report sent` to the journal.

### Account rules

An account is unhealthy when:

- `unavailable` is `true`;
- `status_message` contains `quota`, `usage limit`, `limit reached`,
  `exhausted`, `额度`, or `限额` (English matching is case-insensitive); or
- trimmed `status` is non-empty and is not case-insensitively equal to
  `active`.

For disabled accounts, `unavailable: true` still alerts, while quota-text and
non-active-status checks are suppressed. An unhealthy entry without a usable
`auth_index` is logged as an incomplete account check rather than assigned an
unstable alert key.

### Host metrics

Memory usage is:

```text
(MemTotal - MemAvailable) / MemTotal * 100
```

Disk usage is calculated for non-pseudo filesystems as:

```text
(Blocks - Bfree) / Blocks * 100
```

TCP thresholds count every entry, including LISTEN, ESTABLISHED, TIME_WAIT,
and other states. The service count matches the local port. When
`cliproxy.service_port` is `0`, the port is derived from the base URL (HTTP 80
or HTTPS 443 when no explicit port is present).

Production `statfs` calls are bounded to 10 seconds per mount and honor process
cancellation. If a network filesystem remains blocked, later cycles reuse the
same outstanding call instead of creating an unbounded number of goroutines.

## Alert state and recovery

Active keys are persisted as versioned JSON, normally in
`state/alerts.json`. Writes use a `0600` temporary file in the same directory,
sync it, and atomically rename it. The final state file is `0600`; a newly
created parent directory is `0750`.

An unknown check result never implies recovery. For example, a failed
Management API request preserves existing account alerts. A partial disk
result may raise new alerts for successfully measured mounts but cannot
recover absent disk keys during that cycle.

If an alert send fails, its key is not activated and the next cycle retries.
If recovery mail is enabled and that send fails, the key remains active and
the recovery is retried. A state write failure leaves the in-process state in
place, so the daemon continues to deduplicate until restart.

## Logging

Logs always go to the console. Optional file logging uses numbered backups
(`monitor.log.1`, `monitor.log.2`, and so on) and enforces all three limits at
startup and during writes:

- maximum active/backup file size;
- maximum number of rotated backups (`max_files` excludes the active file);
- maximum total bytes across the active file and managed backups.

Oldest backups are deleted first. Log and state errors never contain configured
credentials.

## One-command Linux installation

[`install.sh`](install.sh) installs the independent binary, a locked-down
system account, configuration/secrets, writable state/log directories, and the
config-check/daemon/one-shot/timer systemd units. It then enables exactly one
scheduling mode. It requires a Linux host booted with systemd and `flock` from
util-linux.

Install the latest published release interactively with one command:

```sh
curl -fsSL https://raw.githubusercontent.com/mokexinxin/cpa-monitor/main/bootstrap.sh | sudo bash
```

The bootstrap supports Linux `amd64` and `arm64`. It downloads the latest
static binary and version-matched installer from GitHub Releases, verifies the
binary against the published SHA-256 checksum, and then starts the setup
prompts. The server needs `curl`, systemd, and `flock`; it does not need Go.

Timer mode can also be selected in the same command:

```sh
curl -fsSL https://raw.githubusercontent.com/mokexinxin/cpa-monitor/main/bootstrap.sh | \
  sudo bash -s -- --mode timer --timer-interval 5min
```

To audit or pin what is executed, inspect `bootstrap.sh` first or set
`CPA_MONITOR_VERSION` to a release tag:

```sh
curl -fsSL https://raw.githubusercontent.com/mokexinxin/cpa-monitor/v0.2.0/bootstrap.sh | \
  sudo env CPA_MONITOR_VERSION=v0.2.0 bash
```

For an installation from a local source checkout, install Go 1.26 or newer and
run:

```sh
sudo ./install.sh
```

The interactive setup does not place secrets in command-line arguments. It
prompts for the Management API key and optional SMTP credentials, builds a
static binary, runs the Go tests, validates the generated configuration without
network access, and starts daemon mode.

To install a prebuilt binary instead of compiling on the server:

```sh
sudo ./install.sh --binary ./cpa-monitor-linux-amd64
```

To run one-shot checks from a systemd timer instead of a resident daemon:

```sh
sudo ./install.sh --mode timer --timer-interval 1min
```

The accepted timer suffixes are `ms`, `s`, `min`, `h`, `d`, and `w`. A timer
waits for the previous one-shot run to finish before starting its next interval.

### Non-interactive installation

The safest non-interactive input is a prepared YAML file and a root-readable
systemd environment file:

```ini
CPA_MANAGEMENT_KEY="replace-with-management-key"
CPA_SMTP_USERNAME="smtp-user"
CPA_SMTP_PASSWORD="smtp-password"
```

SMTP username/password lines may both be omitted when authentication is not
needed. Protect the file, then install it:

```sh
chmod 600 /secure/cpa-monitor.env
curl -fsSL https://raw.githubusercontent.com/mokexinxin/cpa-monitor/main/bootstrap.sh | \
  sudo bash -s -- --non-interactive \
    --config /secure/cpa-monitor.yaml \
    --env-file /secure/cpa-monitor.env
```

The environment file is systemd syntax, not a shell script; do not `source` it.
For CI/provisioning tools, the script can instead generate both files from
`CPA_MONITOR_*` environment variables. `./install.sh --help` lists every
supported variable. Pass secrets through the tool's secret environment, never
as installer arguments.

Health-report installer defaults can be overridden with
`CPA_MONITOR_HEALTH_REPORT_ENABLED`, `CPA_MONITOR_HEALTH_REPORT_INTERVAL`, and
`CPA_MONITOR_HEALTH_REPORT_RETRY_INTERVAL`.

### Installed paths and upgrades

| Asset | Path / identity | Permissions |
| --- | --- | --- |
| Binary | `/usr/local/bin/cpa-monitor` | `root:root 0755` |
| Config | `/etc/cpa-monitor/config.yaml` | `root:cpa-monitor 0640` |
| Secrets | `/etc/cpa-monitor/cpa-monitor.env` | `root:root 0600` |
| Alert state | `/var/lib/cpa-monitor/state/alerts.json` | service-owned, file `0600` |
| Rotating log | `/var/log/cpa-monitor/monitor.log` | service-owned |
| Runtime account | `cpa-monitor` | system user, no login shell |
| Units | `/etc/systemd/system/cpa-monitor*` | `root:root 0644` |

Re-run the same install command to upgrade. The binary and units are replaced
atomically and the active mode is restarted. Existing config, secrets, and
alert state are preserved by default. `--config` and `--env-file` explicitly
replace their respective managed files; `--force-config` regenerates both.
Changed config/secret files receive unique backups in the root-only
`/etc/cpa-monitor/backups` directory.

Use `--no-start` to install files and run `systemctl daemon-reload` without
switching or restarting services. `--skip-tests` skips source tests but still
builds and smoke-tests the binary. `--root /staging/tree` creates a relocatable
packaging/test tree without changing the host user database or systemd; unit
contents intentionally retain production absolute paths.

## systemctl operations

Daemon mode:

```sh
sudo systemctl status --no-pager --full cpa-monitor.service
sudo systemctl restart cpa-monitor.service
sudo systemctl stop cpa-monitor.service
sudo systemctl start cpa-monitor.service
sudo journalctl -u cpa-monitor.service -n 100 --no-pager
sudo journalctl -u cpa-monitor.service -f
```

Timer mode (check output belongs to the one-shot service, not the timer):

```sh
sudo systemctl status --no-pager --full \
  cpa-monitor.timer cpa-monitor-once.service
sudo systemctl list-timers --all cpa-monitor.timer
sudo systemctl start cpa-monitor-once.service
sudo journalctl -u cpa-monitor-once.service -n 100 --no-pager
sudo journalctl -u cpa-monitor-once.service -f
```

Switch modes by rerunning the installer. It stops and disables the conflicting
mode before enabling the selected one, and all execution paths share a `flock`
lock so a manual one-shot cannot race an active daemon:

```sh
sudo ./install.sh --mode daemon
sudo ./install.sh --mode timer --timer-interval 5min
```

On a host installed from a prebuilt binary without Go, append
`--binary /usr/local/bin/cpa-monitor` when switching modes.

After editing configuration or secrets, run the independent check below and
then restart the active service/timer. Each monitor service also has a
no-network `ExecStartPre=... --check-config`, so invalid config is rejected
before a monitor cycle begins. Inspect the journal for the exact field-specific
error.

You can run that validation independently, without starting a monitor cycle:

```sh
sudo systemctl start cpa-monitor-check.service
sudo systemctl status --no-pager --full cpa-monitor-check.service
sudo journalctl -u cpa-monitor-check.service -n 100 --no-pager
```

Disable all scheduling with:

```sh
sudo systemctl disable --now cpa-monitor.service cpa-monitor.timer
sudo systemctl stop cpa-monitor-once.service
```

The units deliberately avoid `PrivateNetwork` and `/proc` restrictions because
the monitor needs HTTP/SMTP access plus `/proc/meminfo`, `/proc/net/tcp*`, and
the host mount view in `/proc/self/mountinfo`. They run unprivileged with no
capabilities, `NoNewPrivileges`, namespace creation restrictions, address-family
restrictions, and a restrictive umask. Filesystem mount-namespace hardening
such as `ProtectSystem`, `ProtectHome`, `PrivateTmp`, or `PrivateDevices` should
not be added without verifying that host mount discovery remains accurate.

## cron (manual alternative)

Disable both systemd modes first, then use `flock` so overlapping one-shot
processes cannot race on the state file:

```cron
* * * * * cd /var/lib/cpa-monitor && flock -n /var/lib/cpa-monitor/.cpa-monitor.lock /usr/local/bin/cpa-monitor --config /etc/cpa-monitor/config.yaml --once
```

Load secrets through the cron environment or a root-owned wrapper; do not put
credentials directly in the crontab command line.
