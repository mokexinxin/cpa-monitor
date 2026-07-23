#!/usr/bin/env bash

# Collect a credential-safe CPA Monitor/DingTalk diagnostic report.
# The default mode is read-only. --run-once explicitly starts a real monitoring
# cycle (or restarts the daemon, whose first cycle runs immediately).

set +x
set -Eeuo pipefail
umask 077

PROGRAM="cpa-monitor-dingtalk-diagnostics"
SCRIPT_VERSION="1.0"

BINARY="${CPA_MONITOR_DIAG_BINARY:-/usr/local/bin/cpa-monitor}"
CONFIG="${CPA_MONITOR_DIAG_CONFIG:-/etc/cpa-monitor/config.yaml}"
ENV_FILE="${CPA_MONITOR_DIAG_ENV_FILE:-/etc/cpa-monitor/cpa-monitor.env}"
STATE_FILE="${CPA_MONITOR_DIAG_STATE_FILE:-}"
SINCE="${CPA_MONITOR_DIAG_SINCE:-24 hours ago}"
OUTPUT=""
RUN_ONCE=false

SYSTEMCTL="${CPA_MONITOR_DIAG_SYSTEMCTL:-systemctl}"
JOURNALCTL="${CPA_MONITOR_DIAG_JOURNALCTL:-journalctl}"

ACTIVE_COUNT="unknown"
PRIMARY_CHANNEL=""
FALLBACK_CHANNEL=""
HEALTH_ENABLED=""
HEALTH_CHANNEL=""
BINARY_SUPPORTS_DINGTALK="unknown"
DAEMON_ACTIVE="unknown"
TIMER_ACTIVE="unknown"
FILTERED_LOGS=""
TOKEN_ENV_STATUS="unknown"
SECRET_ENV_STATUS="unknown"
TOKEN_INLINE_STATUS="unknown"
SECRET_INLINE_STATUS="unknown"

usage() {
    cat <<'EOF'
Usage: sudo bash scripts/diagnose-dingtalk.sh [options]

Collect a redacted report explaining why DingTalk transport tests may succeed
while real monitoring alerts do not arrive. The default mode does not send a
message, restart a service, change alert state, or print credential values.

Options:
  --binary PATH       Installed cpa-monitor binary
                      (default: /usr/local/bin/cpa-monitor)
  --config PATH       Installed YAML config
                      (default: /etc/cpa-monitor/config.yaml)
  --env-file PATH     systemd EnvironmentFile
                      (default: /etc/cpa-monitor/cpa-monitor.env)
  --state PATH        Override alert-state path; otherwise read alerts.state_file
  --since SPAN        Journal window accepted by journalctl
                      (default: "24 hours ago")
  --output PATH       Also save the report to PATH with mode 0600
  --run-once          ACTIVE mode: trigger a real monitoring cycle. If the
                      daemon is active it is restarted; otherwise the installed
                      cpa-monitor-once.service is started. This may send alerts
                      and update alerts.json.
  -h, --help          Show this help

Recommended first run:
  sudo bash scripts/diagnose-dingtalk.sh \
    --output /tmp/cpa-monitor-dingtalk-diagnosis.txt

Do not post config.yaml, cpa-monitor.env, or raw alerts.json. This script only
reports selected non-secret settings and a sanitized state/log summary.
EOF
}

die() {
    printf '[diagnostic] ERROR: %s\n' "$*" >&2
    exit 2
}

