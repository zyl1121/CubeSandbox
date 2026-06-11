#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ONE_CLICK_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"

TMP_DIR="$(mktemp -d)"
cleanup() {
  rm -rf "${TMP_DIR}"
}
trap cleanup EXIT

# shellcheck source=../lib/common.sh
source "${ONE_CLICK_DIR}/lib/common.sh"

_resolv_conf_candidates() {
  if [[ -n "${TEST_RESOLV_CONF:-}" ]]; then
    printf '%s\n' "${TEST_RESOLV_CONF}"
  fi
}

fail() {
  echo "FAIL: $*" >&2
  exit 1
}

assert_contains_text() {
  local text="$1"
  local needle="$2"
  grep -Fq -- "${needle}" <<<"${text}" || fail "expected text to contain ${needle}; got: ${text}"
}

assert_contains_file() {
  local path="$1"
  local needle="$2"
  grep -Fq -- "${needle}" "${path}" || fail "expected ${path} to contain ${needle}"
}

assert_occurrence_count() {
  local text="$1"
  local needle="$2"
  local expected="$3"
  local count
  count="$(grep -Foc -- "${needle}" <<<"${text}")"
  [[ "${count}" == "${expected}" ]] || fail "expected ${needle} to appear ${expected} times; got ${count}: ${text}"
}

assert_not_contains_text() {
  local text="$1"
  local needle="$2"
  if grep -Fq -- "${needle}" <<<"${text}"; then
    fail "expected text not to contain ${needle}; got: ${text}"
  fi
}

line_number() {
  local path="$1"
  local needle="$2"
  local line
  line="$(grep -nF -- "${needle}" "${path}" | head -n1 | cut -d: -f1)"
  [[ -n "${line}" ]] || fail "expected ${path} to contain ${needle}"
  printf '%s\n' "${line}"
}

last_line_number() {
  local path="$1"
  local needle="$2"
  local line
  line="$(grep -nF -- "${needle}" "${path}" | tail -n1 | cut -d: -f1)"
  [[ -n "${line}" ]] || fail "expected ${path} to contain ${needle}"
  printf '%s\n' "${line}"
}

assert_line_before_last() {
  local path="$1"
  local before="$2"
  local after="$3"
  local before_line
  local after_line
  before_line="$(line_number "${path}" "${before}")"
  after_line="$(last_line_number "${path}" "${after}")"
  (( before_line < after_line )) || fail "expected ${before} before final ${after} in ${path}"
}

ip() {
  case "$*" in
    "-4 addr show scope global")
      case "${IP_SCENARIO:-empty}" in
        host_iface_conflict)
          printf '    inet %s brd %s scope global eth0\n' \
            "${TEST_HOST_IFACE_CIDR:?}" \
            "${TEST_HOST_IFACE_BROADCAST:?}"
          ;;
        cube_owned_only)
          printf '    inet %s scope global cube-dev\n' "${TEST_CUBE_DEV_CIDR:?}"
          printf '    inet %s scope global z%s\n' "${TEST_TAP_CIDR:?}" "${TEST_TAP_IP:?}"
          ;;
        *)
          ;;
      esac
      ;;
    "-4 route show")
      case "${IP_SCENARIO:-empty}" in
        host_route_conflict)
          printf '%s dev eth0 proto kernel scope link src %s\n' \
            "${TEST_HOST_ROUTE_CIDR:?}" \
            "${TEST_HOST_ROUTE_SRC_IP:?}"
          ;;
        cube_owned_only)
          printf '%s dev cube-dev proto static scope link src %s\n' \
            "${TEST_CUBE_ROUTE_CIDR:?}" \
            "${TEST_CUBE_DEV_IP:?}"
          printf '%s dev z%s scope link\n' "${TEST_TAP_ROUTE_CIDR:?}" "${TEST_TAP_IP:?}"
          ;;
        cube_route_then_plain)
          printf '%s dev cube-dev proto static scope link src %s\n' \
            "${TEST_CUBE_ROUTE_CIDR:?}" \
            "${TEST_CUBE_DEV_IP:?}"
          printf '%s proto static scope link src %s\n' \
            "${TEST_PLAIN_ROUTE_CIDR:?}" \
            "${TEST_PLAIN_ROUTE_SRC_IP:?}"
          ;;
        *)
          ;;
      esac
      ;;
    *)
      return 1
      ;;
  esac
}

