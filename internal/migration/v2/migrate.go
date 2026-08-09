package v2

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"cpa-key-policy/internal/policy"
	"cpa-key-policy/internal/policy/persist"
)

type Options struct {
	StatePath  string
	UsagePath  string
	AuditPath  string
	BackupDir  string
	Timezone   string
	FreeModels []string
	DryRun     bool
}

type fileOperations struct {
	rename             func(string, string) error
	remove             func(string) error
	atomicWrite        func(string, []byte) error
	writeBackup        func(string, map[string][]byte) error
	writeValidatedTemp func(string, []byte, bool, string) (string, error)
	validatePair       func(string, string, string) error
}

var defaultFileOperations = fileOperations{
	rename:             os.Rename,
	remove:             os.Remove,
	atomicWrite:        persist.AtomicWrite,
	writeBackup:        writeBackup,
	writeValidatedTemp: writeValidatedTemp,
	validatePair:       validateFormalPair,
}

func Run(options Options, stdout io.Writer, now func() time.Time) error {
	return run(options, stdout, now, defaultFileOperations)
}

func run(options Options, stdout io.Writer, now func() time.Time, operations fileOperations) error {
	if now == nil {
		now = time.Now
	}
	if stdout == nil {
		stdout = io.Discard
	}
	var err error
	options.StatePath, err = requiredAbsolutePath(options.StatePath, "state")
	if err != nil {
		return err
	}
	options.UsagePath, err = requiredAbsolutePath(options.UsagePath, "usage")
	if err != nil {
		return err
	}
	options.BackupDir, err = requiredAbsolutePath(options.BackupDir, "backup-dir")
	if err != nil {
		return err
	}
	if options.AuditPath == "" {
		options.AuditPath = persist.AuditPath(options.StatePath)
	} else if options.AuditPath, err = requiredAbsolutePath(options.AuditPath, "audit"); err != nil {
		return err
	}
	if samePath(options.StatePath, options.UsagePath) {
		return errors.New("state and usage paths must be different")
	}
	location, err := time.LoadLocation(strings.TrimSpace(options.Timezone))
	if err != nil {
		return fmt.Errorf("load timezone %q: %w", options.Timezone, err)
	}
	stateRaw, err := os.ReadFile(options.StatePath)
	if err != nil {
		return fmt.Errorf("read state: %w", err)
	}
	usageRaw, err := os.ReadFile(options.UsagePath)
	if err != nil {
		return fmt.Errorf("read usage: %w", err)
	}
	migrationTime := now().UTC()
	conversion, err := Convert(stateRaw, usageRaw, options.FreeModels, migrationTime, location, options.Timezone, options.BackupDir)
	if err != nil {
		return err
	}
	reportRaw, err := marshalReport(conversion.Report)
	if err != nil {
		return err
	}
	if options.DryRun || conversion.AlreadyV3 {
		_, err = stdout.Write(reportRaw)
		return err
	}
	if _, err := stdout.Write(reportRaw); err != nil {
		return fmt.Errorf("write migration report: %w", err)
	}
	if err := prepareBackupDir(options.BackupDir); err != nil {
		return err
	}
	auditPaths, err := existingAuditPaths(options.AuditPath)
	if err != nil {
		return err
	}
	originals := map[string][]byte{options.StatePath: stateRaw, options.UsagePath: usageRaw}
	for _, path := range auditPaths {
		raw, readErr := os.ReadFile(path)
		if readErr != nil {
			return fmt.Errorf("read audit file %q: %w", path, readErr)
		}
		originals[path] = raw
	}
	if err := operations.writeBackup(options.BackupDir, originals); err != nil {
		return err
	}
	stateV3, err := policy.MarshalState(conversion.DatasetID, conversion.Keys, conversion.Models, conversion.Rules, migrationTime)
	if err != nil {
		return err
	}
	usageV3, err := policy.MarshalUsage(conversion.DatasetID, conversion.Usage, migrationTime)
	if err != nil {
		return err
	}
	stateTemp, err := operations.writeValidatedTemp(options.StatePath, stateV3, true, conversion.DatasetID)
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(stateTemp) }()
	usageTemp, err := operations.writeValidatedTemp(options.UsagePath, usageV3, false, conversion.DatasetID)
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(usageTemp) }()

	if err := operations.rename(stateTemp, options.StatePath); err != nil {
		return fmt.Errorf("replace state: %w", err)
	}
	if err := operations.rename(usageTemp, options.UsagePath); err != nil {
		restoreErr := restoreOriginals(originals, operations)
		return errors.Join(fmt.Errorf("replace usage: %w", err), restoreErr)
	}
	if err := syncDirectory(filepath.Dir(options.StatePath)); err != nil {
		restoreErr := restoreOriginals(originals, operations)
		return errors.Join(fmt.Errorf("sync state directory: %w", err), restoreErr)
	}
	if filepath.Dir(options.UsagePath) != filepath.Dir(options.StatePath) {
		if err := syncDirectory(filepath.Dir(options.UsagePath)); err != nil {
			restoreErr := restoreOriginals(originals, operations)
			return errors.Join(fmt.Errorf("sync usage directory: %w", err), restoreErr)
		}
	}
	if err := operations.validatePair(options.StatePath, options.UsagePath, conversion.DatasetID); err != nil {
		restoreErr := restoreOriginals(originals, operations)
		return errors.Join(fmt.Errorf("validate replaced state/usage pair: %w", err), restoreErr)
	}
	for _, path := range auditPaths {
		if err := operations.remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			restoreErr := restoreOriginals(originals, operations)
			return errors.Join(fmt.Errorf("archive active v2 audit %q: %w", path, err), restoreErr)
		}
	}
	if len(auditPaths) > 0 {
		if err := syncDirectory(filepath.Dir(options.AuditPath)); err != nil {
			restoreErr := restoreOriginals(originals, operations)
			return errors.Join(fmt.Errorf("sync audit directory: %w", err), restoreErr)
		}
	}
	return nil
}

