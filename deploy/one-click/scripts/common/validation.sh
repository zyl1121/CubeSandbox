# SPDX-License-Identifier: Apache-2.0
# Copyright (C) 2026 Tencent. All rights reserved.
#
# Shared validation helpers for one-click installer/runtime scripts.
# This file is sourced by callers that already define die().

if [[ "${ONE_CLICK_VALIDATION_LIB_LOADED:-0}" == "1" ]]; then
  return 0
fi
ONE_CLICK_VALIDATION_LIB_LOADED=1

if ! type die >/dev/null 2>&1; then
  die() {
    echo "[validation] ERROR: $*" >&2
    exit 1
  }
fi

ipv4_literal_is_valid() {
  local value="$1"
  local a b c d
  [[ "${value}" =~ ^[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+$ ]] || return 1
  IFS=. read -r a b c d <<< "${value}"
  local octet
  for octet in "${a}" "${b}" "${c}" "${d}"; do
    [[ "${octet}" =~ ^[0-9]{1,3}$ ]] || return 1
    (( 10#${octet} >= 0 && 10#${octet} <= 255 )) || return 1
  done
  return 0
}

validate_ipv4_literal() {
  local value="$1"
  local name="${2:-IPv4 address}"
  ipv4_literal_is_valid "${value}" \
    || die "invalid ${name}: ${value} (expected IPv4 address)"
}

validate_host_port() {
  local value="$1"
  local name="${2:-host:port}"
  local host port
  [[ -n "${value}" ]] || die "${name} must not be empty"
  # This value is written into env files and later into YAML/TOML snippets.
  # Keep it intentionally narrow: hostnames/IPv4 plus :port, no quotes,
  # whitespace, slash, comments, or control characters.
  [[ "${value}" =~ ^[A-Za-z0-9.-]+:[0-9]+$ ]] \
    || die "invalid ${name}: ${value} (expected host:port)"
  host="${value%:*}"
  port="${value##*:}"
  [[ -n "${host}" && -n "${port}" ]] || die "invalid ${name}: ${value}"
  (( 10#${port} >= 1 && 10#${port} <= 65535 )) \
    || die "invalid ${name}: ${value} (port out of range)"
}

canonicalize_resolv_conf_path() {
  local path="$1"
  if command -v realpath >/dev/null 2>&1; then
    realpath -e -- "${path}" 2>/dev/null || return 1
    return 0
  fi
  if command -v readlink >/dev/null 2>&1; then
    readlink -f -- "${path}" 2>/dev/null || return 1
    return 0
  fi
  printf '%s\n' "${path}"
}

resolv_conf_candidates() {
  printf '%s\n' \
    /etc/resolv.conf \
    /run/systemd/resolve/resolv.conf \
    /run/systemd/resolve/stub-resolv.conf \
    /run/NetworkManager/no-stub-resolv.conf \
    /run/NetworkManager/resolv.conf \
    /var/run/NetworkManager/no-stub-resolv.conf \
    /var/run/NetworkManager/resolv.conf \
    /run/resolvconf/resolv.conf \
    /etc/resolvconf/run/resolv.conf
}