require_argument() {
    if (( $# < 2 )); then
        die "$1 requires an argument"
    fi
}

parse_args() {
    while (( $# > 0 )); do
        case "$1" in
            --binary)
                require_argument "$@"
                BINARY="$2"
                shift 2
                ;;
            --binary=*) BINARY="${1#*=}"; shift ;;
            --config)
                require_argument "$@"
                CONFIG="$2"
                shift 2
                ;;
            --config=*) CONFIG="${1#*=}"; shift ;;
            --env-file)
                require_argument "$@"
                ENV_FILE="$2"
                shift 2
                ;;
            --env-file=*) ENV_FILE="${1#*=}"; shift ;;
            --state)
                require_argument "$@"
                STATE_FILE="$2"
                shift 2
                ;;
            --state=*) STATE_FILE="${1#*=}"; shift ;;
            --since)
                require_argument "$@"
                SINCE="$2"
                shift 2
                ;;
            --since=*) SINCE="${1#*=}"; shift ;;
            --output)
                require_argument "$@"
                OUTPUT="$2"
                shift 2
                ;;
            --output=*) OUTPUT="${1#*=}"; shift ;;
            --run-once)
                RUN_ONCE=true
                shift
                ;;
            -h|--help)
                usage
                exit 0
                ;;
            *) die "unknown option: $1" ;;
        esac
    done

    case "$SINCE$OUTPUT$BINARY$CONFIG$ENV_FILE$STATE_FILE" in
        *$'\n'*|*$'\r'*) die "paths and --since must not contain newlines" ;;
    esac
}

have_command() {
    command -v "$1" >/dev/null 2>&1
}

section() {
    printf '\n===== %s =====\n' "$1"
}

item() {
    printf '%-28s %s\n' "$1:" "${2:-<empty>}"
}

# Redact credential patterns that could appear in an HTTP/environment error.
# The report intentionally never prints config/env file contents.
redact() {
    sed -E \
        -e 's/(access_token=)[^&[:space:]]+/\1[REDACTED]/g' \
        -e 's/(CPA_[A-Z0-9_]*(KEY|TOKEN|SECRET|PASSWORD)=)[^[:space:]]+/\1[REDACTED]/g' \
        -e 's/(Authorization:?[[:space:]]*(Bearer|Basic)[[:space:]]+)[^[:space:]]+/\1[REDACTED]/g' \
        -e 's/(webhook_token|signing_secret|management_key|password)([=:][[:space:]]*)[^,[:space:]]+/\1\2[REDACTED]/g'
}

file_metadata() {
    local path="$1"
    if [[ ! -e "$path" ]]; then
        printf 'missing: %s\n' "$path"
        return
    fi
    if stat -c 'path=%n owner=%U:%G mode=%a size=%s modified=%y' "$path" >/dev/null 2>&1; then
        stat -c 'path=%n owner=%U:%G mode=%a size=%s modified=%y' "$path"
    else
        stat -f 'path=%N owner=%Su:%Sg mode=%Lp size=%z modified=%Sm' "$path"
    fi
}