test_rejects_host_interface_overlap() {
  local cidr="10.77.0.0/16"
  local host_iface_cidr="10.77.1.123/24"
  local host_iface_broadcast="10.77.1.255"
  local output
  if output="$(
    TEST_HOST_IFACE_CIDR="${host_iface_cidr}" \
    TEST_HOST_IFACE_BROADCAST="${host_iface_broadcast}" \
    IP_SCENARIO=host_iface_conflict \
    check_cidr_preflight "${cidr}" "Cubelet config cidr" 2>&1
  )"; then
    fail "expected host interface overlap to be rejected"
  fi
  assert_contains_text "${output}" "Cubelet config cidr '${cidr}' conflicts"
  assert_contains_text "${output}" "interface eth0 (${host_iface_cidr})"
}

test_rejects_host_route_overlap() {
  local cidr="10.77.0.0/16"
  local host_route_cidr="10.77.2.0/24"
  local host_route_src_ip="10.77.2.123"
  local output
  if output="$(
    TEST_HOST_ROUTE_CIDR="${host_route_cidr}" \
    TEST_HOST_ROUTE_SRC_IP="${host_route_src_ip}" \
    IP_SCENARIO=host_route_conflict \
    check_cidr_preflight "${cidr}" "Cubelet config cidr" 2>&1
  )"; then
    fail "expected host route overlap to be rejected"
  fi
  assert_contains_text "${output}" "Cubelet config cidr '${cidr}' conflicts"
  assert_contains_text "${output}" "route ${host_route_cidr}"
}

test_rejects_nameserver_overlap() {
  local resolv_conf="${TMP_DIR}/resolv.conf"
  local cidr="10.77.0.0/16"
  local nameserver="10.77.0.53"
  local output
  printf 'nameserver %s\n' "${nameserver}" > "${resolv_conf}"

  if output="$(TEST_RESOLV_CONF="${resolv_conf}" IP_SCENARIO=empty check_cidr_preflight "${cidr}" "Cubelet config cidr" 2>&1)"; then
    fail "expected nameserver overlap to be rejected"
  fi
  assert_contains_text "${output}" "Cubelet config cidr '${cidr}' conflicts"
  assert_contains_text "${output}" "nameserver ${nameserver} (${resolv_conf})"
}

test_dedupes_equivalent_resolver_paths() {
  local resolv_dir="${TMP_DIR}/resolv"
  local real_resolv_conf="${resolv_dir}/real-resolv.conf"
  local alias_resolv_conf="${resolv_dir}/alias-resolv.conf"
  local resolv_candidates
  local cidr="10.77.0.0/16"
  local nameserver="10.77.0.53"
  local output

  mkdir -p "${resolv_dir}"
  printf 'nameserver %s\n' "${nameserver}" > "${real_resolv_conf}"
  ln -s "${real_resolv_conf}" "${alias_resolv_conf}"
  resolv_candidates="${real_resolv_conf}"$'\n'"${alias_resolv_conf}"

  if output="$(
    TEST_RESOLV_CONF="${resolv_candidates}" \
    IP_SCENARIO=empty \
    check_cidr_preflight "${cidr}" "Cubelet config cidr" 2>&1
  )"; then
    fail "expected nameserver overlap to be rejected"
  fi
  assert_occurrence_count "${output}" "nameserver ${nameserver}" 1
}

test_does_not_leak_route_iface_across_iterations() {
  local cidr="10.77.0.0/16"
  local cube_route_cidr="10.77.0.0/24"
  local cube_dev_ip="10.77.0.1"
  local plain_route_cidr="10.77.2.0/24"
  local plain_route_src_ip="10.77.2.123"
  local output

  if output="$(
    TEST_CUBE_ROUTE_CIDR="${cube_route_cidr}" \
    TEST_CUBE_DEV_IP="${cube_dev_ip}" \
    TEST_PLAIN_ROUTE_CIDR="${plain_route_cidr}" \
    TEST_PLAIN_ROUTE_SRC_IP="${plain_route_src_ip}" \
    IP_SCENARIO=cube_route_then_plain \
    check_cidr_preflight "${cidr}" "Cubelet config cidr" 2>&1
  )"; then
    fail "expected plain route conflict to be detected after cube-owned route"
  fi
  assert_contains_text "${output}" "ignored 1 existing Cube-owned network entries during CIDR conflict check"
  assert_contains_text "${output}" "route ${plain_route_cidr}"
}

