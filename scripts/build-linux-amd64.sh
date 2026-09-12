#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
artifact="${1:-${repo_root}/dist/cpa-key-policy_linux_amd64.so}"
# Debian 12 is the oldest supported runtime; building on a newer distribution
# can introduce libc symbols unavailable on the deployed CPA image.
builder="${CPAKP_BUILD_IMAGE:-golang:1.25-bookworm@sha256:3b4a11519ad929d1e1d261a12cff056f0c85b735253d7d861346b9c6f8b36437}"
mkdir -p -- "$(dirname -- "${artifact}")"
output_dir="$(cd -- "$(dirname -- "${artifact}")" && pwd)"
artifact="${output_dir}/$(basename -- "${artifact}")"
staging="$(mktemp -d "${output_dir}/.cpa-build.XXXXXX")"
cleanup() {
  rm -f -- "${staging}/plugin.so" "${staging}/plugin.h" \
    "${staging}/elf.txt" "${staging}/symbols.txt" "${staging}/ldd.txt" \
    "${staging}/abi-smoke.c" "${staging}/abi-smoke" "${staging}/build-info.txt"
  rmdir -- "${staging}"
}
trap cleanup EXIT

docker_args=(--rm -i --platform linux/amd64
  -v "${repo_root}:/src:ro" -v "${staging}:/out" -w /src)
if [[ -n "${CPAKP_GOMODCACHE:-}" ]]; then
  [[ -d "${CPAKP_GOMODCACHE}" ]] || { echo 'CPAKP_GOMODCACHE must be an existing module cache directory' >&2; exit 1; }
  docker_args+=(-v "${CPAKP_GOMODCACHE}:/go/pkg/mod:ro")
fi

docker run "${docker_args[@]}" "${builder}" bash -s <<'BUILD'
set -euo pipefail
export CGO_ENABLED=1 GOOS=linux GOARCH=amd64
go build -trimpath -buildvcs=false -tags cshared -buildmode=c-shared \
  -ldflags '-s -w' -o /out/plugin.so ./cmd/cpa-key-policy
readelf -h /out/plugin.so > /out/elf.txt
grep -Eq 'Class:.*ELF64' /out/elf.txt
grep -Eq 'Machine:.*X86-64' /out/elf.txt
readelf --dyn-syms --wide /out/plugin.so > /out/symbols.txt
grep -Eq '[[:space:]]cliproxy_plugin_init$' /out/symbols.txt
ldd -r /out/plugin.so > /out/ldd.txt 2>&1
if grep -E 'not found|undefined symbol' /out/ldd.txt; then
  exit 1
fi

cat > /out/abi-smoke.c <<'C'
#include <dlfcn.h>
#include <stdint.h>
#include <stdio.h>

typedef struct {
  uint32_t abi_version;
  void *call;
  void *free_buffer;
  void (*shutdown)(void);
} plugin_api;

int main(void) {
  void *library = dlopen("/out/plugin.so", RTLD_NOW | RTLD_LOCAL);
  if (!library) { fprintf(stderr, "%s\n", dlerror()); return 1; }
  int (*initialize)(void *, plugin_api *) = dlsym(library, "cliproxy_plugin_init");
  plugin_api api = {0};
  if (!initialize || initialize(NULL, &api) != 0 || api.abi_version != 1 ||
      !api.call || !api.free_buffer || !api.shutdown) return 1;
  api.shutdown();
  puts("Linux amd64 plugin ABI load passed");
  return 0;
}
C
gcc -Wall -Wextra -Werror /out/abi-smoke.c -ldl -o /out/abi-smoke
/out/abi-smoke
{
  go version
  ldd --version | head -1
} > /out/build-info.txt
BUILD

version="$(sed -n 's/^[[:space:]]*Version[[:space:]]*=[[:space:]]*"\([^"]*\)"/\1/p' "${repo_root}/internal/plugin/types.go")"
{
  printf 'plugin_version=%s\n' "${version}"
  printf 'source_commit=%s\n' "$(git -C "${repo_root}" rev-parse HEAD)"
  if [[ -n "$(git -C "${repo_root}" status --porcelain)" ]]; then
    printf 'source_dirty=true\n'
  else
    printf 'source_dirty=false\n'
  fi
  printf 'target=linux/amd64\nbuilder=%s\n' "${builder}"
  cat "${staging}/build-info.txt"
} > "${artifact}.build-info.txt"
mv -f -- "${staging}/plugin.so" "${artifact}"
(
  cd -- "${output_dir}"
  if command -v sha256sum >/dev/null; then
    sha256sum "$(basename -- "${artifact}")"
  else
    shasum -a 256 "$(basename -- "${artifact}")"
  fi
) > "${artifact}.sha256"
printf 'Built and verified %s\n' "${artifact}"