# Return one scalar from a simple top-level YAML mapping. Only known non-secret
# keys are requested. This is deliberately not a general YAML dumper.
yaml_value() {
    local section_name="$1"
    local key_name="$2"
    local file="$3"
    [[ -r "$file" ]] || return 0
    awk -v wanted_section="$section_name" -v wanted_key="$key_name" '
        function trim(value) {
            sub(/^[[:space:]]+/, "", value)
            sub(/[[:space:]]+$/, "", value)
            return value
        }
        /^[A-Za-z_][A-Za-z0-9_]*:[[:space:]]*($|#)/ {
            current = $0
            sub(/:.*/, "", current)
            next
        }
        current == wanted_section && /^  [A-Za-z_][A-Za-z0-9_]*[[:space:]]*:/ {
            line = $0
            sub(/^  /, "", line)
            parsed_key = line
            sub(/[[:space:]]*:.*/, "", parsed_key)
            if (parsed_key != wanted_key) next
            sub(/^[^:]*:/, "", line)
            sub(/[[:space:]]+#.*/, "", line)
            line = trim(line)
            if ((substr(line, 1, 1) == "\"" && substr(line, length(line), 1) == "\"") ||
                (substr(line, 1, 1) == "\047" && substr(line, length(line), 1) == "\047")) {
                line = substr(line, 2, length(line) - 2)
            }
            print line
            exit
        }
    ' "$file"
}

yaml_root_value() {
    local key_name="$1"
    local file="$2"
    [[ -r "$file" ]] || return 0
    awk -v wanted_key="$key_name" '
        /^[A-Za-z_][A-Za-z0-9_]*[[:space:]]*:/ {
            line = $0
            parsed_key = line
            sub(/[[:space:]]*:.*/, "", parsed_key)
            if (parsed_key != wanted_key) next
            sub(/^[^:]*:/, "", line)
            sub(/[[:space:]]+#.*/, "", line)
            sub(/^[[:space:]]+/, "", line)
            sub(/[[:space:]]+$/, "", line)
            if ((substr(line, 1, 1) == "\"" && substr(line, length(line), 1) == "\"") ||
                (substr(line, 1, 1) == "\047" && substr(line, length(line), 1) == "\047")) {
                line = substr(line, 2, length(line) - 2)
            }
            print line
            exit
        }
    ' "$file"
}

yaml_secret_status() {
    local section_name="$1"
    local key_name="$2"
    local value
    value="$(yaml_value "$section_name" "$key_name" "$CONFIG")"
    if [[ -n "$value" ]]; then
        printf 'set inline (value not displayed; env is safer)'
    else
        printf 'not set inline'
    fi
}

env_assignment_status() {
    local variable_name="$1"
    if [[ -z "$variable_name" ]]; then
        printf 'no env variable configured'
        return
    fi
    case "$variable_name" in
        *[!A-Za-z0-9_]*|[0-9]*)
            printf 'invalid env variable name: %s' "$variable_name"
            return
            ;;
    esac
    if [[ ! -r "$ENV_FILE" ]]; then
        printf '%s: env file unreadable' "$variable_name"
    elif grep -Eq "(^[[:space:]]*${variable_name}[[:space:]]*=[[:space:]]*$)|(^[[:space:]]*${variable_name}[[:space:]]*=[[:space:]]*(\"\"|'')[[:space:]]*$)" "$ENV_FILE"; then
        printf '%s: assignment present but EMPTY' "$variable_name"
    elif grep -Eq "^[[:space:]]*${variable_name}[[:space:]]*=" "$ENV_FILE"; then
        printf '%s: assignment present (value not displayed)' "$variable_name"
    else
        printf '%s: assignment MISSING' "$variable_name"
    fi
}

systemctl_value() {
    local action="$1"
    local unit="$2"
    "$SYSTEMCTL" "$action" "$unit" 2>/dev/null || true
}

show_unit() {
    local unit="$1"
    local active enabled
    active="$(systemctl_value is-active "$unit")"
    enabled="$(systemctl_value is-enabled "$unit")"
    printf '%s: active=%s enabled=%s\n' "$unit" "${active:-unknown}" "${enabled:-unknown}"
    "$SYSTEMCTL" show "$unit" \
        -p LoadState -p ActiveState -p SubState -p Result -p ExecMainStatus \
        -p ExecMainStartTimestamp -p MainPID -p FragmentPath -p User -p Group \
        -p WorkingDirectory -p EnvironmentFiles -p ExecStartPre -p ExecStart \
        2>/dev/null | redact || true
}

inspect_state() {
    local report
    if [[ ! -e "$STATE_FILE" ]]; then
        ACTIVE_COUNT=0
        printf 'state file does not exist yet: %s\n' "$STATE_FILE"
        printf 'active_count=0 (no persisted alert state)\n'
        return
    fi
    file_metadata "$STATE_FILE"
    if [[ ! -r "$STATE_FILE" ]]; then
        ACTIVE_COUNT="unknown"
        printf 'state file is not readable; run this script with sudo\n'
        return
    fi

    if have_command python3; then
        report="$(python3 - "$STATE_FILE" <<'PY'
import collections
import json
import sys

def safe(value, limit=180):
    text = str(value).replace("\r", " ").replace("\n", " ").replace("\t", " ")
    return text[:limit]

try:
    with open(sys.argv[1], "r", encoding="utf-8") as handle:
        document = json.load(handle)
    active = document.get("active", [])
    if not isinstance(active, list):
        raise ValueError("active is not a list")
    print("__ACTIVE_COUNT__=" + str(len(active)))
    print("schema_version=" + safe(document.get("version", "missing")))
    print("active_count=" + str(len(active)))
    scopes = collections.Counter(safe(record.get("scope", "missing")) for record in active if isinstance(record, dict))
    if scopes:
        print("active_by_scope=" + ", ".join(f"{scope}:{count}" for scope, count in sorted(scopes.items())))
    for index, record in enumerate(active[:30], start=1):
        if not isinstance(record, dict):
            print(f"active[{index}]=invalid record")
            continue
        print(
            f"active[{index}] scope={safe(record.get('scope', 'missing'))} "
            f"key={safe(record.get('key', 'missing'))} "
            f"activated_at={safe(record.get('activated_at', 'missing'))}"
        )
    if len(active) > 30:
        print(f"active_omitted={len(active) - 30}")
    health = document.get("health_report")
    if isinstance(health, dict):
        print("health_last_attempt_at=" + safe(health.get("last_attempt_at", "missing")))
        print("health_last_sent_at=" + safe(health.get("last_sent_at", "missing")))
except Exception as exc:
    print("__ACTIVE_COUNT__=unknown")
    print("state_parse_error=" + safe(exc))
PY
)"
        ACTIVE_COUNT="$(printf '%s\n' "$report" | awk -F= '/^__ACTIVE_COUNT__=/{print $2; exit}')"
        printf '%s\n' "$report" | awk '!/^__ACTIVE_COUNT__=/'
    elif have_command jq; then
        ACTIVE_COUNT="$(jq -r 'if (.active | type) == "array" then (.active | length) else "unknown" end' "$STATE_FILE" 2>/dev/null || printf unknown)"
        jq -r '
            "schema_version=\(.version // "missing")",
            "active_count=\(if (.active | type) == "array" then (.active | length) else "unknown" end)",
            (.active[:30][]? | "active scope=\(.scope // "missing") key=\(.key // "missing") activated_at=\(.activated_at // "missing")")
        ' "$STATE_FILE" 2>/dev/null || printf 'state_parse_error=invalid JSON\n'
    else
        ACTIVE_COUNT="unknown"
        printf 'state parser unavailable (install python3 or jq)\n'
    fi
}

