package persist

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const staleTempAge = time.Hour

// ResolvePath resolves a configured path without imposing policy-domain
// defaults on the file layer.
func ResolvePath(path, defaultPath string) (string, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		path = defaultPath
	}
	if filepath.IsAbs(path) {
		return filepath.Clean(path), nil
	}
	return filepath.Abs(path)
}

// UsagePath derives the independent usage-ledger path from the state path.
func UsagePath(statePath string) string {
	return filepath.Join(filepath.Dir(statePath), "cpa-key-policy-usage.json")
}

// AuditPath derives the append-only audit path from the state path.
func AuditPath(statePath string) string {
	return filepath.Join(filepath.Dir(statePath), "cpa-key-policy-audit.jsonl")
}

// Read returns the complete contents of path.
func Read(path string) ([]byte, error) {
	return os.ReadFile(path)
}

// AtomicWrite replaces path only after the complete payload has been written
// and synced to a same-directory temporary file.
func AtomicWrite(path string, raw []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	temp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tempName := temp.Name()
	defer func() { _ = os.Remove(tempName) }()
	if err := temp.Chmod(0o600); err != nil {
		_ = temp.Close()
		return err
	}
	if _, err := temp.Write(raw); err != nil {
		_ = temp.Close()
		return err
	}
	if err := temp.Sync(); err != nil {
		_ = temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	return os.Rename(tempName, path)
}

// CleanupStaleTemps removes interrupted atomic-write files for targetPath
// whose modification time is older than one hour.
func CleanupStaleTemps(targetPath string, now time.Time) error {
	dir := filepath.Dir(targetPath)
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	prefix := "." + filepath.Base(targetPath) + ".tmp-"
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasPrefix(entry.Name(), prefix) {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return fmt.Errorf("stat stale temp %q: %w", entry.Name(), err)
		}
		if now.Sub(info.ModTime()) <= staleTempAge {
			continue
		}
		if err := os.Remove(filepath.Join(dir, entry.Name())); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove stale temp %q: %w", entry.Name(), err)
		}
	}
	return nil
}
