#!/usr/bin/env bash

# Validate the installed CPA Monitor configuration, then start/restart the
# actual scheduler. cpa-monitor-check.service is intentionally a one-shot unit
# and is expected to become inactive after a successful validation.

set +x
set -Eeuo pipefail
umask 077

SYSTEMCTL="${CPA_MONITOR_SYSTEMCTL:-systemctl}"
JOURNALCTL="${CPA_MONITOR_JOURNALCTL:-journalctl}"
MODE="auto"

usage() {
    cat <<'EOF'
用法：sudo bash validate-and-start-cpa-monitor.sh [--mode auto|daemon|timer]

执行流程：
  1. 运行 cpa-monitor-check.service 校验配置；
  2. 确认 Result=success 且 ExecMainStatus=0；
  3. 启动或重启真正的 daemon/timer 调度服务；
  4. 显示服务状态和最近日志。

模式：
  auto      优先使用当前 active/enabled 的模式；都未配置时启用 daemon（默认）
  daemon    使用 cpa-monitor.service
  timer     使用 cpa-monitor.timer，并立即运行一次 one-shot 检查

注意：启动监控后会立即执行真实检查，可能发送告警并更新 alerts.json。
      check.service 成功后显示 inactive (dead) 是正常现象。
EOF
}

die() {
    printf '[cpa-monitor] 错误：%s\n' "$*" >&2
    exit 1
}

while (( $# > 0 )); do
    case "$1" in
        --mode)
            (( $# >= 2 )) || die "--mode 缺少参数"
            MODE="$2"
            shift 2
            ;;
        --mode=*)
            MODE="${1#*=}"
            shift
            ;;
        -h|--help)
            usage
            exit 0
            ;;
        *)
            die "未知参数：$1"
            ;;
    esac
done

case "$MODE" in
    auto|daemon|timer) ;;
    *) die "--mode 必须是 auto、daemon 或 timer" ;;
esac

if [[ "${CPA_MONITOR_TEST_ALLOW_NON_ROOT:-0}" != "1" ]] && (( EUID != 0 )); then
    die "请使用 sudo 运行此脚本"
fi
command -v "$SYSTEMCTL" >/dev/null 2>&1 || die "找不到 systemctl"

unit_is_active() {
    "$SYSTEMCTL" is-active --quiet "$1" 2>/dev/null
}

unit_is_enabled() {
    "$SYSTEMCTL" is-enabled --quiet "$1" 2>/dev/null
}

print_check_failure() {
    printf '\n配置校验失败，服务状态：\n' >&2
    "$SYSTEMCTL" status cpa-monitor-check.service --no-pager --full >&2 || true
    if command -v "$JOURNALCTL" >/dev/null 2>&1; then
        printf '\n最近的配置校验日志：\n' >&2
        "$JOURNALCTL" -u cpa-monitor-check.service -n 100 --no-pager >&2 || true
    fi
}

printf '1/4 正在校验 CPA Monitor 配置……\n'
if ! "$SYSTEMCTL" start cpa-monitor-check.service; then
    print_check_failure
    die "配置校验 unit 执行失败"
fi

CHECK_RESULT="$("$SYSTEMCTL" show cpa-monitor-check.service -p Result --value 2>/dev/null || true)"
CHECK_STATUS="$("$SYSTEMCTL" show cpa-monitor-check.service -p ExecMainStatus --value 2>/dev/null || true)"

if [[ "$CHECK_RESULT" != "success" || "$CHECK_STATUS" != "0" ]]; then
    print_check_failure
    die "配置未通过校验（Result=${CHECK_RESULT:-unknown}, ExecMainStatus=${CHECK_STATUS:-unknown}）"
fi

printf '配置校验成功：Result=success, ExecMainStatus=0。\n'
printf '提示：check.service 是一次性 unit，成功后显示 inactive (dead) 属于正常现象。\n'

if [[ "$MODE" == "auto" ]]; then
    if unit_is_active cpa-monitor.service; then
        MODE="daemon"
    elif unit_is_active cpa-monitor.timer; then
        MODE="timer"
    elif unit_is_enabled cpa-monitor.service; then
        MODE="daemon"
    elif unit_is_enabled cpa-monitor.timer; then
        MODE="timer"
    else
        MODE="daemon"
        printf '未发现已启用的调度模式，将使用默认 daemon 模式。\n'
    fi
fi

printf '\n2/4 选定调度模式：%s\n' "$MODE"

if [[ "$MODE" == "daemon" ]]; then
    if unit_is_active cpa-monitor.timer; then
        die "timer 当前仍在运行。请先执行 systemctl disable --now cpa-monitor.timer，避免两种模式同时运行"
    fi
    printf '3/4 正在启用并重启 cpa-monitor.service……\n'
    "$SYSTEMCTL" enable cpa-monitor.service >/dev/null
    "$SYSTEMCTL" restart cpa-monitor.service
    unit_is_active cpa-monitor.service || die "cpa-monitor.service 未进入 active 状态"

    printf '\n4/4 当前 daemon 状态：\n'
    "$SYSTEMCTL" status cpa-monitor.service --no-pager --full || true
    LOG_UNITS=(-u cpa-monitor.service)
else
    if unit_is_active cpa-monitor.service; then
        die "daemon 当前仍在运行。请先执行 systemctl disable --now cpa-monitor.service，避免状态锁冲突"
    fi
    printf '3/4 正在启用 cpa-monitor.timer，并立即运行一次检查……\n'
    "$SYSTEMCTL" enable --now cpa-monitor.timer >/dev/null
    "$SYSTEMCTL" restart cpa-monitor.timer
    "$SYSTEMCTL" start cpa-monitor-once.service
    unit_is_active cpa-monitor.timer || die "cpa-monitor.timer 未进入 active 状态"

    printf '\n4/4 当前 timer 状态：\n'
    "$SYSTEMCTL" status cpa-monitor.timer --no-pager --full || true
    "$SYSTEMCTL" list-timers cpa-monitor.timer --no-pager || true
    LOG_UNITS=(-u cpa-monitor.timer -u cpa-monitor-once.service)
fi

if command -v "$JOURNALCTL" >/dev/null 2>&1; then
    printf '\n最近的监控日志：\n'
    "$JOURNALCTL" "${LOG_UNITS[@]}" -n 100 --no-pager || true
fi

printf '\n完成：CPA Monitor 已按 %s 模式运行。\n' "$MODE"
