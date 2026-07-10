#!/usr/bin/env bash

set -Eeuo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd -P)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd -P)"
BOOTSTRAP="${REPO_ROOT}/bootstrap.sh"
TEST_TMP="$(mktemp -d "${TMPDIR:-/tmp}/cpa-monitor-bootstrap-test.XXXXXX")"
trap 'rm -rf "$TEST_TMP"' EXIT

fail() {
    printf 'FAIL: %s\n' "$*" >&2
    exit 1
}

assert_eq() {
    local expected="$1"
    local actual="$2"
    local message="$3"
    [[ "$expected" == "$actual" ]] || fail "${message}: expected '${expected}', got '${actual}'"
}

assert_contains() {
    local path="$1"
    local text="$2"
    grep -F -- "$text" "$path" >/dev/null 2>&1 || fail "expected '${text}' in ${path}"
}

source "$BOOTSTRAP"

assert_eq amd64 "$(normalize_architecture x86_64)" "x86_64 mapping"
assert_eq amd64 "$(normalize_architecture amd64)" "amd64 mapping"
assert_eq arm64 "$(normalize_architecture aarch64)" "aarch64 mapping"
assert_eq arm64 "$(normalize_architecture arm64)" "arm64 mapping"
if normalize_architecture riscv64 >/dev/null 2>&1; then
    fail "unsupported architecture was accepted"
fi
if validate_version '../main'; then
    fail "unsafe version was accepted"
fi

fixture_binary="${TEST_TMP}/fixture-binary"
fixture_installer="${TEST_TMP}/fixture-installer.sh"
fixture_checksums="${TEST_TMP}/checksums.txt"
fake_curl="${TEST_TMP}/fake-curl"
args_log="${TEST_TMP}/installer-args.log"
captured_binary="${TEST_TMP}/captured-binary"
output_log="${TEST_TMP}/bootstrap-output.log"

cat >"$fixture_binary" <<'EOF'
#!/bin/sh
case "${1:-}" in
    --help) exit 0 ;;
esac
exit 64
EOF
chmod 0755 "$fixture_binary"

cat >"$fixture_installer" <<'EOF'
#!/usr/bin/env bash
set -Eeuo pipefail
: >"${BOOTSTRAP_ARGS_LOG:?}"
binary=""
while (( $# > 0 )); do
    printf '%s\n' "$1" >>"$BOOTSTRAP_ARGS_LOG"
    if [[ "$1" == "--binary" ]]; then
        binary="$2"
    fi
    shift
done
[[ -n "$binary" ]]
cp "$binary" "${BOOTSTRAP_CAPTURED_BINARY:?}"
EOF
chmod 0755 "$fixture_installer"

architecture="$(normalize_architecture "$(uname -m)")" || fail "test host architecture is unsupported"
asset_name="cpa-monitor-linux-${architecture}"
printf '%s  %s\n' "$(sha256_file "$fixture_binary")" "$asset_name" >"$fixture_checksums"

cat >"$fake_curl" <<'EOF'
#!/usr/bin/env bash
set -Eeuo pipefail
output=""
write_format=""
url=""
while (( $# > 0 )); do
    case "$1" in
        -o|-w|--proto|--retry|--connect-timeout)
            if [[ "$1" == "-o" ]]; then output="$2"; fi
            if [[ "$1" == "-w" ]]; then write_format="$2"; fi
            shift 2
            ;;
        --tlsv1.2|-fsSL)
            shift
            ;;
        *)
            url="$1"
            shift
            ;;
    esac
done

case "$url" in
    */releases/latest)
        [[ "$write_format" == '%{url_effective}' ]]
        printf '%s' 'https://github.com/mokexinxin/cpa-monitor/releases/tag/v9.8.7'
        ;;
    */checksums.txt)
        cp "${BOOTSTRAP_FIXTURE_CHECKSUMS:?}" "$output"
        ;;
    */cpa-monitor-linux-*)
        cp "${BOOTSTRAP_FIXTURE_BINARY:?}" "$output"
        ;;
    */install.sh)
        cp "${BOOTSTRAP_FIXTURE_INSTALLER:?}" "$output"
        ;;
    *)
        printf 'unexpected URL: %s\n' "$url" >&2
        exit 1
        ;;
esac
EOF
chmod 0755 "$fake_curl"

BOOTSTRAP_FIXTURE_BINARY="$fixture_binary" \
BOOTSTRAP_FIXTURE_INSTALLER="$fixture_installer" \
BOOTSTRAP_FIXTURE_CHECKSUMS="$fixture_checksums" \
BOOTSTRAP_ARGS_LOG="$args_log" \
BOOTSTRAP_CAPTURED_BINARY="$captured_binary" \
CPA_MONITOR_BOOTSTRAP_TESTING=true \
CPA_MONITOR_CURL_BIN="$fake_curl" \
bash "$BOOTSTRAP" --non-interactive --mode timer --root /staged/root >"$output_log" 2>&1 \
    || fail "bootstrap integration test failed"

cmp -s "$fixture_binary" "$captured_binary" || fail "downloaded binary did not reach installer intact"
assert_contains "$args_log" '--binary'
assert_contains "$args_log" '--non-interactive'
assert_contains "$args_log" '--mode'
assert_contains "$args_log" 'timer'
assert_contains "$args_log" '/staged/root'
assert_contains "$output_log" 'downloading cpa-monitor v9.8.7'
assert_contains "$output_log" 'release checksum verified'

printf '%064d  %s\n' 0 "$asset_name" >"$fixture_checksums"
rm -f "$args_log"
set +e
BOOTSTRAP_FIXTURE_BINARY="$fixture_binary" \
BOOTSTRAP_FIXTURE_INSTALLER="$fixture_installer" \
BOOTSTRAP_FIXTURE_CHECKSUMS="$fixture_checksums" \
BOOTSTRAP_ARGS_LOG="$args_log" \
BOOTSTRAP_CAPTURED_BINARY="$captured_binary" \
CPA_MONITOR_BOOTSTRAP_TESTING=true \
CPA_MONITOR_CURL_BIN="$fake_curl" \
bash "$BOOTSTRAP" --non-interactive >"$output_log" 2>&1
status=$?
set -e
(( status != 0 )) || fail "checksum mismatch should fail"
[[ ! -e "$args_log" ]] || fail "installer ran after checksum mismatch"
assert_contains "$output_log" 'checksum verification failed'

printf 'PASS: bootstrap tests\n'
