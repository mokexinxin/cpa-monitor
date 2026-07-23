#!/usr/bin/env bash

set -Eeuo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
SCRIPT="${ROOT_DIR}/scripts/diagnose-dingtalk.sh"
TMP_DIR="$(mktemp -d)"
trap 'rm -rf "$TMP_DIR"' EXIT

fail() {
    printf 'diagnose_dingtalk_test: FAIL: %s\n' "$*" >&2
    exit 1
}

assert_contains() {
    local file="$1"
    local expected="$2"
    grep -Fq -- "$expected" "$file" || fail "missing expected text: $expected"
}

assert_not_contains() {
    local file="$1"
    local unexpected="$2"
    if grep -Fq -- "$unexpected" "$file"; then
        fail "found secret/unexpected text: $unexpected"
    fi
}

bash -n "$SCRIPT"
bash "$SCRIPT" --help >"${TMP_DIR}/help.txt"
assert_contains "${TMP_DIR}/help.txt" '--run-once'
assert_contains "${TMP_DIR}/help.txt" 'does not send a'

mkdir -p "${TMP_DIR}/bin"

cat >"${TMP_DIR}/config.yaml" <<'EOF'
cliproxy:
  management_key_env: CPA_MANAGEMENT_KEY
alerts:
  state_file: STATE_PATH_PLACEHOLDER
  primary_channel: dingtalk
  fallback_channel: ""
  send_recovery: true
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
EOF
sed "s#STATE_PATH_PLACEHOLDER#${TMP_DIR}/alerts.json#" "${TMP_DIR}/config.yaml" >"${TMP_DIR}/config.rendered.yaml"
mv "${TMP_DIR}/config.rendered.yaml" "${TMP_DIR}/config.yaml"

cat >"${TMP_DIR}/monitor.env" <<'EOF'
CPA_MANAGEMENT_KEY="do-not-print-management-key"
CPA_DINGTALK_WEBHOOK_TOKEN="do-not-print-token"
CPA_DINGTALK_SIGNING_SECRET="do-not-print-secret"
EOF

cat >"${TMP_DIR}/alerts.json" <<'EOF'
{
  "version": 2,
  "active": [
    {
      "key": "memory:usage",
      "scope": "memory",
      "summary": "secret detail must not be printed",
      "current": "91%",
      "threshold": "90%",
      "activated_at": "2026-07-13T01:00:00Z"
    }
  ],
  "health_report": {
    "last_attempt_at": "2026-07-13T01:00:00Z",
    "last_sent_at": "2026-07-13T01:00:00Z"
  }
}
EOF

cat >"${TMP_DIR}/bin/cpa-monitor" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' 'Usage: cpa-monitor --test-notification primary|dingtalk|smtp'
EOF

cat >"${TMP_DIR}/bin/systemctl" <<'EOF'
#!/usr/bin/env bash
if [[ -n "${SYSTEMCTL_CALL_LOG:-}" ]]; then
    printf '%s\n' "$*" >>"$SYSTEMCTL_CALL_LOG"
fi
case "$1:$2" in
    is-active:cpa-monitor.service) printf 'active\n' ;;
    is-active:cpa-monitor.timer) printf 'inactive\n'; exit 3 ;;
    is-active:*) printf 'inactive\n'; exit 3 ;;
    is-enabled:cpa-monitor.service) printf 'enabled\n' ;;
    is-enabled:cpa-monitor.timer) printf 'disabled\n'; exit 1 ;;
    is-enabled:*) printf 'static\n' ;;
    show:*)
        printf 'LoadState=loaded\nActiveState=active\nSubState=running\nResult=success\nExecMainStatus=0\n'
        ;;
    restart:cpa-monitor.service) printf 'restart accepted\n' ;;
    start:cpa-monitor-once.service) printf 'start accepted\n' ;;
esac
EOF

cat >"${TMP_DIR}/bin/journalctl" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' \
  '2026-07-13T01:01:00+0000 host cpa-monitor[1]: level=WARN msg="monitor condition detected" check=memory scope=memory' \
  '2026-07-13T01:02:00+0000 host cpa-monitor[1]: dingtalk access_token=should-never-appear'
EOF

chmod +x "${TMP_DIR}/bin/cpa-monitor" "${TMP_DIR}/bin/systemctl" "${TMP_DIR}/bin/journalctl"

REPORT="${TMP_DIR}/report.txt"
CPA_MONITOR_DIAG_BINARY="${TMP_DIR}/bin/cpa-monitor" \
CPA_MONITOR_DIAG_SYSTEMCTL="${TMP_DIR}/bin/systemctl" \
CPA_MONITOR_DIAG_JOURNALCTL="${TMP_DIR}/bin/journalctl" \
bash "$SCRIPT" \
    --config "${TMP_DIR}/config.yaml" \
    --env-file "${TMP_DIR}/monitor.env" \
    --state "${TMP_DIR}/alerts.json" \
    >"$REPORT"

assert_contains "$REPORT" 'alerts.primary_channel:      dingtalk'
assert_contains "$REPORT" 'CPA_DINGTALK_WEBHOOK_TOKEN: assignment present (value not displayed)'
assert_contains "$REPORT" 'active_count=1'
assert_contains "$REPORT" 'active_by_scope=memory:1'
assert_contains "$REPORT" 'switching SMTP to DingTalk does not replay conditions already active'
assert_contains "$REPORT" 'access_token=[REDACTED]'
assert_not_contains "$REPORT" 'do-not-print-management-key'
assert_not_contains "$REPORT" 'do-not-print-token'
assert_not_contains "$REPORT" 'do-not-print-secret'
assert_not_contains "$REPORT" 'secret detail must not be printed'
assert_not_contains "$REPORT" 'should-never-appear'

ACTIVE_REPORT="${TMP_DIR}/active-report.txt"
SYSTEMCTL_CALL_LOG="${TMP_DIR}/systemctl-calls.txt" \
CPA_MONITOR_DIAG_BINARY="${TMP_DIR}/bin/cpa-monitor" \
CPA_MONITOR_DIAG_SYSTEMCTL="${TMP_DIR}/bin/systemctl" \
CPA_MONITOR_DIAG_JOURNALCTL="${TMP_DIR}/bin/journalctl" \
bash "$SCRIPT" \
    --config "${TMP_DIR}/config.yaml" \
    --env-file "${TMP_DIR}/monitor.env" \
    --state "${TMP_DIR}/alerts.json" \
    --run-once \
    >"$ACTIVE_REPORT"
assert_contains "$ACTIVE_REPORT" 'restarting it to trigger its immediate first cycle'
assert_contains "${TMP_DIR}/systemctl-calls.txt" 'restart cpa-monitor.service'

printf 'diagnose_dingtalk_test: PASS\n'
