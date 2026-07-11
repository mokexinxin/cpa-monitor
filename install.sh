#!/usr/bin/env bash

# Install cpa-monitor as a standalone Linux systemd service.
#
# The script is intentionally self-contained: it never reads or modifies the
# CLIProxyAPI source tree and it never downloads an unverified binary.

# Callers sometimes invoke installers through a shell with xtrace enabled.
# Disable it before reading any credential-bearing environment variables.
set +x
set -Eeuo pipefail
umask 077

PROGRAM="cpa-monitor"
SERVICE_USER="cpa-monitor"
SERVICE_GROUP="cpa-monitor"

PROD_BINARY="/usr/local/bin/cpa-monitor"
PROD_CONFIG_DIR="/etc/cpa-monitor"
PROD_CONFIG="${PROD_CONFIG_DIR}/config.yaml"
PROD_ENV_FILE="${PROD_CONFIG_DIR}/cpa-monitor.env"
PROD_STATE_DIR="/var/lib/cpa-monitor"
PROD_LOG_DIR="/var/log/cpa-monitor"
PROD_UNIT_DIR="/etc/systemd/system"

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd -P)"

MODE="${CPA_MONITOR_INSTALL_MODE:-daemon}"
TIMER_INTERVAL="${CPA_MONITOR_TIMER_INTERVAL:-1min}"
ROOT="/"
BINARY_SOURCE=""
CONFIG_SOURCE=""
ENV_SOURCE=""
NON_INTERACTIVE=false
FORCE_CONFIG=false
SKIP_TESTS=false
NO_START=false

SYSTEMCTL="${SYSTEMCTL:-}"
TEST_SYSTEMCTL="${CPA_MONITOR_TEST_SYSTEMCTL:-}"
FLOCK_BIN="${FLOCK_BIN:-}"

TMP_DIR=""
ROLLBACK_ACTIVE=false
ERROR_HANDLING=false
SYSTEMD_AVAILABLE=false
OLD_SERVICE_ENABLED=false
OLD_SERVICE_ACTIVE=false
OLD_TIMER_ENABLED=false
OLD_TIMER_ACTIVE=false

TX_TARGETS=()
TX_BACKUPS=()
TX_EXISTED=()
TX_TEMPS=()
DIR_PATHS=()
DIR_EXISTED=()
DIR_MODES=()
DIR_UIDS=()
DIR_GIDS=()

usage() {
    cat <<'EOF'
Usage: sudo ./install.sh [options]

Build and install cpa-monitor, create its system user and directories, install
config-check/daemon/one-shot/timer systemd units, and start exactly one
scheduling mode.

Options:
  --mode daemon|timer      Scheduling mode (default: daemon)
  --timer-interval SPAN    Timer delay after a completed run (default: 1min)
  --binary PATH            Install an existing native binary instead of building
  --config PATH            Install this YAML config, replacing the managed copy
  --env-file PATH          Install this systemd EnvironmentFile (must not be
                           group/world accessible), replacing the managed copy
  --non-interactive        Never prompt; use CPA_MONITOR_* environment variables
  --force-config           Regenerate config and secrets even when they exist
  --skip-tests             Skip "go test ./..." before a source build
  --no-start               Install and daemon-reload, but do not switch/start mode
  --root DIR               Stage files below DIR; unit paths remain production
                           paths. Never changes the host user database/systemd.
  -h, --help               Show this help

First-install automation variables:
  CPA_MONITOR_MANAGEMENT_KEY       required secret
  CPA_MONITOR_BASE_URL             default http://127.0.0.1:8317
  CPA_MONITOR_INTERVAL             default 60s
  CPA_MONITOR_SMTP_HOST            required
  CPA_MONITOR_SMTP_PORT            default 587 (STARTTLS) or 465 (TLS)
  CPA_MONITOR_SMTP_FROM            required sender address
  CPA_MONITOR_SMTP_TO              required comma-separated recipients
  CPA_MONITOR_SMTP_MODE            starttls (default) or tls
  CPA_MONITOR_SMTP_USERNAME        optional; requires matching password
  CPA_MONITOR_SMTP_PASSWORD        optional; requires matching username
  CPA_MONITOR_HEALTH_REPORT_ENABLED        default true
  CPA_MONITOR_HEALTH_REPORT_INTERVAL       default 24h
  CPA_MONITOR_HEALTH_REPORT_RETRY_INTERVAL default 15m

Thresholds and logging can also be set with:
  CPA_MONITOR_MEMORY_PERCENT, CPA_MONITOR_DISK_PERCENT,
  CPA_MONITOR_TOTAL_TCP_CONNECTIONS,
  CPA_MONITOR_SERVICE_PORT_CONNECTIONS, CPA_MONITOR_SERVICE_PORT,
  CPA_MONITOR_SEND_RECOVERY, CPA_MONITOR_LOG_LEVEL,
  CPA_MONITOR_LOG_FILE_ENABLED, CPA_MONITOR_LOG_MAX_SIZE_MB,
  CPA_MONITOR_LOG_MAX_FILES, CPA_MONITOR_LOG_MAX_TOTAL_SIZE_MB

Existing config and secrets are preserved unless --config, --env-file, or
--force-config explicitly requests replacement. Replaced copies are backed up
under /etc/cpa-monitor/backups with root-only directory permissions.
EOF
}

log() {
    printf '[cpa-monitor] %s\n' "$*"
}

warn() {
    printf '[cpa-monitor] WARNING: %s\n' "$*" >&2
}

die() {
    printf '[cpa-monitor] ERROR: %s\n' "$*" >&2
    exit 1
}

require_option_argument() {
    local option="$1"
    local remaining="$2"
    if (( remaining < 2 )); then
        die "${option} requires an argument"
    fi
}

parse_args() {
    while (( $# > 0 )); do
        case "$1" in
            --mode)
                require_option_argument "$1" "$#"
                MODE="$2"
                shift 2
                ;;
            --mode=*)
                MODE="${1#*=}"
                shift
                ;;
            --timer-interval)
                require_option_argument "$1" "$#"
                TIMER_INTERVAL="$2"
                shift 2
                ;;
            --timer-interval=*)
                TIMER_INTERVAL="${1#*=}"
                shift
                ;;
            --binary)
                require_option_argument "$1" "$#"
                BINARY_SOURCE="$2"
                shift 2
                ;;
            --binary=*)
                BINARY_SOURCE="${1#*=}"
                shift
                ;;
            --config)
                require_option_argument "$1" "$#"
                CONFIG_SOURCE="$2"
                shift 2
                ;;
            --config=*)
                CONFIG_SOURCE="${1#*=}"
                shift
                ;;
            --env-file)
                require_option_argument "$1" "$#"
                ENV_SOURCE="$2"
                shift 2
                ;;
            --env-file=*)
                ENV_SOURCE="${1#*=}"
                shift
                ;;
            --root)
                require_option_argument "$1" "$#"
                ROOT="$2"
                shift 2
                ;;
            --root=*)
                ROOT="${1#*=}"
                shift
                ;;
            --non-interactive)
                NON_INTERACTIVE=true
                shift
                ;;
            --force-config)
                FORCE_CONFIG=true
                shift
                ;;
            --skip-tests)
                SKIP_TESTS=true
                shift
                ;;
            --no-start)
                NO_START=true
                shift
                ;;
            -h|--help)
                usage
                exit 0
                ;;
            --)
                shift
                if (( $# > 0 )); then
                    die "unexpected positional arguments"
                fi
                ;;
            -*|*)
                die "unknown argument: $1"
                ;;
        esac
    done
}