trigger_real_cycle() {
    section "ACTIVE DIAGNOSTIC ACTION"
    printf 'WARNING: --run-once may send alerts and update %s.\n' "$STATE_FILE"
    if ! have_command "$SYSTEMCTL"; then
        printf 'Cannot trigger a cycle: systemctl is unavailable.\n'
        return
    fi
    local daemon_state
    daemon_state="$(systemctl_value is-active cpa-monitor.service)"
    if [[ "$daemon_state" == "active" || "$daemon_state" == "activating" ]]; then
        printf 'The daemon owns the state lock; restarting it to trigger its immediate first cycle.\n'
        if "$SYSTEMCTL" restart cpa-monitor.service; then
            printf 'Daemon restart accepted. The check may still be running asynchronously.\n'
        else
            printf 'Daemon restart FAILED (exit=%s).\n' "$?"
        fi
    else
        printf 'Starting cpa-monitor-once.service and waiting for it to finish.\n'
        if "$SYSTEMCTL" start cpa-monitor-once.service; then
            printf 'One-shot monitoring cycle completed successfully.\n'
        else
            printf 'One-shot monitoring cycle FAILED (exit=%s).\n' "$?"
        fi
    fi
}

collect_logs() {
    section "RECENT RELEVANT JOURNAL"
    if ! have_command "$JOURNALCTL"; then
        printf 'journalctl is unavailable\n'
        FILTERED_LOGS=""
        return
    fi
    local raw
    raw="$("$JOURNALCTL" --no-pager -o short-iso --since "$SINCE" \
        -u cpa-monitor.service -u cpa-monitor-once.service \
        -u cpa-monitor-check.service 2>/dev/null || true)"
    FILTERED_LOGS="$(printf '%s\n' "$raw" | grep -Ei \
        'monitor condition detected|monitor check failed|monitor reconciliation failed|healthy report|notification|dingtalk|failed with result|start request repeated|configuration valid|config|flock|status=75' || true)"
    if [[ -z "$FILTERED_LOGS" ]]; then
        printf 'No matching monitor/config/delivery log lines since %s.\n' "$SINCE"
    else
        printf '%s\n' "$FILTERED_LOGS" | tail -n 250 | redact
    fi
}

