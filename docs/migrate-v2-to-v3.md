# Offline migration from v2 to v3

This procedure is destructive and must run while the target CPA instance is stopped. Migrate one instance at a time. Do not reuse a rollback directory between instances.

The input is the paired v2 state and usage files. The tool converts global aliases to v3 model definitions, Key alias references to model references, and usage `by_alias` buckets to `by_model`. These old names exist only in this migration boundary; the v3 runtime will reject them.

## 1. Build the Linux/amd64 tool

```bash
make check-migrator-linux-amd64
```

Record the binary SHA-256 beside the change ticket.

## 2. Inventory the instance

Before stopping CPA, record:

- absolute state, usage, and audit paths;
- plugin binary path, SHA-256, and version;
- state/usage versions, sizes, modes, owners, and SHA-256 values;
- Key count, alias/model count, reference count, date-bucket count, and current daily/7-day/30-day totals;
- every zero-active-price model that is intentionally free.

Do not infer free status from zero prices. Each intentional free model must be explicitly passed with a repeated `-free-model` flag. Create one parent rollback directory per instance and copy the currently installed plugin plus its SHA into a `plugin/` subdirectory. Reserve an absent or empty `data/` subdirectory for the migration tool.

## 3. Stop writes and dry-run

Stop CPA and confirm no process has the state, usage, or audit files open for writing. Then run:

```bash
./migrate-model-schema_linux_amd64 \
  -state /absolute/path/cpa-key-policy-state.json \
  -usage /absolute/path/cpa-key-policy-usage.json \
  -audit /absolute/path/cpa-key-policy-audit.jsonl \
  -backup-dir /absolute/path/rollback-instance-a/data \
  -timezone Asia/Shanghai \
  -free-model community \
  -dry-run > instance-a-dry-run.json
```

Dry-run does not create the backup directory and does not modify input files. Run it twice and require byte-identical reports. Review:

- `status=ready`;
- input file SHA-256 values;
- `dataset_id`;
- Key/model/reference/date-bucket counts;
- per-Key `before` and `after` daily/7-day/30-day totals;
- aggregate `summary.before` and `summary.after` totals.

All before/after values must be equal, and every Key must satisfy daily ≤ 7-day ≤ 30-day.

## 4. Apply

Use the reserved new or empty data rollback directory:

```bash
./migrate-model-schema_linux_amd64 \
  -state /absolute/path/cpa-key-policy-state.json \
  -usage /absolute/path/cpa-key-policy-usage.json \
  -audit /absolute/path/cpa-key-policy-audit.jsonl \
  -backup-dir /absolute/path/rollback-instance-a/data \
  -timezone Asia/Shanghai \
  -free-model community > instance-a-applied.json
```

The tool writes owner-only backups plus `SHA256SUMS`, stages and strictly rereads both v3 files, then replaces the pair. If replacement or audit archival fails, it restores the captured originals. Active v2 audit files are retained in the rollback package and removed from the active path so v3 starts a fresh audit log.

Verify the rollback package before starting CPA:

```bash
cd /absolute/path/rollback-instance-a/data
sha256sum -c SHA256SUMS
```

## 5. Validate and start

Run the migrator once more against the new files with a different unused backup path. It must report `status=already_migrated`, must not create that backup path, and must not change either file hash.

Then start CPA and verify:

- plugin registration succeeds with version 0.5.x and unchanged ABI/schema versions;
- `/status` reports the expected `dataset_id`, Key count, and model count;
- `/models`, `/keys`, `/keys/usage`, and `/keys/history` use v3 fields;
- one allowed request routes to the expected provider/model/group;
- one disallowed model, RPM-limited request, and USD-limited request are rejected as expected;
- state and usage hashes remain a matched v3 pair after a controlled usage write;
- no version, dataset, unknown-model, missing-price, or ledger-reconciliation errors appear in logs.

Observe the instance before proceeding to the second instance.

## Rollback

Stop CPA again. Verify `SHA256SUMS`, restore the state, usage, and audit files from the rollback directory with their recorded modes and ownership, reinstall the previous plugin binary, and start CPA. Confirm the old plugin reads the restored v2 pair before reopening traffic.

Keep both complete rollback packages, including their `data/` backups and previous plugin binaries, after successful migration. Deletion is a separate, explicitly approved operation.
