#!/bin/bash
# CubeProxy container entrypoint.
#
# Layout:
#   - Foreground: openresty/nginx (PID 1's main duty after exec)
#   - Background: cube-proxy-sidecar, lifecycle coordination loop
#   - Background: crond, log rotation
#
# The sidecar binary is shipped inside this image rather than as a separate
# container so the lifecycle (auto-pause / auto-resume) feature is always
# co-resident with nginx. The binary is REQUIRED — if it is missing or
# unreadable we abort the entrypoint instead of starting a half-functional
# container that quietly drops the auto-pause feature. The Dockerfile
# performs the same sanity checks at build time; this is the runtime
# belt-and-braces.

set -u

SIDECAR_BIN="${SIDECAR_BIN:-/usr/local/openresty/nginx/sbin/cube-proxy-sidecar}"
SIDECAR_LOG="${SIDECAR_LOG:-/data/log/cube-proxy/sidecar.log}"
NGINX_TEMPLATE_PATH="${CUBE_PROXY_NGINX_TEMPLATE_PATH:-/usr/local/openresty/nginx/conf/nginx.conf.template}"
NGINX_CONFIG_PATH="${CUBE_PROXY_NGINX_CONFIG_PATH:-/usr/local/openresty/nginx/conf/nginx.conf}"

die() {
  echo "$(date -Iseconds) FATAL: $*" >&2
  return 1
}