print_analysis() {
    section "AUTOMATIC ANALYSIS"
    local finding_count=0

    if [[ "$BINARY_SUPPORTS_DINGTALK" != "yes" ]]; then
        printf '[HIGH] The installed binary did not advertise --test-notification dingtalk; an old binary or wrong path may be running.\n'
        finding_count=$((finding_count + 1))
    fi
    if [[ "$PRIMARY_CHANNEL" != "dingtalk" ]]; then
        printf '[HIGH] alerts.primary_channel resolves to %s, not dingtalk. Real alerts therefore do not use DingTalk as primary.\n' "${PRIMARY_CHANNEL:-<missing/default smtp>}"
        finding_count=$((finding_count + 1))
    fi
    if [[ "$TOKEN_ENV_STATUS" == *"EMPTY"* || "$SECRET_ENV_STATUS" == *"EMPTY"* ]]; then
        printf '[HIGH] A configured DingTalk credential environment assignment is empty; systemd overrides any inline value with the empty assignment.\n'
        finding_count=$((finding_count + 1))
    fi
    if [[ "$TOKEN_INLINE_STATUS" == "not set inline" && "$TOKEN_ENV_STATUS" != *"assignment present (value not displayed)"* ]]; then
        printf '[HIGH] No usable DingTalk webhook token source was confirmed.\n'
        finding_count=$((finding_count + 1))
    fi
    if [[ "$SECRET_INLINE_STATUS" == "not set inline" && "$SECRET_ENV_STATUS" != *"assignment present (value not displayed)"* ]]; then
        printf '[HIGH] No usable DingTalk signing-secret source was confirmed.\n'
        finding_count=$((finding_count + 1))
    fi
    if [[ -n "$FALLBACK_CHANNEL" ]]; then
        printf '[INFO] alerts.fallback_channel=%s. If you require DingTalk-only delivery, set it to an empty string.\n' "$FALLBACK_CHANNEL"
    fi
    if [[ "$DAEMON_ACTIVE" != "active" && "$DAEMON_ACTIVE" != "activating" && "$TIMER_ACTIVE" != "active" && "$TIMER_ACTIVE" != "activating" ]]; then
        printf '[HIGH] Neither daemon nor timer appears active, so scheduled monitoring is not running.\n'
        finding_count=$((finding_count + 1))
    fi
    if [[ "$ACTIVE_COUNT" =~ ^[0-9]+$ ]] && (( ACTIVE_COUNT > 0 )); then
        printf '[HIGH/LIKELY] alerts.json already contains %s active condition(s). CPA Monitor sends only on a new transition into alert state; switching SMTP to DingTalk does not replay conditions already active.\n' "$ACTIVE_COUNT"
        printf '              Do not delete alerts.json blindly: that can resend every active condition and lose recovery history.\n'
        finding_count=$((finding_count + 1))
    fi
    if printf '%s\n' "$FILTERED_LOGS" | grep -Eqi 'monitor reconciliation failed|dingtalk.*(fail|error)|notification.*(fail|error)'; then
        printf '[HIGH] Recent logs contain a reconciliation/DingTalk delivery failure. Inspect the redacted journal lines above for the exact network/API/config error.\n'
        finding_count=$((finding_count + 1))
    elif printf '%s\n' "$FILTERED_LOGS" | grep -Eqi 'monitor condition detected'; then
        if [[ "$ACTIVE_COUNT" =~ ^[0-9]+$ ]] && (( ACTIVE_COUNT > 0 )); then
            printf '[INFO] Recent checks still detect a condition, while persisted active state exists. This strongly matches normal duplicate suppression.\n'
        else
            printf '[INFO] A condition was detected. With no delivery failure logged, compare its time with state-file modification and service restarts.\n'
        fi
    elif [[ "$ACTIVE_COUNT" == "0" ]]; then
        printf '[INFO] No active state and no recent condition log were found. The host/API likely did not cross an alert threshold during this journal window.\n'
    fi
    if [[ "$HEALTH_ENABLED" == "true" || "$HEALTH_ENABLED" == "" ]]; then
        printf '[INFO] Healthy reports are scheduled separately and are sent only when all five checks are complete, error-free, have no active conditions, and the report interval is due.\n'
    fi
    if (( finding_count == 0 )); then
        printf '[INFO] No single configuration/state cause was proven automatically. Use --run-once once, then rerun with --since "10 minutes ago".\n'
    fi

    printf '\nTransport-test interpretation: a successful --test-notification dingtalk proves the robot credentials and outbound HTTPS path only. It does not prove that the scheduler ran, a threshold was crossed, or the condition was new.\n'
}

