#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
plugin_version="$(sed -n 's/^[[:space:]]*Version[[:space:]]*=[[:space:]]*"\([^"]*\)"/\1/p' "${repo_root}/internal/plugin/types.go")"
web_version="$(node -p "require('${repo_root}/web/package.json').version")"
lock_version="$(node -p "require('${repo_root}/web/package-lock.json').packages[''].version")"

[[ -n "${plugin_version}" ]] || { echo "plugin version not found" >&2; exit 1; }
[[ "${plugin_version}" == "${web_version}" ]] || {
  echo "version mismatch: plugin=${plugin_version} web=${web_version}" >&2
  exit 1
}
[[ "${plugin_version}" == "${lock_version}" ]] || {
  echo "version mismatch: plugin=${plugin_version} lock=${lock_version}" >&2
  exit 1
}

release_tag="${1:-}"
if [[ -z "${release_tag}" && "${GITHUB_REF_TYPE:-}" == "tag" ]]; then
  release_tag="${GITHUB_REF_NAME:-}"
fi
if [[ -n "${release_tag}" && "${release_tag}" == v* && "${release_tag#v}" != "${plugin_version}" ]]; then
  echo "version mismatch: tag=${release_tag} plugin=${plugin_version}" >&2
  exit 1
fi

printf 'version check passed: %s\n' "${plugin_version}"
