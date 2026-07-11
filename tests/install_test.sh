#!/usr/bin/env bash

# Integration tests for install.sh. Every installation is staged below a
# temporary --root and systemctl is replaced with a recording stub, so this
# script is safe to run on developer machines and in unprivileged CI jobs.

set -Eeuo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd -P)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd -P)"
INSTALLER="${REPO_ROOT}/install.sh"
BASH_BIN="${BASH:-bash}"
TEST_PATH="${PATH:-/usr/bin:/bin:/usr/sbin:/sbin}"

TEST_TMP="$(mktemp -d "${TMPDIR:-/tmp}/cpa-monitor-install-test.XXXXXX")"
TEST_RUNTIME_TMP="${TEST_TMP}/tmp"
TEST_HOME="${TEST_TMP}/home"
mkdir -p "$TEST_RUNTIME_TMP" "$TEST_HOME"
trap 'rm -rf "$TEST_TMP"' EXIT

TESTS_RUN=0
shopt -s nullglob

fail() {
    printf 'FAIL: %s\n' "$*" >&2
    exit 1
}

run_test() {
    local name="$1"
    local function_name="$2"
    printf 'test: %s ... ' "$name"
    "$function_name"
    TESTS_RUN=$((TESTS_RUN + 1))
    printf 'ok\n'
}

clean_env() {
    env -i \
        PATH="$TEST_PATH" \
        HOME="$TEST_HOME" \
        TMPDIR="$TEST_RUNTIME_TMP" \
        LANG=C \
        LC_ALL=C \
        "$@"
}

new_case_dir() {
    mktemp -d "${TEST_TMP}/case.XXXXXX"
}

file_mode() {
    local path="$1"
    if [[ "$(uname -s)" == "Darwin" ]]; then
        stat -f '%Lp' "$path"
    else
        stat -c '%a' "$path"
    fi
}

assert_eq() {
    local expected="$1"
    local actual="$2"
    local message="${3:-values differ}"
    [[ "$actual" == "$expected" ]] || fail "${message}: expected '${expected}', got '${actual}'"
}

assert_nonzero() {
    local status="$1"
    local message="${2:-command should fail}"
    (( status != 0 )) || fail "$message"
}

assert_file() {
    [[ -f "$1" ]] || fail "expected regular file: $1"
}

assert_dir() {
    [[ -d "$1" ]] || fail "expected directory: $1"
}

assert_not_exists() {
    [[ ! -e "$1" && ! -L "$1" ]] || fail "path should not exist: $1"
}

assert_mode() {
    local expected="$1"
    local path="$2"
    assert_eq "$expected" "$(file_mode "$path")" "unexpected mode for $path"
}

assert_contains() {
    local path="$1"
    local text="$2"
    grep -F -- "$text" "$path" >/dev/null 2>&1 || fail "expected '$text' in $path"
}

assert_not_contains() {
    local path="$1"
    local text="$2"
    if grep -F -- "$text" "$path" >/dev/null 2>&1; then
        fail "did not expect '$text' in $path"
    fi
}

assert_line() {
    local path="$1"
    local text="$2"
    grep -F -x -- "$text" "$path" >/dev/null 2>&1 || fail "expected line '$text' in $path"
}

assert_no_line() {
    local path="$1"
    local text="$2"
    if grep -F -x -- "$text" "$path" >/dev/null 2>&1; then
        fail "did not expect line '$text' in $path"
    fi
}

line_number() {
    local path="$1"
    local wanted="$2"
    local line
    local number=0
    while IFS= read -r line; do
        number=$((number + 1))
        if [[ "$line" == "$wanted" ]]; then
            printf '%s' "$number"
            return 0
        fi
    done <"$path"
    return 1
}