main() {
    parse_args "$@"

    if [[ -n "$OUTPUT" ]]; then
        : >"$OUTPUT" || die "cannot create output file: $OUTPUT"
        chmod 600 "$OUTPUT" || die "cannot chmod output file: $OUTPUT"
        exec > >(tee "$OUTPUT") 2>&1
    fi

    printf '%s v%s\n' "$PROGRAM" "$SCRIPT_VERSION"
    item "generated_at" "$(date -u '+%Y-%m-%dT%H:%M:%SZ')"
    item "hostname" "$(hostname 2>/dev/null || printf unknown)"
    item "effective_user" "$(id -un 2>/dev/null || printf unknown) (uid=$(id -u 2>/dev/null || printf unknown))"
    item "journal_window" "$SINCE"
    if [[ "$(id -u 2>/dev/null || printf 1)" != "0" ]]; then
        printf 'WARNING: not running as root; env/state/journal checks may be incomplete.\n'
    fi

    section "INSTALLED BINARY"
    if [[ -x "$BINARY" ]]; then
        file_metadata "$BINARY"
        if have_command sha256sum; then
            sha256sum "$BINARY" | awk '{print "sha256=" $1}'
        elif have_command shasum; then
            shasum -a 256 "$BINARY" | awk '{print "sha256=" $1}'
        fi
        local help_output
        help_output="$("$BINARY" --help 2>&1 || true)"
        if printf '%s\n' "$help_output" | grep -q -- '--test-notification'; then
            BINARY_SUPPORTS_DINGTALK="yes"
        else
            BINARY_SUPPORTS_DINGTALK="no"
        fi
        item "supports DingTalk CLI" "$BINARY_SUPPORTS_DINGTALK"
    else
        printf 'missing or not executable: %s\n' "$BINARY"
        BINARY_SUPPORTS_DINGTALK="no"
    fi

    section "CONFIGURATION (SELECTED, NO SECRET VALUES)"
    file_metadata "$CONFIG"
    file_metadata "$ENV_FILE"
    if [[ -r "$CONFIG" ]]; then
        PRIMARY_CHANNEL="$(yaml_value alerts primary_channel "$CONFIG" | tr '[:upper:]' '[:lower:]')"
        [[ -n "$PRIMARY_CHANNEL" ]] || PRIMARY_CHANNEL="smtp"
        FALLBACK_CHANNEL="$(yaml_value alerts fallback_channel "$CONFIG" | tr '[:upper:]' '[:lower:]')"
        HEALTH_ENABLED="$(yaml_value health_report enabled "$CONFIG" | tr '[:upper:]' '[:lower:]')"
        HEALTH_CHANNEL="$(yaml_value health_report channel "$CONFIG" | tr '[:upper:]' '[:lower:]')"
        local configured_state token_env secret_env
        configured_state="$(yaml_value alerts state_file "$CONFIG")"
        if [[ -z "$STATE_FILE" ]]; then
            STATE_FILE="${configured_state:-/var/lib/cpa-monitor/state/alerts.json}"
        fi
        token_env="$(yaml_value dingtalk webhook_token_env "$CONFIG")"
        secret_env="$(yaml_value dingtalk signing_secret_env "$CONFIG")"

        item "alerts.primary_channel" "$PRIMARY_CHANNEL"
        item "alerts.fallback_channel" "${FALLBACK_CHANNEL:-<empty: disabled>}"
        item "alerts.send_recovery" "$(yaml_value alerts send_recovery "$CONFIG")"
        item "alerts.state_file" "$STATE_FILE"
        item "monitor.interval" "$(yaml_root_value interval "$CONFIG")"
        item "threshold.memory_percent" "$(yaml_value thresholds memory_percent "$CONFIG")"
        item "threshold.disk_percent" "$(yaml_value thresholds disk_percent "$CONFIG")"
        item "threshold.total_tcp" "$(yaml_value thresholds total_tcp_connections "$CONFIG")"
        item "threshold.service_tcp" "$(yaml_value thresholds service_port_connections "$CONFIG")"
        item "cliproxy.service_port" "$(yaml_value cliproxy service_port "$CONFIG")"
        item "health_report.enabled" "${HEALTH_ENABLED:-<default>}"
        item "health_report.channel" "${HEALTH_CHANNEL:-<empty: follows primary>}"
        item "health_report.interval" "$(yaml_value health_report interval "$CONFIG")"
        item "health_report.retry" "$(yaml_value health_report retry_interval "$CONFIG")"
        item "dingtalk.language" "$(yaml_value dingtalk language "$CONFIG")"
        item "dingtalk.timeout" "$(yaml_value dingtalk timeout "$CONFIG")"
        item "dingtalk.max_items" "$(yaml_value dingtalk max_items "$CONFIG")"
        TOKEN_INLINE_STATUS="$(yaml_secret_status dingtalk webhook_token)"
        SECRET_INLINE_STATUS="$(yaml_secret_status dingtalk signing_secret)"
        TOKEN_ENV_STATUS="$(env_assignment_status "$token_env")"
        SECRET_ENV_STATUS="$(env_assignment_status "$secret_env")"
        item "webhook_token inline" "$TOKEN_INLINE_STATUS"
        item "signing_secret inline" "$SECRET_INLINE_STATUS"
        item "webhook token env" "$TOKEN_ENV_STATUS"
        item "signing secret env" "$SECRET_ENV_STATUS"
        item "management key env" "$(env_assignment_status "$(yaml_value cliproxy management_key_env "$CONFIG")")"
    else
        printf 'Config is unreadable; run with sudo or pass --config.\n'
        [[ -n "$STATE_FILE" ]] || STATE_FILE="/var/lib/cpa-monitor/state/alerts.json"
    fi

    section "SYSTEMD SCHEDULER"
    if have_command "$SYSTEMCTL"; then
        DAEMON_ACTIVE="$(systemctl_value is-active cpa-monitor.service)"
        TIMER_ACTIVE="$(systemctl_value is-active cpa-monitor.timer)"
        show_unit cpa-monitor.service
        show_unit cpa-monitor.timer
        show_unit cpa-monitor-once.service
        show_unit cpa-monitor-check.service
        "$SYSTEMCTL" show cpa-monitor.timer -p LastTriggerUSec -p NextElapseUSecRealtime 2>/dev/null || true
    else
        printf 'systemctl is unavailable; service scheduling cannot be inspected.\n'
    fi

    if $RUN_ONCE; then
        trigger_real_cycle
        if have_command "$SYSTEMCTL"; then
            DAEMON_ACTIVE="$(systemctl_value is-active cpa-monitor.service)"
            TIMER_ACTIVE="$(systemctl_value is-active cpa-monitor.timer)"
        fi
    fi

    section "PERSISTED ALERT STATE (NO DETAILS/SECRETS)"
    inspect_state

    collect_logs
    print_analysis

    section "WHAT TO SEND BACK"
    printf 'Send only this generated report. Do not send config.yaml, cpa-monitor.env, the raw alerts.json, or a DingTalk webhook URL.\n'
    if [[ -n "$OUTPUT" ]]; then
        printf 'Report saved with mode 0600: %s\n' "$OUTPUT"
    fi
}

main "$@"