root_path() {
    local path="$1"
    if [[ "$ROOT" == "/" ]]; then
        printf '%s' "$path"
    else
        printf '%s%s' "${ROOT%/}" "$path"
    fi
}

absolute_file() {
    local path="$1"
    local directory
    local basename
    [[ -f "$path" ]] || die "source file does not exist or is not regular: $path"
    [[ -r "$path" ]] || die "source file is not readable: $path"
    directory="$(cd "$(dirname "$path")" && pwd -P)"
    basename="$(basename "$path")"
    printf '%s/%s' "$directory" "$basename"
}

reject_line_breaks() {
    local field="$1"
    local value="$2"
    case "$value" in
        *$'\n'*|*$'\r'*) die "${field} must not contain line breaks" ;;
    esac
}

trim() {
    local value="$1"
    value="${value#"${value%%[![:space:]]*}"}"
    value="${value%"${value##*[![:space:]]}"}"
    printf '%s' "$value"
}

yaml_quote() {
    local value="$1"
    value=${value//"'"/"''"}
    printf "'%s'" "$value"
}

systemd_env_quote() {
    local value="$1"
    value="${value//\\/\\\\}"
    value="${value//\"/\\\"}"
    printf '"%s"' "$value"
}

prompt_plain() {
    local label="$1"
    local default_value="$2"
    local value
    if [[ -n "$default_value" ]]; then
        printf '%s [%s]: ' "$label" "$default_value" >&2
    else
        printf '%s: ' "$label" >&2
    fi
    IFS= read -r value || die "input ended while reading ${label}"
    if [[ -z "$value" ]]; then
        value="$default_value"
    fi
    PROMPT_RESULT="$value"
}

prompt_secret() {
    local label="$1"
    local current_value="$2"
    local value
    if [[ -n "$current_value" ]]; then
        printf '%s [press Enter to keep supplied value]: ' "$label" >&2
    else
        printf '%s: ' "$label" >&2
    fi
    IFS= read -r -s value || die "input ended while reading ${label}"
    printf '\n' >&2
    if [[ -z "$value" ]]; then
        value="$current_value"
    fi
    PROMPT_RESULT="$value"
}

prompt_required() {
    local label="$1"
    local current_value="$2"
    while true; do
        prompt_plain "$label" "$current_value"
        if [[ -n "$(trim "$PROMPT_RESULT")" ]]; then
            return
        fi
        warn "${label} is required"
        current_value=""
    done
}

prompt_required_secret() {
    local label="$1"
    local current_value="$2"
    while true; do
        prompt_secret "$label" "$current_value"
        if [[ -n "$(trim "$PROMPT_RESULT")" ]]; then
            return
        fi
        warn "${label} is required"
        current_value=""
    done
}

is_uint() {
    [[ "$1" =~ ^[0-9]+$ ]]
}

validate_positive_int() {
    local field="$1"
    local value="$2"
    is_uint "$value" && (( 10#$value > 0 )) || die "${field} must be a positive integer"
}

validate_port() {
    local field="$1"
    local value="$2"
    is_uint "$value" && (( 10#$value >= 0 && 10#$value <= 65535 )) || die "${field} must be between 0 and 65535"
}

validate_bool() {
    local field="$1"
    local value="$2"
    [[ "$value" == "true" || "$value" == "false" ]] || die "${field} must be true or false"
}

validate_monitor_duration() {
    local value="$1"
    local field="${2:-CPA_MONITOR_INTERVAL}"
    [[ "$value" =~ ^([0-9]+(ns|us|ms|s|m|h))+$ ]] || die "${field} must be a positive Go duration such as 60s or 5m"
    [[ ! "$value" =~ ^0+(ns|us|ms|s|m|h)$ ]] || die "${field} must be greater than zero"
}

validate_timer_interval() {
    [[ "$TIMER_INTERVAL" =~ ^[1-9][0-9]*(ms|s|min|h|d|w)$ ]] || die "timer interval must look like 30s, 5min, 1h, or 1d"
}

validate_base_url_transport() {
    local value="$1"
    case "$value" in
        https://*) return 0 ;;
        http://localhost|http://localhost/*|http://localhost:*|http://127.*|http://\[::1\]|http://\[::1\]/*|http://\[::1\]:*) return 0 ;;
        http://*) die "CPA_MONITOR_BASE_URL must use HTTPS for a non-loopback host" ;;
        *) die "CPA_MONITOR_BASE_URL must start with http:// or https://" ;;
    esac
}

file_mode() {
    local path="$1"
    if [[ "$(uname -s)" == "Darwin" ]]; then
        stat -f '%Lp' "$path"
    else
        stat -c '%a' "$path"
    fi
}

file_uid() {
    local path="$1"
    if [[ "$(uname -s)" == "Darwin" ]]; then
        stat -f '%u' "$path"
    else
        stat -c '%u' "$path"
    fi
}

file_gid() {
    local path="$1"
    if [[ "$(uname -s)" == "Darwin" ]]; then
        stat -f '%g' "$path"
    else
        stat -c '%g' "$path"
    fi
}

validate_env_source_permissions() {
    local path="$1"
    local mode
    local numeric
    mode="$(file_mode "$path")"
    numeric=$((8#$mode))
    (( (numeric & 077) == 0 )) || die "--env-file must not be group/world accessible (use chmod 600)"
    if LC_ALL=C grep -q $'\r' "$path"; then
        die "--env-file must not contain carriage returns"
    fi
}

load_generation_values() {
    MONITOR_INTERVAL="${CPA_MONITOR_INTERVAL:-60s}"
    BASE_URL="${CPA_MONITOR_BASE_URL:-http://127.0.0.1:8317}"
    SERVICE_PORT="${CPA_MONITOR_SERVICE_PORT:-0}"
    MEMORY_PERCENT="${CPA_MONITOR_MEMORY_PERCENT:-80}"
    DISK_PERCENT="${CPA_MONITOR_DISK_PERCENT:-80}"
    TOTAL_TCP_CONNECTIONS="${CPA_MONITOR_TOTAL_TCP_CONNECTIONS:-3000}"
    SERVICE_PORT_CONNECTIONS="${CPA_MONITOR_SERVICE_PORT_CONNECTIONS:-800}"
    SEND_RECOVERY="${CPA_MONITOR_SEND_RECOVERY:-false}"
    HEALTH_REPORT_ENABLED="${CPA_MONITOR_HEALTH_REPORT_ENABLED:-true}"
    HEALTH_REPORT_INTERVAL="${CPA_MONITOR_HEALTH_REPORT_INTERVAL:-24h}"
    HEALTH_REPORT_RETRY_INTERVAL="${CPA_MONITOR_HEALTH_REPORT_RETRY_INTERVAL:-15m}"
    LOG_LEVEL="${CPA_MONITOR_LOG_LEVEL:-info}"
    LOG_FILE_ENABLED="${CPA_MONITOR_LOG_FILE_ENABLED:-true}"
    LOG_MAX_SIZE_MB="${CPA_MONITOR_LOG_MAX_SIZE_MB:-20}"
    LOG_MAX_FILES="${CPA_MONITOR_LOG_MAX_FILES:-5}"
    LOG_MAX_TOTAL_SIZE_MB="${CPA_MONITOR_LOG_MAX_TOTAL_SIZE_MB:-80}"

    SMTP_HOST="${CPA_MONITOR_SMTP_HOST:-}"
    SMTP_PORT="${CPA_MONITOR_SMTP_PORT:-}"
    SMTP_FROM="${CPA_MONITOR_SMTP_FROM:-}"
    SMTP_TO_CSV="${CPA_MONITOR_SMTP_TO:-}"
    SMTP_MODE="${CPA_MONITOR_SMTP_MODE:-starttls}"
    SMTP_USERNAME="${CPA_MONITOR_SMTP_USERNAME:-}"
    SMTP_PASSWORD="${CPA_MONITOR_SMTP_PASSWORD:-}"
    MANAGEMENT_KEY="${CPA_MONITOR_MANAGEMENT_KEY:-}"
}

collect_interactive_values() {
    if [[ "$CONFIG_ACTION" == "generate" ]]; then
        prompt_plain "CLIProxyAPI base URL" "$BASE_URL"
        BASE_URL="$PROMPT_RESULT"
        prompt_plain "Monitor interval" "$MONITOR_INTERVAL"
        MONITOR_INTERVAL="$PROMPT_RESULT"
        prompt_plain "Enable periodic healthy email (true/false)" "$HEALTH_REPORT_ENABLED"
        HEALTH_REPORT_ENABLED="$PROMPT_RESULT"
        if [[ "$HEALTH_REPORT_ENABLED" == "true" ]]; then
            prompt_plain "Healthy email interval" "$HEALTH_REPORT_INTERVAL"
            HEALTH_REPORT_INTERVAL="$PROMPT_RESULT"
            prompt_plain "Healthy email retry interval after failure" "$HEALTH_REPORT_RETRY_INTERVAL"
            HEALTH_REPORT_RETRY_INTERVAL="$PROMPT_RESULT"
        fi
        prompt_plain "SMTP mode (starttls/tls)" "$SMTP_MODE"
        SMTP_MODE="$PROMPT_RESULT"
        if [[ -z "$SMTP_PORT" ]]; then
            if [[ "$SMTP_MODE" == "tls" ]]; then
                SMTP_PORT="465"
            else
                SMTP_PORT="587"
            fi
        fi
        prompt_required "SMTP host" "$SMTP_HOST"
        SMTP_HOST="$PROMPT_RESULT"
        prompt_plain "SMTP port" "$SMTP_PORT"
        SMTP_PORT="$PROMPT_RESULT"
        prompt_required "Alert sender address" "$SMTP_FROM"
        SMTP_FROM="$PROMPT_RESULT"
        prompt_required "Alert recipients (comma-separated)" "$SMTP_TO_CSV"
        SMTP_TO_CSV="$PROMPT_RESULT"
    fi

    if [[ "$ENV_ACTION" == "generate" ]]; then
        prompt_required_secret "CLIProxyAPI management key" "$MANAGEMENT_KEY"
        MANAGEMENT_KEY="$PROMPT_RESULT"
        prompt_plain "SMTP username (empty disables authentication)" "$SMTP_USERNAME"
        SMTP_USERNAME="$PROMPT_RESULT"
        if [[ -n "$SMTP_USERNAME" || -n "$SMTP_PASSWORD" ]]; then
            prompt_required_secret "SMTP password" "$SMTP_PASSWORD"
            SMTP_PASSWORD="$PROMPT_RESULT"
        fi
    fi
}

split_recipients() {
    local item
    local cleaned
    local i
    SMTP_RECIPIENTS=()
    IFS=',' read -r -a SMTP_RECIPIENTS <<< "$SMTP_TO_CSV"
    (( ${#SMTP_RECIPIENTS[@]} > 0 )) || die "CPA_MONITOR_SMTP_TO must contain at least one recipient"
    for ((i = 0; i < ${#SMTP_RECIPIENTS[@]}; i++)); do
        item="${SMTP_RECIPIENTS[$i]}"
        cleaned="$(trim "$item")"
        [[ -n "$cleaned" ]] || die "CPA_MONITOR_SMTP_TO contains an empty recipient"
        reject_line_breaks "CPA_MONITOR_SMTP_TO" "$cleaned"
        SMTP_RECIPIENTS[$i]="$cleaned"
    done
}

validate_generation_values() {
    if [[ "$CONFIG_ACTION" == "generate" ]]; then
        reject_line_breaks "CPA_MONITOR_BASE_URL" "$BASE_URL"
        reject_line_breaks "CPA_MONITOR_SMTP_HOST" "$SMTP_HOST"
        reject_line_breaks "CPA_MONITOR_SMTP_FROM" "$SMTP_FROM"
        validate_monitor_duration "$MONITOR_INTERVAL" "CPA_MONITOR_INTERVAL"
        validate_bool "CPA_MONITOR_HEALTH_REPORT_ENABLED" "$HEALTH_REPORT_ENABLED"
        validate_monitor_duration "$HEALTH_REPORT_INTERVAL" "CPA_MONITOR_HEALTH_REPORT_INTERVAL"
        validate_monitor_duration "$HEALTH_REPORT_RETRY_INTERVAL" "CPA_MONITOR_HEALTH_REPORT_RETRY_INTERVAL"
        validate_base_url_transport "$BASE_URL"
        validate_port "CPA_MONITOR_SERVICE_PORT" "$SERVICE_PORT"
        validate_positive_int "CPA_MONITOR_MEMORY_PERCENT" "$MEMORY_PERCENT"
        validate_positive_int "CPA_MONITOR_DISK_PERCENT" "$DISK_PERCENT"
        (( 10#$MEMORY_PERCENT <= 100 )) || die "CPA_MONITOR_MEMORY_PERCENT must be at most 100"
        (( 10#$DISK_PERCENT <= 100 )) || die "CPA_MONITOR_DISK_PERCENT must be at most 100"
        validate_positive_int "CPA_MONITOR_TOTAL_TCP_CONNECTIONS" "$TOTAL_TCP_CONNECTIONS"
        validate_positive_int "CPA_MONITOR_SERVICE_PORT_CONNECTIONS" "$SERVICE_PORT_CONNECTIONS"
        validate_bool "CPA_MONITOR_SEND_RECOVERY" "$SEND_RECOVERY"
        validate_bool "CPA_MONITOR_LOG_FILE_ENABLED" "$LOG_FILE_ENABLED"
        case "$LOG_LEVEL" in debug|info|warn|error) ;; *) die "CPA_MONITOR_LOG_LEVEL must be debug, info, warn, or error" ;; esac
        validate_positive_int "CPA_MONITOR_LOG_MAX_SIZE_MB" "$LOG_MAX_SIZE_MB"
        validate_positive_int "CPA_MONITOR_LOG_MAX_FILES" "$LOG_MAX_FILES"
        validate_positive_int "CPA_MONITOR_LOG_MAX_TOTAL_SIZE_MB" "$LOG_MAX_TOTAL_SIZE_MB"
        (( 10#$LOG_MAX_TOTAL_SIZE_MB >= 10#$LOG_MAX_SIZE_MB )) || die "log total size must be at least log file size"
        [[ -n "$SMTP_HOST" ]] || die "CPA_MONITOR_SMTP_HOST is required"
        [[ -n "$SMTP_FROM" ]] || die "CPA_MONITOR_SMTP_FROM is required"
        [[ "$SMTP_MODE" == "starttls" || "$SMTP_MODE" == "tls" ]] || die "CPA_MONITOR_SMTP_MODE must be starttls or tls"
        if [[ -z "$SMTP_PORT" ]]; then
            if [[ "$SMTP_MODE" == "tls" ]]; then SMTP_PORT=465; else SMTP_PORT=587; fi
        fi
        validate_positive_int "CPA_MONITOR_SMTP_PORT" "$SMTP_PORT"
        (( 10#$SMTP_PORT <= 65535 )) || die "CPA_MONITOR_SMTP_PORT must be at most 65535"
        split_recipients
    fi

    if [[ "$ENV_ACTION" == "generate" ]]; then
        reject_line_breaks "CPA_MONITOR_MANAGEMENT_KEY" "$MANAGEMENT_KEY"
        reject_line_breaks "CPA_MONITOR_SMTP_USERNAME" "$SMTP_USERNAME"
        reject_line_breaks "CPA_MONITOR_SMTP_PASSWORD" "$SMTP_PASSWORD"
        [[ -n "$(trim "$MANAGEMENT_KEY")" ]] || die "CPA_MONITOR_MANAGEMENT_KEY is required"
        if [[ -n "$SMTP_USERNAME" && -z "$SMTP_PASSWORD" ]] || [[ -z "$SMTP_USERNAME" && -n "$SMTP_PASSWORD" ]]; then
            die "SMTP username and password must both be set or both be empty"
        fi
    fi
}

render_config() {
    local starttls="false"
    local tls="false"
    local recipient
    if [[ "$SMTP_MODE" == "starttls" ]]; then starttls="true"; else tls="true"; fi

    cat <<EOF
interval: $(yaml_quote "$MONITOR_INTERVAL")

cliproxy:
  base_url: $(yaml_quote "$BASE_URL")
  management_key: ''
  management_key_env: CPA_MANAGEMENT_KEY
  service_port: ${SERVICE_PORT}
  timeout: 10s

thresholds:
  memory_percent: ${MEMORY_PERCENT}
  disk_percent: ${DISK_PERCENT}
  total_tcp_connections: ${TOTAL_TCP_CONNECTIONS}
  service_port_connections: ${SERVICE_PORT_CONNECTIONS}

alerts:
  send_recovery: ${SEND_RECOVERY}
  state_file: ${PROD_STATE_DIR}/state/alerts.json

health_report:
  enabled: ${HEALTH_REPORT_ENABLED}
  interval: $(yaml_quote "$HEALTH_REPORT_INTERVAL")
  retry_interval: $(yaml_quote "$HEALTH_REPORT_RETRY_INTERVAL")

smtp:
  host: $(yaml_quote "$SMTP_HOST")
  port: ${SMTP_PORT}
  username: ''
  username_env: CPA_SMTP_USERNAME
  password: ''
  password_env: CPA_SMTP_PASSWORD
  from: $(yaml_quote "$SMTP_FROM")
  to:
EOF
    for recipient in "${SMTP_RECIPIENTS[@]}"; do
        printf '    - %s\n' "$(yaml_quote "$recipient")"
    done
    cat <<EOF
  starttls: ${starttls}
  tls: ${tls}
  timeout: 10s

logging:
  level: ${LOG_LEVEL}
  file:
    enabled: ${LOG_FILE_ENABLED}
    path: ${PROD_LOG_DIR}/monitor.log
    max_size_mb: ${LOG_MAX_SIZE_MB}
    max_files: ${LOG_MAX_FILES}
    max_total_size_mb: ${LOG_MAX_TOTAL_SIZE_MB}
EOF
}

render_environment() {
    printf 'CPA_MANAGEMENT_KEY=%s\n' "$(systemd_env_quote "$MANAGEMENT_KEY")"
    if [[ -n "$SMTP_USERNAME" ]]; then
        printf 'CPA_SMTP_USERNAME=%s\n' "$(systemd_env_quote "$SMTP_USERNAME")"
        printf 'CPA_SMTP_PASSWORD=%s\n' "$(systemd_env_quote "$SMTP_PASSWORD")"
    fi
}

render_service_hardening() {
    cat <<'EOF'
NoNewPrivileges=true
CapabilityBoundingSet=
AmbientCapabilities=
LockPersonality=true
MemoryDenyWriteExecute=true
RestrictNamespaces=true
RestrictRealtime=true
RestrictSUIDSGID=true
SystemCallArchitectures=native
RestrictAddressFamilies=AF_UNIX AF_INET AF_INET6
ProtectHostname=true
EOF
}

render_daemon_unit() {
    cat <<EOF
[Unit]
Description=CPA Monitor daemon
Wants=network-online.target
After=network-online.target
RequiresMountsFor=${PROD_STATE_DIR}

[Service]
Type=simple
User=${SERVICE_USER}
Group=${SERVICE_GROUP}
WorkingDirectory=${PROD_STATE_DIR}
StateDirectory=${PROGRAM}
StateDirectoryMode=0750
UMask=0077
EnvironmentFile=${PROD_ENV_FILE}
ExecStartPre=${PROD_BINARY} --config ${PROD_CONFIG} --check-config
ExecStart=${FLOCK_BIN} -n -E 75 ${PROD_STATE_DIR}/.cpa-monitor.lock ${PROD_BINARY} --config ${PROD_CONFIG}
Restart=on-failure
RestartPreventExitStatus=75
RestartSec=5s
TimeoutStopSec=30s
KillSignal=SIGTERM
StandardOutput=journal
StandardError=journal
SyslogIdentifier=${PROGRAM}
$(render_service_hardening)

[Install]
WantedBy=multi-user.target
EOF
}

render_oneshot_unit() {
    cat <<EOF
[Unit]
Description=CPA Monitor one-shot check
Wants=network-online.target
After=network-online.target
RequiresMountsFor=${PROD_STATE_DIR}

[Service]
Type=oneshot
User=${SERVICE_USER}
Group=${SERVICE_GROUP}
WorkingDirectory=${PROD_STATE_DIR}
StateDirectory=${PROGRAM}
StateDirectoryMode=0750
UMask=0077
EnvironmentFile=${PROD_ENV_FILE}
ExecStartPre=${PROD_BINARY} --config ${PROD_CONFIG} --check-config
ExecStart=${FLOCK_BIN} -n -E 75 ${PROD_STATE_DIR}/.cpa-monitor.lock ${PROD_BINARY} --config ${PROD_CONFIG} --once
TimeoutStartSec=infinity
TimeoutStopSec=30s
KillSignal=SIGTERM
StandardOutput=journal
StandardError=journal
SyslogIdentifier=${PROGRAM}
$(render_service_hardening)
EOF
}

render_check_unit() {
    cat <<EOF
[Unit]
Description=Validate CPA Monitor configuration

[Service]
Type=oneshot
User=${SERVICE_USER}
Group=${SERVICE_GROUP}
UMask=0077
EnvironmentFile=${PROD_ENV_FILE}
ExecStart=${PROD_BINARY} --config ${PROD_CONFIG} --check-config
TimeoutStartSec=30s
StandardOutput=journal
StandardError=journal
SyslogIdentifier=${PROGRAM}
$(render_service_hardening)
EOF
}

render_timer_unit() {
    cat <<EOF
[Unit]
Description=Run CPA Monitor periodically

[Timer]
OnBootSec=1min
OnUnitInactiveSec=${TIMER_INTERVAL}
Unit=cpa-monitor-once.service

[Install]
WantedBy=timers.target
EOF
}

cleanup() {
    local temp
    if (( ${#TX_TEMPS[@]} > 0 )); then
        for temp in "${TX_TEMPS[@]}"; do
            [[ -n "$temp" ]] && rm -f "$temp" 2>/dev/null || true
        done
    fi
    if [[ -n "$TMP_DIR" && -d "$TMP_DIR" ]]; then
        rm -rf "$TMP_DIR"
    fi
}

capture_unit_state() {
    if [[ "$SYSTEMD_AVAILABLE" != "true" ]]; then
        return
    fi
    if "$SYSTEMCTL" is-enabled --quiet cpa-monitor.service >/dev/null 2>&1; then OLD_SERVICE_ENABLED=true; fi
    if "$SYSTEMCTL" is-active --quiet cpa-monitor.service >/dev/null 2>&1; then OLD_SERVICE_ACTIVE=true; fi
    if "$SYSTEMCTL" is-enabled --quiet cpa-monitor.timer >/dev/null 2>&1; then OLD_TIMER_ENABLED=true; fi
    if "$SYSTEMCTL" is-active --quiet cpa-monitor.timer >/dev/null 2>&1; then OLD_TIMER_ACTIVE=true; fi
}

restore_unit_state() {
    local failed=false
    [[ "$SYSTEMD_AVAILABLE" == "true" ]] || return 0
    if ! "$SYSTEMCTL" disable --now cpa-monitor.service >/dev/null 2>&1; then failed=true; fi
    if ! "$SYSTEMCTL" disable --now cpa-monitor.timer >/dev/null 2>&1; then failed=true; fi
    if ! "$SYSTEMCTL" stop cpa-monitor-once.service >/dev/null 2>&1; then failed=true; fi
    if ! "$SYSTEMCTL" stop cpa-monitor-check.service >/dev/null 2>&1; then failed=true; fi
    if ! "$SYSTEMCTL" daemon-reload >/dev/null 2>&1; then failed=true; fi

    if [[ "$OLD_SERVICE_ENABLED" == "true" ]] && ! "$SYSTEMCTL" enable cpa-monitor.service >/dev/null 2>&1; then failed=true; fi
    if [[ "$OLD_TIMER_ENABLED" == "true" ]] && ! "$SYSTEMCTL" enable cpa-monitor.timer >/dev/null 2>&1; then failed=true; fi
    if [[ "$OLD_SERVICE_ACTIVE" == "true" ]] && ! "$SYSTEMCTL" start cpa-monitor.service >/dev/null 2>&1; then failed=true; fi
    if [[ "$OLD_TIMER_ACTIVE" == "true" ]] && ! "$SYSTEMCTL" start cpa-monitor.timer >/dev/null 2>&1; then failed=true; fi
    [[ "$failed" == "false" ]]
}

rollback_files() {
    local failed=false
    local i
    local target
    local backup
    local temporary
    for ((i = ${#TX_TARGETS[@]} - 1; i >= 0; i--)); do
        target="${TX_TARGETS[$i]}"
        backup="${TX_BACKUPS[$i]}"
        if [[ "${TX_EXISTED[$i]}" == "true" ]]; then
            temporary="${target}.cpa-monitor-rollback.$$"
            rm -f "$temporary"
            if ! cp -p "$backup" "$temporary" || ! mv -f "$temporary" "$target"; then
                warn "could not restore previous file: $target"
                rm -f "$temporary" 2>/dev/null || true
                failed=true
            fi
        else
            if ! rm -f "$target"; then
                warn "could not remove newly installed file: $target"
                failed=true
            fi
        fi
    done
    [[ "$failed" == "false" ]]
}

rollback_directories() {
    local failed=false
    local i
    local path
    for ((i = ${#DIR_PATHS[@]} - 1; i >= 0; i--)); do
        path="${DIR_PATHS[$i]}"
        if [[ "${DIR_EXISTED[$i]}" != "true" ]]; then
            continue
        fi
        if ! chmod "${DIR_MODES[$i]}" "$path"; then
            warn "could not restore previous directory mode: $path"
            failed=true
        fi
        if [[ "$ROOT" == "/" ]] && ! chown "${DIR_UIDS[$i]}:${DIR_GIDS[$i]}" "$path"; then
            warn "could not restore previous directory owner: $path"
            failed=true
        fi
    done
    [[ "$failed" == "false" ]]
}

on_error() {
    local status="$1"
    local line="$2"
    local rollback_failed=false
    trap - ERR INT TERM HUP
    if [[ "$ERROR_HANDLING" == "true" ]]; then
        exit "$status"
    fi
    ERROR_HANDLING=true
    set +e
    if [[ "$ROLLBACK_ACTIVE" == "true" ]]; then
        warn "installation failed at line ${line}; restoring previous files and service state"
        if ! rollback_files; then rollback_failed=true; fi
        if ! rollback_directories; then rollback_failed=true; fi
        if ! restore_unit_state; then rollback_failed=true; fi
        if [[ "$rollback_failed" == "true" ]]; then
            warn "automatic rollback was incomplete; inspect managed files and run: systemctl daemon-reload"
            warn "then verify cpa-monitor.service and cpa-monitor.timer before re-enabling either mode"
        fi
    fi
    cleanup
    exit "$status"
}

record_target() {
    local target="$1"
    local index="${#TX_TARGETS[@]}"
    local backup="${TMP_DIR}/rollback/${index}"
    local existed
    if [[ -e "$target" ]]; then
        cp -p "$target" "$backup"
        existed=true
    else
        existed=false
    fi
    TX_TARGETS[$index]="$target"
    TX_BACKUPS[$index]="$backup"
    TX_EXISTED[$index]="$existed"
}

apply_owner() {
    local owner="$1"
    local group="$2"
    local path="$3"
    if [[ "$ROOT" == "/" ]]; then
        chown "${owner}:${group}" "$path"
    fi
}

replace_file() {
    local source="$1"
    local target="$2"
    local mode="$3"
    local owner="$4"
    local group="$5"
    local index
    local temporary

    [[ ! -L "$target" ]] || die "refusing to replace symlink: $target"
    if [[ -e "$target" ]] && cmp -s "$source" "$target"; then
        record_target "$target"
        chmod "$mode" "$target"
        apply_owner "$owner" "$group" "$target"
        return
    fi

    record_target "$target"
    index="${#TX_TARGETS[@]}"
    temporary="${target}.cpa-monitor-tmp.$$.$index"
    TX_TEMPS+=("$temporary")
    rm -f "$temporary"
    cp "$source" "$temporary"
    chmod "$mode" "$temporary"
    apply_owner "$owner" "$group" "$temporary"
    mv -f "$temporary" "$target"
}

unique_backup_path() {
    local target="$1"
    local backup_dir="$2"
    local stamp
    local candidate
    local suffix=0
    stamp="$(date '+%Y%m%d%H%M%S')"
    candidate="${backup_dir}/$(basename "$target").${stamp}"
    while [[ -e "$candidate" ]]; do
        suffix=$((suffix + 1))
        candidate="${backup_dir}/$(basename "$target").${stamp}.${suffix}"
    done
    printf '%s' "$candidate"
}

backup_managed_file() {
    local target="$1"
    local replacement="$2"
    local backup_dir="$3"
    local backup
    if [[ ! -e "$target" ]] || cmp -s "$target" "$replacement"; then
        return
    fi
    backup="$(unique_backup_path "$target" "$backup_dir")"
    cp -p "$target" "$backup"
    apply_owner root root "$backup"
    if [[ "$target" == "$TARGET_ENV_FILE" ]]; then chmod 0600 "$backup"; else chmod 0640 "$backup"; fi
    log "backed up $(basename "$target") to $backup"
}

safe_managed_directory() {
    local path="$1"
    local mode="$2"
    local owner="$3"
    local group="$4"
    [[ ! -L "$path" ]] || die "refusing managed directory symlink: $path"
    record_directory "$path"
    mkdir -p "$path"
    [[ -d "$path" ]] || die "managed path is not a directory: $path"
    chmod "$mode" "$path"
    apply_owner "$owner" "$group" "$path"
}

record_directory() {
    local path="$1"
    local index="${#DIR_PATHS[@]}"
    DIR_PATHS[$index]="$path"
    if [[ -e "$path" ]]; then
        DIR_EXISTED[$index]=true
        DIR_MODES[$index]="$(file_mode "$path")"
        if [[ "$ROOT" == "/" ]]; then
            DIR_UIDS[$index]="$(file_uid "$path")"
            DIR_GIDS[$index]="$(file_gid "$path")"
        else
            DIR_UIDS[$index]=""
            DIR_GIDS[$index]=""
        fi
    else
        DIR_EXISTED[$index]=false
        DIR_MODES[$index]=""
        DIR_UIDS[$index]=""
        DIR_GIDS[$index]=""
    fi
}

choose_nologin() {
    if [[ -x /usr/sbin/nologin ]]; then
        printf '%s' /usr/sbin/nologin
    elif [[ -x /sbin/nologin ]]; then
        printf '%s' /sbin/nologin
    else
        printf '%s' /bin/false
    fi
}

ensure_service_account() {
    local group_entry
    local group_fields
    local group_gid
    local nologin
    local uid
    if getent group "$SERVICE_GROUP" >/dev/null; then
        group_entry="$(getent group "$SERVICE_GROUP")"
        group_fields="${group_entry#*:}"
        group_fields="${group_fields#*:}"
        group_gid="${group_fields%%:*}"
        is_uint "$group_gid" && (( 10#$group_gid != 0 )) || die "existing ${SERVICE_GROUP} group must not be gid 0"
    else
        groupadd --system "$SERVICE_GROUP"
    fi
    if getent passwd "$SERVICE_USER" >/dev/null; then
        uid="$(id -u "$SERVICE_USER")"
        (( uid != 0 )) || die "existing ${SERVICE_USER} account must not be uid 0"
    else
        nologin="$(choose_nologin)"
        useradd --system --gid "$SERVICE_GROUP" --home-dir "$PROD_STATE_DIR" --shell "$nologin" --no-create-home "$SERVICE_USER"
    fi
}

prepare_directories() {
    local binary_dir
    local unit_dir
    binary_dir="$(dirname "$TARGET_BINARY")"
    unit_dir="$(dirname "$TARGET_SERVICE_UNIT")"
    mkdir -p "$binary_dir" "$unit_dir"

    safe_managed_directory "$TARGET_CONFIG_DIR" 0750 root "$SERVICE_GROUP"
    safe_managed_directory "$TARGET_STATE_DIR" 0750 "$SERVICE_USER" "$SERVICE_GROUP"
    safe_managed_directory "${TARGET_STATE_DIR}/state" 0750 "$SERVICE_USER" "$SERVICE_GROUP"
    safe_managed_directory "$TARGET_LOG_DIR" 0750 "$SERVICE_USER" "$SERVICE_GROUP"
    if [[ "$NEED_BACKUP_DIR" == "true" ]]; then
        safe_managed_directory "$TARGET_BACKUP_DIR" 0700 root root
    fi
}

check_target_shape() {
    local path="$1"
    [[ ! -L "$path" ]] || die "refusing managed symlink: $path"
    if [[ -e "$path" && ! -f "$path" ]]; then
        die "managed file path is not a regular file: $path"
    fi
}

preflight_platform() {
    local command_name
    if [[ "$ROOT" == "/" ]]; then
        [[ "$(uname -s)" == "Linux" ]] || die "production installation requires Linux"
        (( EUID == 0 )) || die "production installation must run as root (use sudo)"
        [[ -d /run/systemd/system ]] || die "systemd is not running on this host"
        for command_name in basename cp chmod chown cmp date dirname getent grep groupadd id mkdir mktemp mv rm sleep stat uname useradd; do
            command -v "$command_name" >/dev/null 2>&1 || die "required command not found: $command_name"
        done
        if [[ -z "$SYSTEMCTL" ]]; then SYSTEMCTL="$(command -v systemctl || true)"; fi
        [[ -n "$SYSTEMCTL" && -x "$SYSTEMCTL" ]] || die "systemctl was not found or is not executable"
        SYSTEMD_AVAILABLE=true
    elif [[ -n "$TEST_SYSTEMCTL" ]]; then
        SYSTEMCTL="$TEST_SYSTEMCTL"
        [[ -x "$SYSTEMCTL" ]] || die "test systemctl stub is not executable"
        SYSTEMD_AVAILABLE=true
    fi

    if [[ -z "$FLOCK_BIN" ]]; then
        if [[ "$ROOT" == "/" ]]; then
            FLOCK_BIN="$(command -v flock || true)"
        else
            FLOCK_BIN="/usr/bin/flock"
        fi
    fi
    [[ "$FLOCK_BIN" == /* ]] || die "flock executable path must be absolute"
    [[ "$FLOCK_BIN" =~ ^/[A-Za-z0-9_./+-]+$ ]] || die "flock executable path contains unsupported characters"
    if [[ "$ROOT" == "/" ]]; then
        [[ -x "$FLOCK_BIN" ]] || die "flock is required (normally provided by util-linux)"
    fi
}

acquire_install_lock() {
    if [[ "$ROOT" == "/" ]]; then
        exec 9>/run/cpa-monitor-install.lock
        "$FLOCK_BIN" -n 9 || die "another cpa-monitor installation is running"
    fi
}

stage_binary() {
    local go_bin
    STAGE_BINARY="${TMP_DIR}/cpa-monitor"
    if [[ -n "$BINARY_SOURCE" ]]; then
        cp "$BINARY_SOURCE" "$STAGE_BINARY"
    else
        [[ -f "${SCRIPT_DIR}/go.mod" ]] || die "go.mod not found next to install.sh; use --binary"
        go_bin="${GO:-}"
        if [[ -z "$go_bin" ]]; then go_bin="$(command -v go || true)"; fi
        [[ -n "$go_bin" && -x "$go_bin" ]] || die "Go 1.26+ is required for a source build; alternatively use --binary"
        if [[ "$SKIP_TESTS" != "true" ]]; then
            log "running Go tests"
            (cd "$SCRIPT_DIR" && GOWORK=off GOTOOLCHAIN=local "$go_bin" test -mod=readonly ./...)
        fi
        log "building static cpa-monitor binary"
        (cd "$SCRIPT_DIR" && GOWORK=off GOTOOLCHAIN=local CGO_ENABLED=0 "$go_bin" build -mod=readonly -trimpath -ldflags='-s -w' -o "$STAGE_BINARY" ./cmd/cpa-monitor)
    fi
    chmod 0755 "$STAGE_BINARY"
    "$STAGE_BINARY" --help >/dev/null 2>&1 || die "candidate binary failed the --help smoke test (wrong platform or invalid executable)"
}

stage_assets() {
    STAGE_CONFIG="${TMP_DIR}/config.yaml"
    STAGE_ENV_FILE="${TMP_DIR}/cpa-monitor.env"
    STAGE_SERVICE_UNIT="${TMP_DIR}/cpa-monitor.service"
    STAGE_ONESHOT_UNIT="${TMP_DIR}/cpa-monitor-once.service"
    STAGE_CHECK_UNIT="${TMP_DIR}/cpa-monitor-check.service"
    STAGE_TIMER_UNIT="${TMP_DIR}/cpa-monitor.timer"

    case "$CONFIG_ACTION" in
        copy) cp "$CONFIG_SOURCE" "$STAGE_CONFIG" ;;
        generate) render_config >"$STAGE_CONFIG" ;;
        preserve) cp "$TARGET_CONFIG" "$STAGE_CONFIG" ;;
    esac
    case "$ENV_ACTION" in
        copy) cp "$ENV_SOURCE" "$STAGE_ENV_FILE" ;;
        generate) render_environment >"$STAGE_ENV_FILE" ;;
        preserve) cp "$TARGET_ENV_FILE" "$STAGE_ENV_FILE" ;;
    esac
    chmod 0640 "$STAGE_CONFIG"
    chmod 0600 "$STAGE_ENV_FILE"

    render_daemon_unit >"$STAGE_SERVICE_UNIT"
    render_oneshot_unit >"$STAGE_ONESHOT_UNIT"
    render_check_unit >"$STAGE_CHECK_UNIT"
    render_timer_unit >"$STAGE_TIMER_UNIT"
    chmod 0644 "$STAGE_SERVICE_UNIT" "$STAGE_ONESHOT_UNIT" "$STAGE_CHECK_UNIT" "$STAGE_TIMER_UNIT"
}

validate_generated_config() {
    if [[ "$CONFIG_ACTION" == "generate" && "$ENV_ACTION" == "generate" ]]; then
        log "validating generated configuration without network access"
        CPA_MANAGEMENT_KEY="$MANAGEMENT_KEY" \
        CPA_SMTP_USERNAME="$SMTP_USERNAME" \
        CPA_SMTP_PASSWORD="$SMTP_PASSWORD" \
        "$STAGE_BINARY" --config "$STAGE_CONFIG" --check-config >/dev/null
    else
        warn "copied or existing configuration will be validated when the selected service starts"
    fi
}

install_assets() {
    if [[ "$NEED_BACKUP_DIR" == "true" ]]; then
        if [[ "$CONFIG_ACTION" != "preserve" ]]; then backup_managed_file "$TARGET_CONFIG" "$STAGE_CONFIG" "$TARGET_BACKUP_DIR"; fi
        if [[ "$ENV_ACTION" != "preserve" ]]; then backup_managed_file "$TARGET_ENV_FILE" "$STAGE_ENV_FILE" "$TARGET_BACKUP_DIR"; fi
    fi

    replace_file "$STAGE_BINARY" "$TARGET_BINARY" 0755 root root
    replace_file "$STAGE_SERVICE_UNIT" "$TARGET_SERVICE_UNIT" 0644 root root
    replace_file "$STAGE_ONESHOT_UNIT" "$TARGET_ONESHOT_UNIT" 0644 root root
    replace_file "$STAGE_CHECK_UNIT" "$TARGET_CHECK_UNIT" 0644 root root
    replace_file "$STAGE_TIMER_UNIT" "$TARGET_TIMER_UNIT" 0644 root root
    replace_file "$STAGE_CONFIG" "$TARGET_CONFIG" 0640 root "$SERVICE_GROUP"
    replace_file "$STAGE_ENV_FILE" "$TARGET_ENV_FILE" 0600 root root
}

verify_units() {
    if [[ "$ROOT" == "/" ]] && command -v systemd-analyze >/dev/null 2>&1; then
        systemd-analyze verify "$TARGET_SERVICE_UNIT" "$TARGET_ONESHOT_UNIT" "$TARGET_CHECK_UNIT" "$TARGET_TIMER_UNIT"
    fi
}

wait_for_stable_active() {
    local unit="$1"
    "$SYSTEMCTL" is-active --quiet "$unit" || return $?
    sleep 1
    "$SYSTEMCTL" is-active --quiet "$unit" || return $?
}

activate_mode() {
    [[ "$SYSTEMD_AVAILABLE" == "true" ]] || return 0
    "$SYSTEMCTL" daemon-reload || return $?
    if [[ "$NO_START" == "true" ]]; then
        return
    fi
    "$SYSTEMCTL" start cpa-monitor-check.service || return $?
    if [[ "$MODE" == "daemon" ]]; then
        "$SYSTEMCTL" disable --now cpa-monitor.timer || return $?
        "$SYSTEMCTL" stop cpa-monitor-once.service || return $?
        "$SYSTEMCTL" enable cpa-monitor.service || return $?
        "$SYSTEMCTL" restart cpa-monitor.service || return $?
        wait_for_stable_active cpa-monitor.service || return $?
    else
        "$SYSTEMCTL" disable --now cpa-monitor.service || return $?
        "$SYSTEMCTL" stop cpa-monitor-once.service || return $?
        "$SYSTEMCTL" enable cpa-monitor.timer || return $?
        "$SYSTEMCTL" restart cpa-monitor.timer || return $?
        wait_for_stable_active cpa-monitor.timer || return $?
    fi
}

show_failure_diagnostics() {
    [[ "$ROOT" == "/" && "$SYSTEMD_AVAILABLE" == "true" ]] || return 0
    "$SYSTEMCTL" status --no-pager --full cpa-monitor-check.service >&2 || true
    journalctl -u cpa-monitor-check.service -n 100 --no-pager >&2 || true
    if [[ "$MODE" == "daemon" ]]; then
        "$SYSTEMCTL" status --no-pager --full cpa-monitor.service >&2 || true
        journalctl -u cpa-monitor.service -n 100 --no-pager >&2 || true
    else
        "$SYSTEMCTL" status --no-pager --full cpa-monitor.timer cpa-monitor-once.service >&2 || true
        journalctl -u cpa-monitor-once.service -n 100 --no-pager >&2 || true
    fi
}

print_summary() {
    log "installation completed"
    printf '\nInstalled files:\n'
    printf '  binary: %s\n' "$TARGET_BINARY"
    printf '  config: %s\n' "$TARGET_CONFIG"
    printf '  secrets: %s\n' "$TARGET_ENV_FILE"
    printf '  state: %s/state/alerts.json\n' "$TARGET_STATE_DIR"
    printf '  logs: %s/monitor.log (when file logging is enabled)\n' "$TARGET_LOG_DIR"

    if [[ "$ROOT" != "/" ]]; then
        printf '\nStaged below %s; systemd unit paths intentionally target production locations.\n' "$ROOT"
        return
    fi

    printf '\nCommon commands:\n'
    printf '  sudo systemctl start cpa-monitor-check.service\n'
    if [[ "$MODE" == "daemon" ]]; then
        printf '  sudo systemctl status --no-pager cpa-monitor.service\n'
        printf '  sudo systemctl restart cpa-monitor.service\n'
        printf '  sudo journalctl -u cpa-monitor.service -f\n'
    else
        printf '  sudo systemctl status --no-pager cpa-monitor.timer cpa-monitor-once.service\n'
        printf '  sudo systemctl list-timers --all cpa-monitor.timer\n'
        printf '  sudo systemctl start cpa-monitor-once.service\n'
        printf '  sudo journalctl -u cpa-monitor-once.service -f\n'
    fi
    printf '\nAfter editing config or secrets, run cpa-monitor-check.service and restart the active unit.\n'
    if [[ "$NO_START" == "true" ]]; then
        printf '\nNothing was started. To activate this mode:\n'
        if [[ "$MODE" == "daemon" ]]; then
            printf '  sudo systemctl disable --now cpa-monitor.timer\n'
            printf '  sudo systemctl enable --now cpa-monitor.service\n'
        else
            printf '  sudo systemctl disable --now cpa-monitor.service\n'
            printf '  sudo systemctl enable --now cpa-monitor.timer\n'
        fi
    fi
}

main() {
    local activation_status=0
    local need_inputs=false

    parse_args "$@"
    [[ "$MODE" == "daemon" || "$MODE" == "timer" ]] || die "--mode must be daemon or timer"
    validate_timer_interval
    [[ "$ROOT" == /* ]] || die "--root must be an absolute path"
    if [[ "$ROOT" != "/" ]]; then ROOT="${ROOT%/}"; fi

    TARGET_BINARY="$(root_path "$PROD_BINARY")"
    TARGET_CONFIG_DIR="$(root_path "$PROD_CONFIG_DIR")"
    TARGET_CONFIG="$(root_path "$PROD_CONFIG")"
    TARGET_ENV_FILE="$(root_path "$PROD_ENV_FILE")"
    TARGET_STATE_DIR="$(root_path "$PROD_STATE_DIR")"
    TARGET_LOG_DIR="$(root_path "$PROD_LOG_DIR")"
    TARGET_SERVICE_UNIT="$(root_path "${PROD_UNIT_DIR}/cpa-monitor.service")"
    TARGET_ONESHOT_UNIT="$(root_path "${PROD_UNIT_DIR}/cpa-monitor-once.service")"
    TARGET_CHECK_UNIT="$(root_path "${PROD_UNIT_DIR}/cpa-monitor-check.service")"
    TARGET_TIMER_UNIT="$(root_path "${PROD_UNIT_DIR}/cpa-monitor.timer")"
    TARGET_BACKUP_DIR="${TARGET_CONFIG_DIR}/backups"

    preflight_platform
    acquire_install_lock

    if [[ -n "$BINARY_SOURCE" ]]; then BINARY_SOURCE="$(absolute_file "$BINARY_SOURCE")"; fi
    if [[ -n "$CONFIG_SOURCE" ]]; then CONFIG_SOURCE="$(absolute_file "$CONFIG_SOURCE")"; fi
    if [[ -n "$ENV_SOURCE" ]]; then
        ENV_SOURCE="$(absolute_file "$ENV_SOURCE")"
        validate_env_source_permissions "$ENV_SOURCE"
    fi

    check_target_shape "$TARGET_BINARY"
    check_target_shape "$TARGET_CONFIG"
    check_target_shape "$TARGET_ENV_FILE"
    check_target_shape "$TARGET_SERVICE_UNIT"
    check_target_shape "$TARGET_ONESHOT_UNIT"
    check_target_shape "$TARGET_CHECK_UNIT"
    check_target_shape "$TARGET_TIMER_UNIT"
    [[ ! -L "$TARGET_CONFIG_DIR" ]] || die "refusing managed directory symlink: $TARGET_CONFIG_DIR"
    [[ ! -L "$TARGET_STATE_DIR" ]] || die "refusing managed directory symlink: $TARGET_STATE_DIR"
    [[ ! -L "$TARGET_LOG_DIR" ]] || die "refusing managed directory symlink: $TARGET_LOG_DIR"

    if [[ -n "$CONFIG_SOURCE" ]]; then
        CONFIG_ACTION="copy"
    elif [[ "$FORCE_CONFIG" == "true" || ! -e "$TARGET_CONFIG" ]]; then
        CONFIG_ACTION="generate"
        need_inputs=true
    else
        CONFIG_ACTION="preserve"
    fi
    if [[ -n "$ENV_SOURCE" ]]; then
        ENV_ACTION="copy"
    elif [[ "$FORCE_CONFIG" == "true" || ! -e "$TARGET_ENV_FILE" ]]; then
        ENV_ACTION="generate"
        need_inputs=true
    else
        ENV_ACTION="preserve"
    fi

    NEED_BACKUP_DIR=false
    if [[ "$CONFIG_ACTION" != "preserve" && -e "$TARGET_CONFIG" ]] || [[ "$ENV_ACTION" != "preserve" && -e "$TARGET_ENV_FILE" ]]; then
        NEED_BACKUP_DIR=true
    fi

    load_generation_values
    if [[ "$need_inputs" == "true" && "$NON_INTERACTIVE" != "true" ]]; then
        [[ -t 0 ]] || die "interactive input is unavailable; pass --non-interactive with CPA_MONITOR_* variables or provide --config and --env-file"
        collect_interactive_values
    fi
    validate_generation_values

    TMP_DIR="$(mktemp -d "${TMPDIR:-/tmp}/cpa-monitor-install.XXXXXX")"
    mkdir -p "${TMP_DIR}/rollback"
    trap cleanup EXIT

    stage_binary
    stage_assets
    validate_generated_config

    if [[ "$ROOT" == "/" ]]; then
        ensure_service_account
    fi
    capture_unit_state

    ROLLBACK_ACTIVE=true
    trap 'on_error $? $LINENO' ERR
    trap 'on_error 130 $LINENO' INT
    trap 'on_error 143 $LINENO' TERM
    trap 'on_error 129 $LINENO' HUP
    prepare_directories
    install_assets
    verify_units
    if activate_mode; then
        :
    else
        activation_status=$?
        show_failure_diagnostics
        on_error "$activation_status" "$LINENO"
    fi
    ROLLBACK_ACTIVE=false
    trap - ERR INT TERM HUP
    print_summary
}

if [[ "${BASH_SOURCE[0]}" == "$0" ]]; then
    main "$@"
fi
