#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
legacy_word="$(printf '\141\154\151\141\163')"
legacy_field="$(printf '\101\154\151\141\163')"
reset_word="$(printf '\162\145\163\145\164')"
legacy_model_rule="$(printf '\115\157\144\145\154\122\165\154\145')"
legacy_symbols="${legacy_field}(Mapping|Target)|Key${legacy_field}Ref|By${legacy_field}|${legacy_model_rule}"
pattern="\\b${legacy_word}(es)?\\b|${legacy_symbols}|by_${legacy_word}|daily_${reset_word}_at|weekly_${reset_word}_at"

matches="$({
  rg -n -i \
    --glob '!.git/**' \
    --glob '!goals/**' \
    --glob '!memories/**' \
    --glob '!web/node_modules/**' \
    --glob '!web/*.tsbuildinfo' \
    "${pattern}" "${repo_root}" || true
})"

unexpected=""
while IFS=: read -r file line content; do
  [[ -n "${file}" ]] || continue
  relative="${file#"${repo_root}/"}"
  case "${relative}" in
    internal/migration/v2/*|docs/migrate-v2-to-v3.md|internal/plugin/types.go)
      ;;
    internal/plugin/app.go)
      [[ "${content}" == *"req.${legacy_field}"* ]] || unexpected+="${relative}:${line}:${content}"$'\n'
      ;;
    internal/plugin/app_test.go)
      [[ "${content}" == *"${legacy_field}:"* ]] || unexpected+="${relative}:${line}:${content}"$'\n'
      ;;
    *)
      unexpected+="${relative}:${line}:${content}"$'\n'
      ;;
  esac
done <<< "${matches}"

if [[ -n "${unexpected}" ]]; then
  printf 'v3 model-domain terminology check failed:\n%s' "${unexpected}" >&2
  exit 1
fi

printf 'v3 model-domain terminology check passed\n'
