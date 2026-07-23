#!/usr/bin/env bash

set -Eeuo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
SCRIPT="${ROOT_DIR}/scripts/validate-and-start-cpa-monitor.sh"
TMP_DIR="$(mktemp -d)"
trap 'rm -rf "$TMP_DIR"' EXIT

fail() {
    printf 'validate_and_start_test: FAIL: %s\n' "$*" >&2
    exit 1
}

assert_contains() {
    grep -Fq -- "$2" "$1" || fail "missing expected text: $2"
}

bash -n "$SCRIPT"
bash "$SCRIPT" --help >"${TMP_DIR}/help.txt"
assert_contains "${TMP_DIR}/help.txt" 'inactive (dead)'

cat >"${TMP_DIR}/systemctl" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$CALL_LOG"
case "$1" in
    start|enable|restart) exit 0 ;;
    show)
        case "$*" in
            *Result*) printf 'success\n' ;;
            *ExecMainStatus*) printf '0\n' ;;
        esac
        ;;
    is-active)
        case "${*: -1}" in
            cpa-monitor.service) exit 0 ;;
            *) exit 3 ;;
        esac
        ;;
    is-enabled) exit 1 ;;
    status) printf 'Active: active (running)\n' ;;
esac
EOF

cat >"${TMP_DIR}/journalctl" <<'EOF'
#!/usr/bin/env bash
printf 'monitor log is available\n'
EOF
chmod +x "${TMP_DIR}/systemctl" "${TMP_DIR}/journalctl"

CALL_LOG="${TMP_DIR}/calls.txt" \
CPA_MONITOR_TEST_ALLOW_NON_ROOT=1 \
CPA_MONITOR_SYSTEMCTL="${TMP_DIR}/systemctl" \
CPA_MONITOR_JOURNALCTL="${TMP_DIR}/journalctl" \
bash "$SCRIPT" >"${TMP_DIR}/report.txt"

assert_contains "${TMP_DIR}/report.txt" '配置校验成功'
assert_contains "${TMP_DIR}/report.txt" 'inactive (dead) 属于正常现象'
assert_contains "${TMP_DIR}/report.txt" '已按 daemon 模式运行'
assert_contains "${TMP_DIR}/calls.txt" 'start cpa-monitor-check.service'
assert_contains "${TMP_DIR}/calls.txt" 'restart cpa-monitor.service'

printf 'validate_and_start_test: PASS\n'
