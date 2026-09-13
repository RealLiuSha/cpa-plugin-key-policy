#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
host_field="$(printf '\141\154\151\141\163')"
host_field_title="$(printf '\101\154\151\141\163')"
reset_marker="$(printf '\162\145\163\145\164')"
compound_symbols="${host_field_title}(Mapping|Target|Resolver|Config)|Key${host_field_title}Ref|By${host_field_title}"
# V5 supports storage migration. Keep checking the removed model contracts,
# without rejecting ordinary words used by the data migration implementation.
pattern="\\b${host_field}(es)?\\b|${compound_symbols}|by_${host_field}|daily_${reset_marker}_at|weekly_${reset_marker}_at"

matches="$({
  rg -n -i \
    --glob '!.git/**' \
    --glob '!goals/**' \
    --glob '!memories/**' \
    --glob '!web/node_modules/**' \
    --glob '!web/*.tsbuildinfo' \
    --glob '!internal/plugin/web/dist/index.html' \
    "${pattern}" "${repo_root}" || true
})"

unexpected=""
while IFS=: read -r file line content; do
  [[ -n "${file}" ]] || continue
  relative="${file#"${repo_root}/"}"
  case "${relative}" in
    internal/plugin/types.go)
      [[ "${content}" == *$'\t'"${host_field_title}"$' string `json:"'"${host_field_title}"$'"`'* ]] || unexpected+="${relative}:${line}:${content}"$'\n'
      ;;
    internal/plugin/app.go)
      [[ "${content}" == *"req.${host_field_title}"* ]] || unexpected+="${relative}:${line}:${content}"$'\n'
      ;;
    internal/plugin/app_test.go)
      [[ "${content}" == *"${host_field_title}:"* ]] || unexpected+="${relative}:${line}:${content}"$'\n'
      ;;
    *)
      unexpected+="${relative}:${line}:${content}"$'\n'
      ;;
  esac
done <<< "${matches}"

if [[ -n "${unexpected}" ]]; then
  printf 'current model-domain terminology check failed:\n%s' "${unexpected}" >&2
  exit 1
fi

printf 'current model-domain terminology check passed\n'
