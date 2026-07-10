#!/usr/bin/env bash

# Remote bootstrap for cpa-monitor.
#
# Intended usage:
#   curl -fsSL https://raw.githubusercontent.com/mokexinxin/cpa-monitor/main/bootstrap.sh | sudo bash

set +x
set -Eeuo pipefail
umask 077

REPOSITORY="mokexinxin/cpa-monitor"
VERSION="${CPA_MONITOR_VERSION:-latest}"
CURL_BIN="${CPA_MONITOR_CURL_BIN:-}"
BOOTSTRAP_TESTING="${CPA_MONITOR_BOOTSTRAP_TESTING:-false}"
TMP_DIR=""

log() {
    printf '[cpa-monitor-bootstrap] %s\n' "$*"
}

die() {
    printf '[cpa-monitor-bootstrap] ERROR: %s\n' "$*" >&2
    exit 1
}

cleanup() {
    if [[ -n "$TMP_DIR" && -d "$TMP_DIR" ]]; then
        rm -rf "$TMP_DIR"
    fi
}

on_signal() {
    local status="$1"
    trap - INT TERM HUP
    exit "$status"
}

normalize_architecture() {
    case "$1" in
        x86_64|amd64) printf '%s' amd64 ;;
        aarch64|arm64) printf '%s' arm64 ;;
        *) return 1 ;;
    esac
}

validate_version() {
    local version="$1"
    [[ -n "$version" ]] || return 1
    [[ "$version" =~ ^[A-Za-z0-9][A-Za-z0-9._-]*$ ]] || return 1
}

download() {
    local url="$1"
    local output="$2"
    "$CURL_BIN" \
        --proto '=https' \
        --tlsv1.2 \
        --retry 3 \
        --connect-timeout 15 \
        -fsSL \
        -o "$output" \
        "$url"
}

resolve_version() {
    local effective_url
    if [[ "$VERSION" != "latest" ]]; then
        validate_version "$VERSION" || die "CPA_MONITOR_VERSION contains unsupported characters"
        return
    fi

    if ! effective_url="$(
        "$CURL_BIN" \
            --proto '=https' \
            --tlsv1.2 \
            --retry 3 \
            --connect-timeout 15 \
            -fsSL \
            -o /dev/null \
            -w '%{url_effective}' \
            "https://github.com/${REPOSITORY}/releases/latest"
    )"; then
        die "could not resolve the latest GitHub release"
    fi
    VERSION="${effective_url##*/}"
    validate_version "$VERSION" || die "could not resolve the latest release version"
    [[ "$VERSION" != "latest" ]] || die "the repository does not have a published release yet"
}

sha256_file() {
    local path="$1"
    if command -v sha256sum >/dev/null 2>&1; then
        sha256sum "$path" | awk '{print $1}'
    elif command -v shasum >/dev/null 2>&1; then
        shasum -a 256 "$path" | awk '{print $1}'
    else
        die "sha256sum or shasum is required to verify the release"
    fi
}

verify_release() {
    local binary="$1"
    local asset_name="$2"
    local checksums="$3"
    local expected
    local actual

    expected="$(awk -v name="$asset_name" '$2 == name || $2 == "*" name { print $1 }' "$checksums")"
    [[ "$expected" =~ ^[0-9a-fA-F]{64}$ ]] || die "release checksum is missing for ${asset_name}"
    actual="$(sha256_file "$binary")"
    [[ "${actual}" == "${expected}" ]] || die "release checksum verification failed for ${asset_name}"
}

run_installer() {
    local installer="$1"
    local binary="$2"
    shift 2
    local non_interactive=false
    local argument

    for argument in "$@"; do
        if [[ "$argument" == "--non-interactive" ]]; then
            non_interactive=true
        fi
    done

    if [[ "$non_interactive" != "true" && ! -t 0 ]]; then
        [[ -r /dev/tty ]] || die "no interactive terminal is available; use --non-interactive"
        bash "$installer" --binary "$binary" "$@" </dev/tty
    else
        bash "$installer" --binary "$binary" "$@"
    fi
}

main() {
    local architecture
    local asset_name
    local release_base
    local binary
    local checksums
    local installer

    if [[ "$BOOTSTRAP_TESTING" != "true" ]]; then
        [[ "$(uname -s)" == "Linux" ]] || die "remote installation requires Linux"
        (( EUID == 0 )) || die "run the bootstrap as root (pipe it to sudo bash)"
    fi

    if [[ -z "$CURL_BIN" ]]; then
        CURL_BIN="$(command -v curl || true)"
    fi
    [[ -n "$CURL_BIN" && -x "$CURL_BIN" ]] || die "curl is required"
    command -v awk >/dev/null 2>&1 || die "awk is required"
    command -v mktemp >/dev/null 2>&1 || die "mktemp is required"

    architecture="$(normalize_architecture "$(uname -m)")" || die "unsupported architecture: $(uname -m)"
    resolve_version

    TMP_DIR="$(mktemp -d "${TMPDIR:-/tmp}/cpa-monitor-bootstrap.XXXXXX")"
    trap cleanup EXIT
    trap 'on_signal 130' INT
    trap 'on_signal 143' TERM
    trap 'on_signal 129' HUP

    asset_name="cpa-monitor-linux-${architecture}"
    release_base="https://github.com/${REPOSITORY}/releases/download/${VERSION}"
    binary="${TMP_DIR}/${asset_name}"
    checksums="${TMP_DIR}/checksums.txt"
    installer="${TMP_DIR}/install.sh"

    log "downloading cpa-monitor ${VERSION} for linux/${architecture}"
    download "${release_base}/${asset_name}" "$binary"
    download "${release_base}/checksums.txt" "$checksums"
    verify_release "$binary" "$asset_name" "$checksums"
    chmod 0755 "$binary"

    download "https://raw.githubusercontent.com/${REPOSITORY}/${VERSION}/install.sh" "$installer"
    chmod 0700 "$installer"
    log "release checksum verified; starting system installation"
    run_installer "$installer" "$binary" "$@"
}

if [[ -z "${BASH_SOURCE[0]:-}" || "${BASH_SOURCE[0]}" == "$0" ]]; then
    main "$@"
fi
