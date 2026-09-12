package policy

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"

	"cpa-key-policy/internal/policy/persist"
)

// A single atomic backup contains the exact original pair. Keeping the backup
// separate from both live files makes a failed first migration recoverable.
type migrationBackup struct {
	DatasetID string `json:"dataset_id"`
	State     []byte `json:"state"`
	Usage     []byte `json:"usage"`
}

func MigrationBackupPath(statePath string) string { return statePath + ".before-v5.json" }

func backupBeforeMigration(statePath, datasetID string) error {
	stateRaw, err := os.ReadFile(statePath)
	if err != nil {
		return err
	}
	usageRaw, err := os.ReadFile(persist.UsagePath(statePath))
	if err != nil {
		return err
	}
	state, err := decodeState(stateRaw)
	if err != nil {
		return fmt.Errorf("validate migration source state: %w", err)
	}
	usage, err := decodeUsage(usageRaw)
	if err != nil {
		return fmt.Errorf("validate migration source usage: %w", err)
	}
	if state.DatasetID != datasetID || usage.DatasetID != datasetID || state.Version >= currentStateFileVersion {
		return errors.New("migration source does not match the expected legacy dataset")
	}
	path := MigrationBackupPath(statePath)
	if raw, err := os.ReadFile(path); err == nil {
		var backup migrationBackup
		if err := decodeJSONStrict(raw, &backup); err != nil {
			return fmt.Errorf("read migration backup: %w", err)
		}
		backupState, stateErr := decodeState(backup.State)
		backupUsage, usageErr := decodeUsage(backup.Usage)
		if stateErr != nil || usageErr != nil {
			return fmt.Errorf("invalid migration backup (state: %v; usage: %v); restore a valid recovery pair", stateErr, usageErr)
		}
		if backup.DatasetID != datasetID || backupState.DatasetID != datasetID || backupUsage.DatasetID != datasetID || backupState.Version >= 5 || backupUsage.Version >= 5 {
			return errors.New("existing migration backup does not match this dataset")
		}
		if usage.Version == currentUsageFileVersion {
			if !bytes.Equal(backup.State, stateRaw) {
				return errors.New("partially migrated state differs from its recovery snapshot")
			}
			return nil
		}
		if bytes.Equal(backup.State, stateRaw) && bytes.Equal(backup.Usage, usageRaw) {
			return nil
		}
		// A rollback followed by more legacy traffic is a new migration. Keep the
		// earlier recovery point and back up the current pair before replacing it.
		archive := fmt.Sprintf("%s.%x", path, sha256.Sum256(raw))
		if existing, err := os.ReadFile(archive); err == nil {
			if !bytes.Equal(existing, raw) {
				return errors.New("migration backup archive has unexpected content")
			}
		} else if errors.Is(err, os.ErrNotExist) {
			if err := persist.AtomicWrite(archive, raw); err != nil {
				return fmt.Errorf("archive earlier migration backup: %w", err)
			}
		} else {
			return fmt.Errorf("inspect migration backup archive: %w", err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect migration backup: %w", err)
	}
	if usage.Version >= currentUsageFileVersion {
		return errors.New("partially migrated dataset is missing its original backup")
	}
	raw, err := json.MarshalIndent(migrationBackup{DatasetID: datasetID, State: stateRaw, Usage: usageRaw}, "", "  ")
	if err != nil {
		return err
	}
	if err := persist.AtomicWrite(path, raw); err != nil {
		return fmt.Errorf("save pre-migration backup: %w", err)
	}
	return nil
}

func migrateStorage(statePath, datasetID string, stateVersion, usageVersion int, cfg Config, usage map[string]*UsageState) error {
	if stateVersion == currentStateFileVersion && usageVersion == currentUsageFileVersion {
		return nil
	}
	if stateVersion == currentStateFileVersion && usageVersion < currentUsageFileVersion {
		return errors.New("v5 state is paired with legacy usage; restore the matching pair before starting")
	}
	if err := backupBeforeMigration(statePath, datasetID); err != nil {
		return err
	}
	// Persist cycle anchors first. If the state write fails, the next start reads
	// this v5 ledger and finishes migration without reapplying opening balances.
	if usageVersion < currentUsageFileVersion {
		if err := SaveUsage(persist.UsagePath(statePath), datasetID, usage); err != nil {
			return fmt.Errorf("migrate usage: %w", err)
		}
	}
	if stateVersion < currentStateFileVersion {
		if err := SaveState(statePath, datasetID, cfg.Keys, cfg.Models, cfg.ClassifyRules); err != nil {
			return fmt.Errorf("migrate state (usage is recoverable on restart): %w", err)
		}
	}
	return nil
}