func validateFormalPair(statePath, usagePath, datasetID string) error {
	state, err := policy.LoadState(statePath)
	if err != nil {
		return fmt.Errorf("load replaced state: %w", err)
	}
	usage, err := policy.LoadUsage(usagePath)
	if err != nil {
		return fmt.Errorf("load replaced usage: %w", err)
	}
	if state.DatasetID != datasetID || usage.DatasetID != datasetID || state.DatasetID != usage.DatasetID {
		return fmt.Errorf("replaced dataset_id mismatch: state=%q usage=%q expected=%q", state.DatasetID, usage.DatasetID, datasetID)
	}
	return nil
}

func requiredAbsolutePath(path, name string) (string, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return "", fmt.Errorf("-%s is required", name)
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	return filepath.Clean(absolute), nil
}

func samePath(left, right string) bool {
	leftInfo, leftErr := os.Stat(left)
	rightInfo, rightErr := os.Stat(right)
	if leftErr == nil && rightErr == nil {
		return os.SameFile(leftInfo, rightInfo)
	}
	return filepath.Clean(left) == filepath.Clean(right)
}

func prepareBackupDir(path string) error {
	entries, err := os.ReadDir(path)
	switch {
	case err == nil:
		if len(entries) != 0 {
			return fmt.Errorf("backup directory %q must be empty", path)
		}
		return nil
	case errors.Is(err, os.ErrNotExist):
		if err := os.MkdirAll(path, 0o700); err != nil {
			return fmt.Errorf("create backup directory: %w", err)
		}
		return nil
	default:
		return fmt.Errorf("inspect backup directory: %w", err)
	}
}

func existingAuditPaths(auditPath string) ([]string, error) {
	paths := make([]string, 0, 4)
	if _, err := os.Stat(auditPath); err == nil {
		paths = append(paths, auditPath)
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	entries, err := os.ReadDir(filepath.Dir(auditPath))
	if errors.Is(err, os.ErrNotExist) {
		return paths, nil
	}
	if err != nil {
		return nil, err
	}
	prefix := filepath.Base(auditPath) + "."
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasPrefix(entry.Name(), prefix) {
			continue
		}
		if _, err := strconv.Atoi(strings.TrimPrefix(entry.Name(), prefix)); err == nil {
			paths = append(paths, filepath.Join(filepath.Dir(auditPath), entry.Name()))
		}
	}
	sort.Strings(paths)
	return paths, nil
}

func writeBackup(backupDir string, originals map[string][]byte) error {
	paths := make([]string, 0, len(originals))
	for path := range originals {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	checksums := make([]string, 0, len(paths))
	usedNames := make(map[string]struct{}, len(paths))
	for _, path := range paths {
		name := filepath.Base(path)
		if _, duplicate := usedNames[name]; duplicate {
			return fmt.Errorf("backup filename collision for %q", name)
		}
		usedNames[name] = struct{}{}
		destination := filepath.Join(backupDir, name)
		file, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			return fmt.Errorf("create backup %q: %w", destination, err)
		}
		if _, err := file.Write(originals[path]); err != nil {
			_ = file.Close()
			return fmt.Errorf("write backup %q: %w", destination, err)
		}
		if err := file.Sync(); err != nil {
			_ = file.Close()
			return fmt.Errorf("sync backup %q: %w", destination, err)
		}
		if err := file.Close(); err != nil {
			return err
		}
		checksums = append(checksums, sha256Hex(originals[path])+"  "+name)
	}
	checksumRaw := []byte(strings.Join(checksums, "\n") + "\n")
	if err := persist.AtomicWrite(filepath.Join(backupDir, "SHA256SUMS"), checksumRaw); err != nil {
		return fmt.Errorf("write backup checksums: %w", err)
	}
	return syncDirectory(backupDir)
}

func writeValidatedTemp(target string, raw []byte, state bool, datasetID string) (string, error) {
	file, err := os.CreateTemp(filepath.Dir(target), "."+filepath.Base(target)+".migrate-v3-*")
	if err != nil {
		return "", err
	}
	path := file.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(path)
		}
	}()
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		return "", err
	}
	if _, err := file.Write(raw); err != nil {
		_ = file.Close()
		return "", err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return "", err
	}
	if err := file.Close(); err != nil {
		return "", err
	}
	if state {
		loaded, err := policy.LoadState(path)
		if err != nil {
			return "", fmt.Errorf("reread staged state: %w", err)
		}
		if loaded.DatasetID != datasetID {
			return "", errors.New("staged state dataset_id changed")
		}
	} else {
		loaded, err := policy.LoadUsage(path)
		if err != nil {
			return "", fmt.Errorf("reread staged usage: %w", err)
		}
		if loaded.DatasetID != datasetID {
			return "", errors.New("staged usage dataset_id changed")
		}
	}
	cleanup = false
	return path, nil
}

func restoreOriginals(originals map[string][]byte, operations fileOperations) error {
	paths := make([]string, 0, len(originals))
	for path := range originals {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	var restoreErr error
	for _, path := range paths {
		if err := operations.atomicWrite(path, originals[path]); err != nil {
			restoreErr = errors.Join(restoreErr, fmt.Errorf("restore %q: %w", path, err))
		}
	}
	return restoreErr
}

func syncDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}

func marshalReport(report Report) ([]byte, error) {
	raw, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(raw, '\n'), nil
}