ipv4_literal_is_valid() {
  local value="${1:-}"
  local a b c d octet
  [[ "${value}" =~ ^[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+$ ]] || return 1
  IFS=. read -r a b c d <<< "${value}"
  for octet in "${a}" "${b}" "${c}" "${d}"; do
    [[ "${octet}" =~ ^[0-9]{1,3}$ ]] || return 1
    (( 10#${octet} >= 0 && 10#${octet} <= 255 )) || return 1
  done
  return 0
}

ipv6_literal_is_valid() {
  local value="${1:-}"
  [[ -n "${value}" ]] || return 1
  value="${value#\[}"
  value="${value%\]}"
  [[ "${value}" == *:* ]] || return 1
  [[ "${value}" =~ ^[0-9A-Fa-f:.]+$ ]] || return 1
  return 0
}

is_ip_literal() {
  local value="${1:-}"
  ipv4_literal_is_valid "${value}" || ipv6_literal_is_valid "${value}"
}

escape_sed() {
  local value="$1"
  local delimiter="${2:-/}"
  value="${value//\\/\\\\}"
  value="${value//&/\\&}"
  if [[ "${delimiter}" != "/" ]]; then
    value="${value//${delimiter}/\\${delimiter}}"
  else
    value="${value//\//\\/}"
  fi
  printf '%s' "${value}"
}

discover_resolver_nameservers() {
  local path="${1:-${CUBE_PROXY_RESOLV_CONF:-/etc/resolv.conf}}"
  [[ -f "${path}" ]] || return 0

  local line keyword nameserver
  declare -A seen_nameservers=()
  while IFS= read -r line || [[ -n "${line}" ]]; do
    read -r keyword nameserver _ <<< "${line}"
    [[ "${keyword:-}" == "nameserver" ]] || continue
    [[ -n "${nameserver}" ]] || continue
    ipv4_literal_is_valid "${nameserver}" || continue
    [[ -n "${seen_nameservers[${nameserver}]:-}" ]] && continue
    seen_nameservers["${nameserver}"]=1
    printf '%s\n' "${nameserver}"
  done < "${path}"
}

build_cube_proxy_resolver_directives() {
  local resolver_list="${1:-}"
  [[ -n "${resolver_list}" ]] || return 0
  printf 'resolver %s ipv6=off valid=30s; resolver_timeout 5s;' "${resolver_list}"
}

ensure_hostname_target_has_resolver() {
  local target="${1:-}"
  local resolver_list="${2:-}"
  local target_var="${3:-target}"

  if ! is_ip_literal "${target}" && [[ -z "${resolver_list}" ]]; then
    die "${target_var} '${target}' is not an IP literal, but no nginx resolver nameserver could be discovered from ${CUBE_PROXY_RESOLV_CONF:-/etc/resolv.conf}. IPv6-only resolvers are not currently supported. Set CUBE_PROXY_NGINX_RESOLVER explicitly or ensure the container resolv.conf has at least one IPv4 nameserver."
  fi
}

render_nginx_config() {
  local redis_host="${CUBE_PROXY_REDIS_IP:-127.0.0.1}"
  local resolver_list="${CUBE_PROXY_NGINX_RESOLVER:-}"
  local http_port="${CUBE_PROXY_HTTP_PORT:-8081}"
  local https_port="${CUBE_PROXY_HTTPS_PORT:-8080}"
  local ssl_cert="${CUBE_PROXY_SSL_CERT:-cube.app+3.pem}"
  local ssl_key="${CUBE_PROXY_SSL_KEY:-cube.app+3-key.pem}"
  local resolver_directives tmp

  if [[ ! -f "${NGINX_TEMPLATE_PATH}" ]]; then
    die "nginx config template not found: ${NGINX_TEMPLATE_PATH}" || return 1
  fi

  if [[ -z "${resolver_list}" ]]; then
    local -a discovered_nameservers=()
    mapfile -t discovered_nameservers < <(discover_resolver_nameservers)
    if [[ "${#discovered_nameservers[@]}" -gt 0 ]]; then
      resolver_list="${discovered_nameservers[*]}"
    fi
  fi

  ensure_hostname_target_has_resolver "${redis_host}" "${resolver_list}" "CUBE_PROXY_REDIS_IP" || return 1
  resolver_directives="$(build_cube_proxy_resolver_directives "${resolver_list}")"

  mkdir -p "$(dirname "${NGINX_CONFIG_PATH}")"
  tmp="${NGINX_CONFIG_PATH}.tmp"
  if ! sed \
    -e "s#__CUBE_PROXY_RESOLVER_DIRECTIVES__#$(escape_sed "${resolver_directives}" '#')#g" \
    -e "s/__CUBE_PROXY_HTTP_PORT__/$(escape_sed "${http_port}")/g" \
    -e "s/__CUBE_PROXY_HTTPS_PORT__/$(escape_sed "${https_port}")/g" \
    -e "s/__CUBE_PROXY_SSL_CERT__/$(escape_sed "${ssl_cert}")/g" \
    -e "s/__CUBE_PROXY_SSL_KEY__/$(escape_sed "${ssl_key}")/g" \
    "${NGINX_TEMPLATE_PATH}" > "${tmp}"; then
    rm -f "${tmp}"
    die "failed to render nginx config from template: ${NGINX_TEMPLATE_PATH}" || return 1
  fi

  if grep -Eq '__CUBE_PROXY_[A-Z0-9_]+__' "${tmp}"; then
    rm -f "${tmp}"
    die "rendered nginx config still contains unsubstituted placeholders" || return 1
  fi

  mv -f "${tmp}" "${NGINX_CONFIG_PATH}"
}

start_sidecar() {
  if [[ ! -x "${SIDECAR_BIN}" ]]; then
    echo "$(date -Iseconds) FATAL: cube-proxy-sidecar binary missing or not executable at ${SIDECAR_BIN}" >&2
    echo "$(date -Iseconds)        rebuild the cube-proxy image (CubeProxy/Makefile prebuild-sidecar)" >&2
    return 1
  fi

  # Loop in the background so a sidecar crash auto-restarts without taking
  # nginx down with it. Exponential-ish backoff bounded at 30s.
  (
    backoff=1
    while true; do
      "${SIDECAR_BIN}" >>"${SIDECAR_LOG}" 2>&1 &
      sidecar_pid=$!
      wait "${sidecar_pid}"
      rc=$?
      echo "$(date -Iseconds) cube-proxy-sidecar exited rc=${rc}; restarting in ${backoff}s" >>"${SIDECAR_LOG}"
      sleep "${backoff}"
      if [[ "${backoff}" -lt 30 ]]; then
        backoff=$((backoff * 2))
        [[ "${backoff}" -gt 30 ]] && backoff=30
      fi
    done
  ) &
  echo "$(date -Iseconds) cube-proxy-sidecar supervisor started (logs: ${SIDECAR_LOG})" >&2
}

main() {
  mkdir -p "$(dirname "${SIDECAR_LOG}")"
  render_nginx_config || exit 1

  /usr/sbin/crond
  # Abort the entrypoint if the sidecar can't be brought up — nginx alone
  # would silently mishandle paused sandboxes (returning 503 forever).
  start_sidecar || exit 1
  exec /usr/local/openresty/nginx/sbin/nginx
}

if [[ "${BASH_SOURCE[0]}" == "$0" ]]; then
  main "$@"
fi