test_accepts_empty_cidr() {
  IP_SCENARIO=host_iface_conflict check_cidr_preflight "" "Cubelet config cidr" >/dev/null 2>&1
}

test_bypass_still_rejects_malformed_cidr() {
  local output
  if output="$(
    CUBE_SANDBOX_NETWORK_CIDR_SKIP_CONFLICT_CHECK=1 \
    IP_SCENARIO=empty \
    check_cidr_preflight "10.77.1.99/24" "CUBE_SANDBOX_NETWORK_CIDR" 2>&1
  )"; then
    fail "expected malformed CIDR to be rejected even with bypass enabled"
  fi
  assert_contains_text "${output}" "Did you mean: 10.77.1.0/24?"
  assert_not_contains_text "${output}" "conflict check SKIPPED"
}

test_bypass_skips_conflict_detection() {
  local output
  if ! output="$(
    CUBE_SANDBOX_NETWORK_CIDR_SKIP_CONFLICT_CHECK=1 \
    IP_SCENARIO=host_iface_conflict \
    TEST_HOST_IFACE_CIDR="10.77.1.123/24" \
    TEST_HOST_IFACE_BROADCAST="10.77.1.255" \
    check_cidr_preflight "10.77.0.0/16" "Cubelet config cidr" 2>&1
  )"; then
    fail "expected bypass to skip conflict detection for a valid CIDR"
  fi
  assert_contains_text "${output}" "CUBE_SANDBOX_NETWORK_CIDR conflict check SKIPPED (bypass flag set)"
  assert_not_contains_text "${output}" "conflicts with existing host network"
}

test_accepts_cidr_mask_boundaries() {
  IP_SCENARIO=empty check_cidr_preflight "10.0.0.0/8" "Cubelet config cidr" >/dev/null 2>&1
  IP_SCENARIO=empty check_cidr_preflight "10.77.1.0/30" "Cubelet config cidr" >/dev/null 2>&1
}

test_rejects_cidr_mask_outside_boundaries() {
  local output
  if output="$(IP_SCENARIO=empty check_cidr_preflight "10.0.0.0/31" "Cubelet config cidr" 2>&1)"; then
    fail "expected /31 to be rejected"
  fi
  assert_contains_text "${output}" "mask must be between 8 and 30"

  if output="$(IP_SCENARIO=empty check_cidr_preflight "10.0.0.0/32" "Cubelet config cidr" 2>&1)"; then
    fail "expected /32 to be rejected"
  fi
  assert_contains_text "${output}" "mask must be between 8 and 30"
}

test_ignores_cube_owned_networks() {
  local cidr="10.77.0.0/16"
  local cube_dev_cidr="10.77.0.1/16"
  local cube_dev_ip="10.77.0.1"
  local tap_ip="10.77.0.10"
  local tap_cidr="${tap_ip}/32"
  local tap_route_cidr="${tap_ip}/32"

  TEST_CUBE_DEV_CIDR="${cube_dev_cidr}" \
  TEST_CUBE_ROUTE_CIDR="${cidr}" \
  TEST_CUBE_DEV_IP="${cube_dev_ip}" \
  TEST_TAP_IP="${tap_ip}" \
  TEST_TAP_CIDR="${tap_cidr}" \
  TEST_TAP_ROUTE_CIDR="${tap_route_cidr}" \
  IP_SCENARIO=cube_owned_only \
  check_cidr_preflight "${cidr}" "Cubelet config cidr" >/dev/null 2>&1
}

test_cube_owned_netdev_matching_is_narrow() {
  is_cube_owned_netdev "cube-dev" || fail "expected cube-dev to be Cube-owned"
  is_cube_owned_netdev "z10.77.0.10" || fail "expected z10.77.0.10 to be Cube-owned"
  is_cube_owned_netdev "z10.77.0.10@if123" || fail "expected peer suffix to be ignored"

  if is_cube_owned_netdev "zfoo"; then
    fail "expected zfoo not to be Cube-owned"
  fi
  if is_cube_owned_netdev "z10.77.0"; then
    fail "expected incomplete z<ip> name not to be Cube-owned"
  fi
}

