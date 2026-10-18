package policy

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"cpa-key-policy/internal/policy/persist"
)

// currentFileVersion is the data format of all three files and the only one
// the plugin reads. Older datasets are converted offline before the upgrade
// (cmd/cpa-key-policy-check --out).
const currentFileVersion = 7

type stateFile struct {
	Version   int          `json:"version"`
	DatasetID string       `json:"dataset_id"`
	Keys      []KeyConfig  `json:"keys"`
	Models    []ModelPrice `json:"models"`
	UpdatedAt time.Time    `json:"updated_at"`
}

// liveFile holds every key's quota cycles and the days newer than the
// history watermark. Keys that no longer exist in state keep their days.
type liveFile struct {
	Version        int                `json:"version"`
	DatasetID      string             `json:"dataset_id"`
	UpdatedAt      time.Time          `json:"updated_at"`
	Timezone       string             `json:"timezone"`
	HistoryThrough string             `json:"history_through"`
	Keys           map[string]liveKey `json:"keys"`
}

type liveKey struct {
	Cycles  *Cycles                      `json:"cycles,omitempty"`
	Balance *Balance                     `json:"balance,omitempty"`
	Days    map[string]map[string]Bucket `json:"days,omitempty"`
}

// historyFile holds the closed days (≤ through) inside retention.
type historyFile struct {
	Version   int                                     `json:"version"`
	DatasetID string                                  `json:"dataset_id"`
	UpdatedAt time.Time                               `json:"updated_at"`
	Through   string                                  `json:"through"`
	Days      map[string]map[string]map[string]Bucket `json:"days"`
}

type State struct {
	Version   int
	DatasetID string
	Keys      []KeyConfig
	Models    []ModelPrice
	UpdatedAt time.Time
}

// UsageDataset is the merged view of the live and history files.
type UsageDataset struct {
	DatasetID string
	Timezone  string
	// Through is the history watermark: days up to it came from the history
	// file, later days from the live file.
	Through   string
	UpdatedAt time.Time
	Keys      map[string]*KeyUsage
}

func ResolveStatePath(path string) (string, error) {
	return persist.ResolvePath(path, DefaultConfig().StateFile)
}

func NewDatasetID() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("generate dataset id: %w", err)
	}
	return hex.EncodeToString(raw[:]), nil
}

type fileHeader struct {
	Version   int    `json:"version"`
	DatasetID string `json:"dataset_id"`
}

func readHeader(raw []byte, kind string) (fileHeader, error) {
	var header fileHeader
	if err := json.Unmarshal(raw, &header); err != nil {
		return header, fmt.Errorf("decode %s header: %w", kind, err)
	}
	if strings.TrimSpace(header.DatasetID) == "" {
		return header, fmt.Errorf("%s dataset_id is required", kind)
	}
	return header, nil
}

// unsupportedVersion is the one judgement of a file version that is not the
// one a decoder expects.
func unsupportedVersion(kind string, version int) error {
	if version < currentFileVersion {
		return fmt.Errorf("unsupported %s version %d; convert the dataset offline with cpa-key-policy-check --out before starting this release", kind, version)
	}
	return fmt.Errorf("unsupported %s version %d; require version %d", kind, version, currentFileVersion)
}

func LoadState(path string) (*State, error) {
	raw, err := persist.Read(path)
	if err != nil {
		return nil, err
	}
	return decodeState(raw)
}

func decodeState(raw []byte) (*State, error) {
	header, err := readHeader(raw, "state")
	if err != nil {
		return nil, err
	}
	if header.Version != currentFileVersion {
		return nil, unsupportedVersion("state", header.Version)
	}
	var disk stateFile
	if err := decodeJSONStrict(raw, &disk); err != nil {
		return nil, fmt.Errorf("decode state: %w", err)
	}
	if err := normalizeKeys(disk.Keys); err != nil {
		return nil, fmt.Errorf("validate state: %w", err)
	}
	if _, err := normalizeModelPrices(disk.Models); err != nil {
		return nil, fmt.Errorf("validate state: %w", err)
	}
	if disk.Models == nil {
		disk.Models = []ModelPrice{}
	}
	if disk.Keys == nil {
		disk.Keys = []KeyConfig{}
	}
	return &State{Version: disk.Version, DatasetID: strings.TrimSpace(disk.DatasetID), Keys: disk.Keys, Models: disk.Models, UpdatedAt: disk.UpdatedAt}, nil
}

// LoadUsage reads the live file and, when the live file says history exists,
// the history file, and merges them: history owns every day up to its
// watermark, the live file owns the later days.
func LoadUsage(statePath string) (*UsageDataset, error) {
	liveRaw, err := persist.Read(persist.UsagePath(statePath))
	if err != nil {
		return nil, fmt.Errorf("read live usage: %w", err)
	}
	live, err := decodeLive(liveRaw)
	if err != nil {
		return nil, err
	}
	historyRaw, err := persist.Read(persist.HistoryPath(statePath))
	var history *historyFile
	switch {
	case err == nil:
		if history, err = decodeHistory(historyRaw); err != nil {
			return nil, err
		}
	case errors.Is(err, os.ErrNotExist):
		if live.HistoryThrough != "" {
			return nil, fmt.Errorf("live usage expects history through %s but %s is missing", live.HistoryThrough, persist.HistoryPath(statePath))
		}
	default:
		return nil, fmt.Errorf("read usage history: %w", err)
	}
	return mergeUsage(live, history)
}