decode_systemd_assignment() {
    local line="$1"
    local encoded="${line#*=}"
    local output=""
    local character
    local escaped
    local last
    (( ${#encoded} >= 2 )) || return 1
    last="${encoded:$((${#encoded} - 1)):1}"
    [[ "${encoded:0:1}" == '"' && "$last" == '"' ]] || return 1
    encoded="${encoded:1:$((${#encoded} - 2))}"
    while (( ${#encoded} > 0 )); do
        character="${encoded:0:1}"
        encoded="${encoded:1}"
        if [[ "$character" == '\' ]]; then
            (( ${#encoded} > 0 )) || return 1
            escaped="${encoded:0:1}"
            encoded="${encoded:1}"
            case "$escaped" in
                '\'|'"') output="${output}${escaped}" ;;
                *) output="${output}\\${escaped}" ;;
            esac
        else
            output="${output}${character}"
        fi
    done
    printf '%s' "$output"
}

assert_line_before() {
    local path="$1"
    local first="$2"
    local second="$3"
    local first_number
    local second_number
    first_number="$(line_number "$path" "$first")" || fail "missing line '$first' in $path"
    second_number="$(line_number "$path" "$second")" || fail "missing line '$second' in $path"
    (( first_number < second_number )) || fail "expected '$first' before '$second' in $path"
}

assert_same_file() {
    local expected="$1"
    local actual="$2"
    cmp -s "$expected" "$actual" || fail "files differ: $expected and $actual"
}

make_fake_binary() {
    local path="$1"
    local version="$2"
    cat >"$path" <<EOF
#!/usr/bin/env bash
# fake cpa-monitor fixture: ${version}
for argument in "\$@"; do
    case "\$argument" in
        --help|--check-config) exit 0 ;;
    esac
done
exit 64
EOF
    chmod 0755 "$path"
}

make_systemctl_stub() {
    local path="$1"
    cat >"$path" <<'EOF'
#!/usr/bin/env bash
set -u

printf '%s\n' "$*" >>"${SYSTEMCTL_LOG:?SYSTEMCTL_LOG is required}"

if [[ -n "${SYSTEMCTL_SIGNAL_MATCH:-}" && "$*" == "$SYSTEMCTL_SIGNAL_MATCH" ]]; then
    kill -TERM "$PPID"
    exit 0
fi

if [[ -n "${SYSTEMCTL_FAIL_MATCH:-}" && "$*" == "$SYSTEMCTL_FAIL_MATCH" ]]; then
    exit "${SYSTEMCTL_FAIL_STATUS:-42}"
fi

case "${1:-}" in
    is-enabled) exit "${SYSTEMCTL_IS_ENABLED_STATUS:-1}" ;;
    is-active) exit 0 ;;
esac

exit 0
EOF
    chmod 0755 "$path"
}

make_config() {
    local path="$1"
    local marker="$2"
    cat >"$path" <<EOF
# fixture-version: ${marker}
interval: 60s

cliproxy:
  base_url: http://127.0.0.1:8317
  management_key: ""
  management_key_env: CPA_MANAGEMENT_KEY
  service_port: 0
  timeout: 10s

thresholds:
  memory_percent: 80
  disk_percent: 80
  total_tcp_connections: 3000
  service_port_connections: 800

alerts:
  send_recovery: false
  state_file: /var/lib/cpa-monitor/state/alerts.json

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
  timeout: 10s

logging:
  level: info
  file:
    enabled: true
    path: /var/log/cpa-monitor/monitor.log
    max_size_mb: 20
    max_files: 5
    max_total_size_mb: 80
EOF
}

make_env_file() {
    local path="$1"
    local marker="$2"
    cat >"$path" <<EOF
# fixture-version: ${marker}
CPA_MANAGEMENT_KEY="management-${marker}"
EOF
    chmod 0600 "$path"
}

run_installer_with_sources() {
    local root="$1"
    local binary="$2"
    local config="$3"
    local environment="$4"
    shift 4
    clean_env "$BASH_BIN" "$INSTALLER" \
        --root "$root" \
        --binary "$binary" \
        --config "$config" \
        --env-file "$environment" \
        --non-interactive \
        "$@"
}

assert_backup_set() {
    local directory="$1"
    local prefix="$2"
    local expected_mode="$3"
    shift 3
    local -a expected_files=("$@")
    local -a backups=("$directory"/"$prefix".*)
    local expected
    local backup
    local matches

    assert_eq "${#expected_files[@]}" "${#backups[@]}" "unexpected backup count for $prefix"
    for backup in "${backups[@]}"; do
        assert_file "$backup"
        assert_mode "$expected_mode" "$backup"
    done
    for expected in "${expected_files[@]}"; do
        matches=0
        for backup in "${backups[@]}"; do
            if cmp -s "$expected" "$backup"; then
                matches=$((matches + 1))
            fi
        done
        assert_eq 1 "$matches" "expected exactly one backup matching $expected"
    done
}

test_syntax_and_help() {
    local case_dir
    local help_output
    local error_output
    local status
    case_dir="$(new_case_dir)"
    help_output="${case_dir}/help.out"
    error_output="${case_dir}/error.out"

    "$BASH_BIN" -n "$INSTALLER" || fail "install.sh failed bash syntax validation"
    clean_env "$BASH_BIN" "$INSTALLER" --help >"$help_output" 2>&1 || fail "--help failed"
    assert_contains "$help_output" 'Usage: sudo ./install.sh [options]'
    assert_contains "$help_output" '--root DIR'
    assert_contains "$help_output" '--mode daemon|timer'

    set +e
    clean_env "$BASH_BIN" "$INSTALLER" --root >"$error_output" 2>&1
    status=$?
    set -e
    assert_nonzero "$status" "missing option argument should fail"
    assert_contains "$error_output" '--root requires an argument'
}

test_staged_first_install_and_daemon_activation() {
    local case_dir
    local root
    local binary
    local systemctl_stub
    local systemctl_log
    local output
    local service
    local oneshot
    local check
    local timer
    case_dir="$(new_case_dir)"
    root="${case_dir}/root"
    binary="${case_dir}/cpa-monitor"
    systemctl_stub="${case_dir}/systemctl"
    systemctl_log="${case_dir}/systemctl.log"
    output="${case_dir}/install.out"
    make_fake_binary "$binary" first
    make_systemctl_stub "$systemctl_stub"
    : >"$systemctl_log"

    clean_env \
        CPA_MONITOR_TEST_SYSTEMCTL="$systemctl_stub" \
        SYSTEMCTL_LOG="$systemctl_log" \
        CPA_MONITOR_MANAGEMENT_KEY='first-management-key' \
        CPA_MONITOR_BASE_URL='http://127.0.0.1:8317' \
        CPA_MONITOR_INTERVAL='45s' \
        CPA_MONITOR_SMTP_HOST='smtp.example.com' \
        CPA_MONITOR_SMTP_PORT='587' \
        CPA_MONITOR_SMTP_FROM='monitor@example.com' \
        CPA_MONITOR_SMTP_TO='ops@example.com, admin@example.com' \
        CPA_MONITOR_SMTP_MODE='starttls' \
        CPA_MONITOR_SMTP_USERNAME='smtp-user' \
        CPA_MONITOR_SMTP_PASSWORD='smtp-password' \
        "$BASH_BIN" "$INSTALLER" \
        --root "$root" \
        --binary "$binary" \
        --mode daemon \
        --timer-interval 3min \
        --non-interactive >"$output" 2>&1 || fail "staged daemon installation failed"

    service="${root}/etc/systemd/system/cpa-monitor.service"
    oneshot="${root}/etc/systemd/system/cpa-monitor-once.service"
    check="${root}/etc/systemd/system/cpa-monitor-check.service"
    timer="${root}/etc/systemd/system/cpa-monitor.timer"

    assert_file "${root}/usr/local/bin/cpa-monitor"
    assert_file "${root}/etc/cpa-monitor/config.yaml"
    assert_file "${root}/etc/cpa-monitor/cpa-monitor.env"
    assert_file "$service"
    assert_file "$oneshot"
    assert_file "$check"
    assert_file "$timer"
    assert_dir "${root}/var/lib/cpa-monitor/state"
    assert_dir "${root}/var/log/cpa-monitor"

    assert_mode 755 "${root}/usr/local/bin/cpa-monitor"
    assert_mode 750 "${root}/etc/cpa-monitor"
    assert_mode 640 "${root}/etc/cpa-monitor/config.yaml"
    assert_mode 600 "${root}/etc/cpa-monitor/cpa-monitor.env"
    assert_mode 750 "${root}/var/lib/cpa-monitor"
    assert_mode 750 "${root}/var/lib/cpa-monitor/state"
    assert_mode 750 "${root}/var/log/cpa-monitor"
    assert_mode 644 "$service"
    assert_mode 644 "$oneshot"
    assert_mode 644 "$check"
    assert_mode 644 "$timer"

    assert_contains "${root}/etc/cpa-monitor/config.yaml" 'state_file: /var/lib/cpa-monitor/state/alerts.json'
    assert_contains "${root}/etc/cpa-monitor/config.yaml" 'health_report:'
    assert_contains "${root}/etc/cpa-monitor/config.yaml" '  enabled: true'
    assert_contains "${root}/etc/cpa-monitor/config.yaml" "  interval: '24h'"
    assert_contains "${root}/etc/cpa-monitor/config.yaml" "  retry_interval: '15m'"
    assert_contains "${root}/etc/cpa-monitor/config.yaml" 'path: /var/log/cpa-monitor/monitor.log'
    assert_contains "${root}/etc/cpa-monitor/config.yaml" "- 'ops@example.com'"
    assert_not_contains "${root}/etc/cpa-monitor/config.yaml" 'first-management-key'
    assert_contains "${root}/etc/cpa-monitor/cpa-monitor.env" 'CPA_MANAGEMENT_KEY="first-management-key"'
    assert_contains "${root}/etc/cpa-monitor/cpa-monitor.env" 'CPA_SMTP_USERNAME="smtp-user"'

    assert_contains "$service" 'WorkingDirectory=/var/lib/cpa-monitor'
    assert_contains "$service" 'EnvironmentFile=/etc/cpa-monitor/cpa-monitor.env'
    assert_contains "$service" 'ExecStartPre=/usr/local/bin/cpa-monitor --config /etc/cpa-monitor/config.yaml --check-config'
    assert_contains "$service" 'ExecStart=/usr/bin/flock -n -E 75 /var/lib/cpa-monitor/.cpa-monitor.lock /usr/local/bin/cpa-monitor --config /etc/cpa-monitor/config.yaml'
    assert_contains "$service" 'NoNewPrivileges=true'
    assert_not_contains "$service" 'PrivateNetwork=true'
    assert_not_contains "$service" 'ProcSubset='
    assert_not_contains "$service" 'ProtectSystem='
    assert_not_contains "$service" 'ProtectHome='
    assert_contains "$oneshot" 'ExecStartPre=/usr/local/bin/cpa-monitor --config /etc/cpa-monitor/config.yaml --check-config'
    assert_contains "$oneshot" 'ExecStart=/usr/bin/flock -n -E 75 /var/lib/cpa-monitor/.cpa-monitor.lock /usr/local/bin/cpa-monitor --config /etc/cpa-monitor/config.yaml --once'
    assert_contains "$check" 'EnvironmentFile=/etc/cpa-monitor/cpa-monitor.env'
    assert_contains "$check" 'ExecStart=/usr/local/bin/cpa-monitor --config /etc/cpa-monitor/config.yaml --check-config'
    assert_not_contains "$check" 'network-online.target'
    assert_contains "$timer" 'OnUnitInactiveSec=3min'
    assert_contains "$timer" 'Unit=cpa-monitor-once.service'
    assert_not_contains "$timer" 'Persistent=true'
    assert_not_contains "$service" "$root"
    assert_not_contains "$oneshot" "$root"
    assert_not_contains "$check" "$root"
    assert_not_contains "$timer" "$root"

    assert_line "$systemctl_log" 'daemon-reload'
    assert_line "$systemctl_log" 'start cpa-monitor-check.service'
    assert_line "$systemctl_log" 'disable --now cpa-monitor.timer'
    assert_line "$systemctl_log" 'stop cpa-monitor-once.service'
    assert_line "$systemctl_log" 'enable cpa-monitor.service'
    assert_line "$systemctl_log" 'restart cpa-monitor.service'
    assert_line "$systemctl_log" 'is-active --quiet cpa-monitor.service'
    assert_line_before "$systemctl_log" 'start cpa-monitor-check.service' 'restart cpa-monitor.service'
    assert_no_line "$systemctl_log" 'enable cpa-monitor.timer'
}

test_timer_activation() {
    local case_dir
    local root
    local binary
    local config
    local environment
    local systemctl_stub
    local systemctl_log
    case_dir="$(new_case_dir)"
    root="${case_dir}/root"
    binary="${case_dir}/cpa-monitor"
    config="${case_dir}/config.yaml"
    environment="${case_dir}/cpa-monitor.env"
    systemctl_stub="${case_dir}/systemctl"
    systemctl_log="${case_dir}/systemctl.log"
    make_fake_binary "$binary" timer
    make_config "$config" timer
    make_env_file "$environment" timer
    make_systemctl_stub "$systemctl_stub"
    : >"$systemctl_log"

    clean_env CPA_MONITOR_TEST_SYSTEMCTL="$systemctl_stub" SYSTEMCTL_LOG="$systemctl_log" \
        "$BASH_BIN" "$INSTALLER" \
        --root "$root" \
        --binary "$binary" \
        --config "$config" \
        --env-file "$environment" \
        --mode timer \
        --timer-interval 7min \
        --non-interactive >/dev/null 2>&1 || fail "staged timer installation failed"

    assert_contains "${root}/etc/systemd/system/cpa-monitor.timer" 'OnUnitInactiveSec=7min'
    assert_line "$systemctl_log" 'daemon-reload'
    assert_line "$systemctl_log" 'start cpa-monitor-check.service'
    assert_line "$systemctl_log" 'disable --now cpa-monitor.service'
    assert_line "$systemctl_log" 'stop cpa-monitor-once.service'
    assert_line "$systemctl_log" 'enable cpa-monitor.timer'
    assert_line "$systemctl_log" 'restart cpa-monitor.timer'
    assert_line "$systemctl_log" 'is-active --quiet cpa-monitor.timer'
    assert_line_before "$systemctl_log" 'start cpa-monitor-check.service' 'restart cpa-monitor.timer'
    assert_no_line "$systemctl_log" 'enable cpa-monitor.service'
}

test_generated_quoting_and_weird_root() {
    local case_dir
    local root
    local binary
    local secret
    local environment
    local encoded
    local decoded
    local trace_output
    case_dir="$(new_case_dir)"
    root="${case_dir}/root with spaces [*]"
    binary="${case_dir}/binary with spaces"
    trace_output="${case_dir}/installer.trace"
    secret='space $HOME `tick` "quote" \ slash # ; = and O'\''Brien'
    make_fake_binary "$binary" quoting

    clean_env \
        CPA_MONITOR_MANAGEMENT_KEY="$secret" \
        CPA_MONITOR_SMTP_HOST='smtp.example.com' \
        CPA_MONITOR_SMTP_FROM="O'Brien <monitor@example.com>" \
        CPA_MONITOR_SMTP_TO="D'Angelo <ops@example.com>" \
        "$BASH_BIN" -x "$INSTALLER" \
        --root "$root" \
        --binary "$binary" \
        --non-interactive \
        --no-start >"$trace_output" 2>&1 || fail "generated quoting installation failed"

    environment="${root}/etc/cpa-monitor/cpa-monitor.env"
    encoded="$(grep '^CPA_MANAGEMENT_KEY=' "$environment")"
    decoded="$(decode_systemd_assignment "$encoded")" || fail "could not decode generated EnvironmentFile value"
    assert_eq "$secret" "$decoded" "EnvironmentFile value did not round-trip"
    assert_no_line "$environment" 'CPA_SMTP_USERNAME=""'
    assert_no_line "$environment" 'CPA_SMTP_PASSWORD=""'
    assert_contains "${root}/etc/cpa-monitor/config.yaml" "from: 'O''Brien <monitor@example.com>'"
    assert_contains "${root}/etc/cpa-monitor/config.yaml" "- 'D''Angelo <ops@example.com>'"
    assert_not_contains "${root}/etc/cpa-monitor/config.yaml" "$secret"
    assert_not_contains "${root}/etc/systemd/system/cpa-monitor.service" "$root"
    assert_not_contains "$trace_output" "$secret"
}

test_no_start_skips_config_check() {
    local case_dir
    local root
    local binary
    local config
    local environment
    local systemctl_stub
    local systemctl_log
    case_dir="$(new_case_dir)"
    root="${case_dir}/root"
    binary="${case_dir}/cpa-monitor"
    config="${case_dir}/config.yaml"
    environment="${case_dir}/cpa-monitor.env"
    systemctl_stub="${case_dir}/systemctl"
    systemctl_log="${case_dir}/systemctl.log"
    make_fake_binary "$binary" no-start
    make_config "$config" no-start
    make_env_file "$environment" no-start
    make_systemctl_stub "$systemctl_stub"
    : >"$systemctl_log"

    clean_env CPA_MONITOR_TEST_SYSTEMCTL="$systemctl_stub" SYSTEMCTL_LOG="$systemctl_log" \
        "$BASH_BIN" "$INSTALLER" \
        --root "$root" \
        --binary "$binary" \
        --config "$config" \
        --env-file "$environment" \
        --mode daemon \
        --non-interactive \
        --no-start >/dev/null 2>&1 || fail "--no-start installation failed"

    assert_file "${root}/etc/systemd/system/cpa-monitor-check.service"
    assert_line "$systemctl_log" 'daemon-reload'
    assert_no_line "$systemctl_log" 'start cpa-monitor-check.service'
    assert_no_line "$systemctl_log" 'enable cpa-monitor.service'
    assert_no_line "$systemctl_log" 'restart cpa-monitor.service'
}

test_preserve_and_unique_backups() {
    local case_dir
    local root
    local binary
    local config_one
    local config_two
    local config_three
    local env_one
    local env_two
    local env_three
    local installed_config
    local installed_env
    local preserved_config
    local preserved_env
    local backup_dir
    case_dir="$(new_case_dir)"
    root="${case_dir}/root"
    binary="${case_dir}/cpa-monitor"
    config_one="${case_dir}/config-one.yaml"
    config_two="${case_dir}/config-two.yaml"
    config_three="${case_dir}/config-three.yaml"
    env_one="${case_dir}/env-one"
    env_two="${case_dir}/env-two"
    env_three="${case_dir}/env-three"
    preserved_config="${case_dir}/preserved-config.yaml"
    preserved_env="${case_dir}/preserved-env"
    make_fake_binary "$binary" backups
    make_config "$config_one" one
    make_config "$config_two" two
    make_config "$config_three" three
    make_env_file "$env_one" one
    make_env_file "$env_two" two
    make_env_file "$env_three" three

    run_installer_with_sources "$root" "$binary" "$config_one" "$env_one" --no-start >/dev/null 2>&1 || fail "initial source installation failed"
    installed_config="${root}/etc/cpa-monitor/config.yaml"
    installed_env="${root}/etc/cpa-monitor/cpa-monitor.env"
    cp "$installed_config" "$preserved_config"
    cp "$installed_env" "$preserved_env"

    clean_env "$BASH_BIN" "$INSTALLER" \
        --root "$root" \
        --binary "$binary" \
        --non-interactive \
        --no-start >/dev/null 2>&1 || fail "preserving upgrade failed"
    assert_same_file "$preserved_config" "$installed_config"
    assert_same_file "$preserved_env" "$installed_env"
    assert_not_exists "${root}/etc/cpa-monitor/backups"

    run_installer_with_sources "$root" "$binary" "$config_two" "$env_two" --no-start >/dev/null 2>&1 || fail "first explicit replacement failed"
    assert_same_file "$config_two" "$installed_config"
    assert_same_file "$env_two" "$installed_env"

    run_installer_with_sources "$root" "$binary" "$config_three" "$env_three" --no-start >/dev/null 2>&1 || fail "second explicit replacement failed"
    assert_same_file "$config_three" "$installed_config"
    assert_same_file "$env_three" "$installed_env"

    backup_dir="${root}/etc/cpa-monitor/backups"
    assert_dir "$backup_dir"
    assert_mode 700 "$backup_dir"
    assert_backup_set "$backup_dir" config.yaml 640 "$config_one" "$config_two"
    assert_backup_set "$backup_dir" cpa-monitor.env 600 "$env_one" "$env_two"
}

test_symlink_target_rejected() {
    local case_dir
    local root
    local binary
    local config
    local environment
    local sentinel
    local target
    local output
    local status
    case_dir="$(new_case_dir)"
    root="${case_dir}/root"
    binary="${case_dir}/cpa-monitor"
    config="${case_dir}/config.yaml"
    environment="${case_dir}/cpa-monitor.env"
    sentinel="${case_dir}/sentinel"
    output="${case_dir}/install.out"
    make_fake_binary "$binary" symlink
    make_config "$config" symlink
    make_env_file "$environment" symlink
    printf '%s\n' untouched >"$sentinel"
    mkdir -p "${root}/etc/cpa-monitor"
    target="${root}/etc/cpa-monitor/config.yaml"
    ln -s "$sentinel" "$target"

    set +e
    run_installer_with_sources "$root" "$binary" "$config" "$environment" --no-start >"$output" 2>&1
    status=$?
    set -e

    assert_nonzero "$status" "symlink target should be rejected"
    [[ -L "$target" ]] || fail "installer replaced the config symlink"
    assert_eq "$sentinel" "$(readlink "$target")" "config symlink target changed"
    assert_contains "$sentinel" untouched
    assert_contains "$output" 'refusing managed symlink'
    assert_not_exists "${root}/usr/local/bin/cpa-monitor"
    assert_not_exists "${root}/etc/systemd/system/cpa-monitor.service"
}

test_invalid_and_missing_input_leave_no_files() {
    local case_dir
    local binary
    local missing_root
    local invalid_root
    local missing_output
    local invalid_output
    local status
    case_dir="$(new_case_dir)"
    binary="${case_dir}/cpa-monitor"
    missing_root="${case_dir}/missing-root"
    invalid_root="${case_dir}/invalid-root"
    missing_output="${case_dir}/missing.out"
    invalid_output="${case_dir}/invalid.out"
    make_fake_binary "$binary" validation

    set +e
    clean_env "$BASH_BIN" "$INSTALLER" \
        --root "$missing_root" \
        --binary "$binary" \
        --non-interactive \
        --no-start >"$missing_output" 2>&1
    status=$?
    set -e
    assert_nonzero "$status" "missing first-install settings should fail"
    assert_contains "$missing_output" 'CPA_MONITOR_SMTP_HOST is required'
    assert_not_exists "$missing_root"

    set +e
    clean_env \
        CPA_MONITOR_MANAGEMENT_KEY='valid-key' \
        CPA_MONITOR_BASE_URL='http://monitor.example.com:8317' \
        CPA_MONITOR_SMTP_HOST='smtp.example.com' \
        CPA_MONITOR_SMTP_FROM='monitor@example.com' \
        CPA_MONITOR_SMTP_TO='ops@example.com' \
        "$BASH_BIN" "$INSTALLER" \
        --root "$invalid_root" \
        --binary "$binary" \
        --non-interactive \
        --no-start >"$invalid_output" 2>&1
    status=$?
    set -e
    assert_nonzero "$status" "remote plaintext HTTP should fail"
    assert_contains "$invalid_output" 'must use HTTPS for a non-loopback host'
    assert_not_exists "$invalid_root"
}

test_systemctl_failure_rolls_back_files() {
    local case_dir
    local root
    local binary_one
    local binary_two
    local config_one
    local config_two
    local env_one
    local env_two
    local systemctl_stub
    local systemctl_log
    local snapshot
    local output
    local status
    local relative
    local old_config_dir_mode
    local old_state_dir_mode
    local -a managed_files=(
        usr/local/bin/cpa-monitor
        etc/cpa-monitor/config.yaml
        etc/cpa-monitor/cpa-monitor.env
        etc/systemd/system/cpa-monitor.service
        etc/systemd/system/cpa-monitor-once.service
        etc/systemd/system/cpa-monitor-check.service
        etc/systemd/system/cpa-monitor.timer
    )
    case_dir="$(new_case_dir)"
    root="${case_dir}/root"
    binary_one="${case_dir}/cpa-monitor-one"
    binary_two="${case_dir}/cpa-monitor-two"
    config_one="${case_dir}/config-one.yaml"
    config_two="${case_dir}/config-two.yaml"
    env_one="${case_dir}/env-one"
    env_two="${case_dir}/env-two"
    systemctl_stub="${case_dir}/systemctl"
    systemctl_log="${case_dir}/systemctl.log"
    snapshot="${case_dir}/snapshot"
    output="${case_dir}/failed-install.out"
    make_fake_binary "$binary_one" rollback-one
    make_fake_binary "$binary_two" rollback-two
    make_config "$config_one" rollback-one
    make_config "$config_two" rollback-two
    make_env_file "$env_one" rollback-one
    make_env_file "$env_two" rollback-two
    make_systemctl_stub "$systemctl_stub"
    : >"$systemctl_log"

    clean_env CPA_MONITOR_TEST_SYSTEMCTL="$systemctl_stub" SYSTEMCTL_LOG="$systemctl_log" \
        "$BASH_BIN" "$INSTALLER" \
        --root "$root" \
        --binary "$binary_one" \
        --config "$config_one" \
        --env-file "$env_one" \
        --mode daemon \
        --timer-interval 1min \
        --non-interactive >/dev/null 2>&1 || fail "rollback baseline installation failed"

    chmod 0711 "${root}/etc/cpa-monitor"
    chmod 0701 "${root}/var/lib/cpa-monitor"
    old_config_dir_mode="$(file_mode "${root}/etc/cpa-monitor")"
    old_state_dir_mode="$(file_mode "${root}/var/lib/cpa-monitor")"

    for relative in "${managed_files[@]}"; do
        mkdir -p "${snapshot}/$(dirname "$relative")"
        cp -p "${root}/${relative}" "${snapshot}/${relative}"
    done
    : >"$systemctl_log"

    set +e
    clean_env \
        CPA_MONITOR_TEST_SYSTEMCTL="$systemctl_stub" \
        SYSTEMCTL_LOG="$systemctl_log" \
        SYSTEMCTL_FAIL_MATCH='restart cpa-monitor.service' \
        SYSTEMCTL_FAIL_STATUS=47 \
        FLOCK_BIN=/opt/cpa-monitor-test-flock \
        "$BASH_BIN" "$INSTALLER" \
        --root "$root" \
        --binary "$binary_two" \
        --config "$config_two" \
        --env-file "$env_two" \
        --mode daemon \
        --timer-interval 9min \
        --non-interactive >"$output" 2>&1
    status=$?
    set -e

    assert_eq 47 "$status" "systemctl failure status was not propagated"
    assert_contains "$output" 'restoring previous files and service state'
    assert_line "$systemctl_log" 'restart cpa-monitor.service'
    assert_line "$systemctl_log" 'disable --now cpa-monitor.service'
    assert_line "$systemctl_log" 'disable --now cpa-monitor.timer'

    for relative in "${managed_files[@]}"; do
        assert_same_file "${snapshot}/${relative}" "${root}/${relative}"
        assert_eq "$(file_mode "${snapshot}/${relative}")" "$(file_mode "${root}/${relative}")" "rollback mode mismatch for $relative"
    done
    assert_not_contains "${root}/etc/systemd/system/cpa-monitor.service" '/opt/cpa-monitor-test-flock'
    assert_contains "${root}/etc/systemd/system/cpa-monitor.timer" 'OnUnitInactiveSec=1min'
    assert_eq "$old_config_dir_mode" "$(file_mode "${root}/etc/cpa-monitor")" "config directory mode was not rolled back"
    assert_eq "$old_state_dir_mode" "$(file_mode "${root}/var/lib/cpa-monitor")" "state directory mode was not rolled back"
}

test_signal_rolls_back_files() {
    local case_dir
    local root
    local binary_one
    local binary_two
    local config_one
    local config_two
    local env_one
    local env_two
    local systemctl_stub
    local systemctl_log
    local snapshot
    local output
    local status
    local relative
    local -a managed_files=(
        usr/local/bin/cpa-monitor
        etc/cpa-monitor/config.yaml
        etc/cpa-monitor/cpa-monitor.env
        etc/systemd/system/cpa-monitor.service
        etc/systemd/system/cpa-monitor-once.service
        etc/systemd/system/cpa-monitor-check.service
        etc/systemd/system/cpa-monitor.timer
    )
    case_dir="$(new_case_dir)"
    root="${case_dir}/root"
    binary_one="${case_dir}/binary-one"
    binary_two="${case_dir}/binary-two"
    config_one="${case_dir}/config-one.yaml"
    config_two="${case_dir}/config-two.yaml"
    env_one="${case_dir}/env-one"
    env_two="${case_dir}/env-two"
    systemctl_stub="${case_dir}/systemctl"
    systemctl_log="${case_dir}/systemctl.log"
    snapshot="${case_dir}/snapshot"
    output="${case_dir}/signal-install.out"
    make_fake_binary "$binary_one" signal-one
    make_fake_binary "$binary_two" signal-two
    make_config "$config_one" signal-one
    make_config "$config_two" signal-two
    make_env_file "$env_one" signal-one
    make_env_file "$env_two" signal-two
    make_systemctl_stub "$systemctl_stub"
    : >"$systemctl_log"

    clean_env CPA_MONITOR_TEST_SYSTEMCTL="$systemctl_stub" SYSTEMCTL_LOG="$systemctl_log" \
        "$BASH_BIN" "$INSTALLER" \
        --root "$root" --binary "$binary_one" --config "$config_one" \
        --env-file "$env_one" --mode daemon --non-interactive >/dev/null 2>&1 \
        || fail "signal rollback baseline installation failed"

    for relative in "${managed_files[@]}"; do
        mkdir -p "${snapshot}/$(dirname "$relative")"
        cp -p "${root}/${relative}" "${snapshot}/${relative}"
    done
    : >"$systemctl_log"

    set +e
    clean_env \
        CPA_MONITOR_TEST_SYSTEMCTL="$systemctl_stub" \
        SYSTEMCTL_LOG="$systemctl_log" \
        SYSTEMCTL_SIGNAL_MATCH='restart cpa-monitor.service' \
        FLOCK_BIN=/opt/cpa-monitor-signal-test-flock \
        "$BASH_BIN" "$INSTALLER" \
        --root "$root" --binary "$binary_two" --config "$config_two" \
        --env-file "$env_two" --mode daemon --non-interactive >"$output" 2>&1
    status=$?
    set -e

    assert_eq 143 "$status" "SIGTERM status was not propagated"
    assert_contains "$output" 'restoring previous files and service state'
    for relative in "${managed_files[@]}"; do
        assert_same_file "${snapshot}/${relative}" "${root}/${relative}"
        assert_eq "$(file_mode "${snapshot}/${relative}")" "$(file_mode "${root}/${relative}")" "signal rollback mode mismatch for $relative"
    done
}

run_test 'bash syntax and help' test_syntax_and_help
run_test 'isolated first install and daemon systemctl flow' test_staged_first_install_and_daemon_activation
run_test 'timer systemctl flow' test_timer_activation
run_test 'generated YAML/environment quoting and weird root' test_generated_quoting_and_weird_root
run_test '--no-start skips runtime config check' test_no_start_skips_config_check
run_test 'preserve existing config and create unique backups' test_preserve_and_unique_backups
run_test 'reject managed symlink targets' test_symlink_target_rejected
run_test 'invalid or missing input leaves no files' test_invalid_and_missing_input_leave_no_files
run_test 'systemctl failure rolls files back' test_systemctl_failure_rolls_back_files
run_test 'SIGTERM rolls files back' test_signal_rolls_back_files

printf 'PASS: %d installer tests\n' "$TESTS_RUN"