test_rejects_invalid_cidr() {
  local output
  if output="$(IP_SCENARIO=empty check_cidr_preflight "10.77.1.99/24" "CUBE_SANDBOX_NETWORK_CIDR" 2>&1)"; then
    fail "expected non-canonical CIDR to be rejected"
  fi
  assert_contains_text "${output}" "Did you mean: 10.77.1.0/24?"
}

test_reads_network_cidr_from_cubelet_config() {
  local config="${TMP_DIR}/config.toml"
  cat >"${config}" <<'EOF'
[plugins."io.cubelet.internal.v1.other"]
  cidr = "10.10.0.0/16"

[plugins."io.cubelet.internal.v1.network"]
  object_dir = "/usr/local/services/cubetoolbox/cube-vs/network"
  cidr = "172.31.64.0/18"
EOF

  local cidr
  cidr="$(cubelet_network_cidr_from_config "${config}")"
  [[ "${cidr}" == "172.31.64.0/18" ]] || fail "expected 172.31.64.0/18, got ${cidr}"
}

test_reads_network_cidr_from_single_quoted_config() {
  local config="${TMP_DIR}/single-quoted-config.toml"
  cat >"${config}" <<'EOF'
[plugins."io.cubelet.internal.v1.network"]
  cidr = '172.31.64.0/18' # inline comment
EOF

  local cidr
  cidr="$(cubelet_network_cidr_from_config "${config}")"
  [[ "${cidr}" == "172.31.64.0/18" ]] || fail "expected 172.31.64.0/18, got ${cidr}"
}

test_install_wires_effective_cidr_preflight_before_mutations() {
  local install_script="${ONE_CLICK_DIR}/install.sh"

  assert_contains_file "${install_script}" 'CUBELET_PACKAGE_CONFIG="${PKG_ROOT}/Cubelet/config/config.toml"'
  assert_contains_file "${install_script}" 'check_cidr_preflight "${CUBE_SANDBOX_NETWORK_CIDR}" "CUBE_SANDBOX_NETWORK_CIDR"'
  assert_contains_file "${install_script}" 'CUBE_SANDBOX_EFFECTIVE_NETWORK_CIDR="$(cubelet_network_cidr_from_config "${CUBELET_PACKAGE_CONFIG}")"'
  assert_contains_file "${install_script}" 'check_cidr_preflight "${CUBE_SANDBOX_EFFECTIVE_NETWORK_CIDR}" "Cubelet config cidr"'

  assert_line_before_last "${install_script}" 'check_cidr_preflight "${CUBE_SANDBOX_NETWORK_CIDR}" "CUBE_SANDBOX_NETWORK_CIDR"' 'install_required_dependencies'
  assert_line_before_last "${install_script}" 'check_cidr_preflight "${CUBE_SANDBOX_EFFECTIVE_NETWORK_CIDR}" "Cubelet config cidr"' 'install_required_dependencies'
  assert_line_before_last "${install_script}" 'check_cidr_preflight "${CUBE_SANDBOX_EFFECTIVE_NETWORK_CIDR}" "Cubelet config cidr"' 'stop_existing_systemd_deployment'
}

test_rejects_host_interface_overlap
test_rejects_host_route_overlap
test_rejects_nameserver_overlap
test_dedupes_equivalent_resolver_paths
test_does_not_leak_route_iface_across_iterations
test_accepts_empty_cidr
test_bypass_still_rejects_malformed_cidr
test_bypass_skips_conflict_detection
test_accepts_cidr_mask_boundaries
test_rejects_cidr_mask_outside_boundaries
test_ignores_cube_owned_networks
test_cube_owned_netdev_matching_is_narrow
test_rejects_invalid_cidr
test_reads_network_cidr_from_cubelet_config
test_reads_network_cidr_from_single_quoted_config
test_install_wires_effective_cidr_preflight_before_mutations

echo "cidr preflight tests OK"