func mergeUsage(live *liveFile, history *historyFile) (*UsageDataset, error) {
	dataset := &UsageDataset{DatasetID: live.DatasetID, Timezone: live.Timezone, UpdatedAt: live.UpdatedAt, Keys: make(map[string]*KeyUsage, len(live.Keys))}
	if history != nil {
		if history.DatasetID != live.DatasetID {
			return nil, fmt.Errorf("live/history dataset_id mismatch: live=%q history=%q", live.DatasetID, history.DatasetID)
		}
		if live.HistoryThrough > history.Through {
			return nil, fmt.Errorf("live usage was written against history through %s but the history file only reaches %s", live.HistoryThrough, history.Through)
		}
		dataset.Through = history.Through
		for id, days := range history.Days {
			dataset.Keys[id] = &KeyUsage{Days: cloneDays(days, func(string) bool { return true })}
		}
	}
	for id, key := range live.Keys {
		usage := dataset.Keys[id]
		if usage == nil {
			usage = &KeyUsage{Days: make(map[string]map[string]Bucket)}
			dataset.Keys[id] = usage
		}
		usage.Cycles, usage.Balance = key.Cycles, key.Balance
		for date, models := range key.Days {
			if date > dataset.Through {
				usage.Days[date] = models
			}
		}
	}
	return dataset, nil
}

func decodeLive(raw []byte) (*liveFile, error) {
	header, err := readHeader(raw, "usage")
	if err != nil {
		return nil, err
	}
	if header.Version != currentFileVersion {
		return nil, unsupportedVersion("usage", header.Version)
	}
	var disk liveFile
	if err := decodeJSONStrict(raw, &disk); err != nil {
		return nil, fmt.Errorf("decode live usage: %w", err)
	}
	disk.DatasetID = strings.TrimSpace(disk.DatasetID)
	location, err := time.LoadLocation(disk.Timezone)
	if err != nil || disk.Timezone == "" {
		return nil, fmt.Errorf("invalid live usage timezone %q", disk.Timezone)
	}
	if disk.HistoryThrough != "" {
		if err := validateDate(disk.HistoryThrough); err != nil {
			return nil, fmt.Errorf("live usage history_through: %w", err)
		}
	}
	if disk.Keys == nil {
		disk.Keys = make(map[string]liveKey)
	}
	for id, key := range disk.Keys {
		if strings.TrimSpace(id) == "" {
			return nil, errors.New("live usage has an empty key id")
		}
		if key.Cycles != nil {
			if err := validateCycles(key.Cycles, location); err != nil {
				return nil, fmt.Errorf("live usage key %q: %w", id, err)
			}
		}
		if key.Balance != nil {
			if err := key.Balance.validate(); err != nil {
				return nil, fmt.Errorf("live usage key %q: %w", id, err)
			}
		}
		if err := validateDays(key.Days); err != nil {
			return nil, fmt.Errorf("live usage key %q: %w", id, err)
		}
	}
	return &disk, nil
}

func decodeHistory(raw []byte) (*historyFile, error) {
	header, err := readHeader(raw, "history")
	if err != nil {
		return nil, err
	}
	if header.Version != currentFileVersion {
		return nil, unsupportedVersion("history", header.Version)
	}
	var disk historyFile
	if err := decodeJSONStrict(raw, &disk); err != nil {
		return nil, fmt.Errorf("decode usage history: %w", err)
	}
	disk.DatasetID = strings.TrimSpace(disk.DatasetID)
	if err := validateDate(disk.Through); err != nil {
		return nil, fmt.Errorf("usage history through: %w", err)
	}
	if disk.Days == nil {
		disk.Days = make(map[string]map[string]map[string]Bucket)
	}
	for id, days := range disk.Days {
		if strings.TrimSpace(id) == "" {
			return nil, errors.New("usage history has an empty key id")
		}
		if err := validateDays(days); err != nil {
			return nil, fmt.Errorf("usage history key %q: %w", id, err)
		}
		for date := range days {
			if date > disk.Through {
				return nil, fmt.Errorf("usage history key %q has day %s after through %s", id, date, disk.Through)
			}
		}
	}
	return &disk, nil
}

func validateDate(date string) error {
	if _, err := time.Parse(dateLayout, date); err != nil {
		return fmt.Errorf("invalid date %q", date)
	}
	return nil
}

func validateDays(days map[string]map[string]Bucket) error {
	for date, models := range days {
		if err := validateDate(date); err != nil {
			return err
		}
		for model, bucket := range models {
			if strings.TrimSpace(model) == "" {
				return fmt.Errorf("day %s has an empty model name", date)
			}
			if err := bucket.validate(); err != nil {
				return fmt.Errorf("day %s model %q: %w", date, model, err)
			}
		}
	}
	return nil
}

func decodeJSONStrict(raw []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		if err == nil {
			return errors.New("multiple JSON values are not allowed")
		}
		return err
	}
	return nil
}

func SaveState(path, datasetID string, keys []KeyConfig, models []ModelPrice) error {
	raw, err := MarshalState(datasetID, keys, models, time.Now().UTC())
	if err != nil {
		return err
	}
	return persist.AtomicWrite(path, raw)
}

// WriteDataset creates a complete dataset next to statePath, none of whose
// files may exist yet: days before now's accounting day go to the history
// file, the quota periods and today to the live file, and the state is
// written last. Days outside retention are dropped. Everything is validated
// as on load before the first file is written.
func WriteDataset(statePath, datasetID, timezone string, now time.Time, keys []KeyConfig, models []ModelPrice, usage map[string]*KeyUsage) error {
	location, err := time.LoadLocation(timezone)
	if err != nil || strings.TrimSpace(timezone) == "" {
		return fmt.Errorf("invalid usage timezone %q", timezone)
	}
	livePath, historyPath := persist.UsagePath(statePath), persist.HistoryPath(statePath)
	for _, path := range []string{statePath, livePath, historyPath} {
		if _, err := os.Stat(path); err == nil {
			return fmt.Errorf("%s already exists", path)
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	if _, err := MarshalState(datasetID, keys, models, now); err != nil {
		return fmt.Errorf("validate state: %w", err)
	}
	for id, key := range usage {
		if strings.TrimSpace(id) == "" || key == nil {
			return errors.New("usage has an empty key id or entry")
		}
		if key.Cycles != nil {
			if err := validateCycles(key.Cycles, location); err != nil {
				return fmt.Errorf("usage key %q: %w", id, err)
			}
		}
		if err := validateDays(key.Days); err != nil {
			return fmt.Errorf("usage key %q: %w", id, err)
		}
	}
	ledger := newUsageLedger(func() time.Time { return now }, location, timezone)
	ledger.load(usage, "")
	through := ledger.dateKeyOffset(now, -1)
	ledger.mu.Lock()
	history := ledger.historySnapshotLocked(through)
	live := ledger.liveSnapshotLocked(through)
	ledger.mu.Unlock()
	if err := writeHistory(historyPath, datasetID, through, history, now); err != nil {
		return fmt.Errorf("write usage history: %w", err)
	}
	if err := writeLive(livePath, datasetID, timezone, through, live, now); err != nil {
		return fmt.Errorf("write live usage: %w", err)
	}
	if err := SaveState(statePath, datasetID, keys, models); err != nil {
		return fmt.Errorf("write state: %w", err)
	}
	return nil
}

func MarshalState(datasetID string, keys []KeyConfig, models []ModelPrice, updatedAt time.Time) ([]byte, error) {
	datasetID = strings.TrimSpace(datasetID)
	if datasetID == "" {
		return nil, errors.New("state dataset_id is required")
	}
	keys = append([]KeyConfig{}, keys...)
	models = append([]ModelPrice{}, models...)
	if err := normalizeKeys(keys); err != nil {
		return nil, err
	}
	if _, err := normalizeModelPrices(models); err != nil {
		return nil, err
	}
	return json.MarshalIndent(stateFile{Version: currentFileVersion, DatasetID: datasetID, Keys: keys, Models: models, UpdatedAt: updatedAt.UTC()}, "", "  ")
}

// writeLive replaces the live file. The snapshot is taken against the
// watermark before the flush, so a failed history write keeps the closed
// days here; through is the watermark the history file actually reached,
// and days at or before it are trimmed. The ledger is rewritten whole on
// every flush, so the file stays compact.
func writeLive(path, datasetID, timezone, through string, keys map[string]liveKey, updatedAt time.Time) error {
	disk := liveFile{Version: currentFileVersion, DatasetID: datasetID, UpdatedAt: updatedAt.UTC(), Timezone: timezone, HistoryThrough: through, Keys: make(map[string]liveKey, len(keys))}
	for id, key := range keys {
		days := make(map[string]map[string]Bucket, len(key.Days))
		for date, models := range key.Days {
			if date > through && len(models) > 0 {
				days[date] = models
			}
		}
		if key.Cycles == nil && key.Balance == nil && len(days) == 0 {
			continue
		}
		disk.Keys[id] = liveKey{Cycles: key.Cycles, Balance: key.Balance, Days: days}
	}
	raw, err := json.Marshal(disk)
	if err != nil {
		return err
	}
	return persist.AtomicWrite(path, raw)
}

func writeHistory(path, datasetID, through string, days map[string]map[string]map[string]Bucket, updatedAt time.Time) error {
	if days == nil {
		days = make(map[string]map[string]map[string]Bucket)
	}
	raw, err := json.Marshal(historyFile{Version: currentFileVersion, DatasetID: datasetID, UpdatedAt: updatedAt.UTC(), Through: through, Days: days})
	if err != nil {
		return err
	}
	return persist.AtomicWrite(path, raw)
}
