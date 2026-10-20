package channel

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"sort"
	"strings"
	"unicode/utf8"

	"cpa-key-policy/internal/policy/persist"
)

const (
	metaFileVersion = 1
	maxTextLength   = 200
)

// metaFile is the metadata file: {"version":1,"channels":[Meta...]}.
type metaFile struct {
	Version  int    `json:"version"`
	Channels []Meta `json:"channels"`
}

func loadMeta(path string) (map[string]Meta, error) {
	metas := make(map[string]Meta)
	if path == "" {
		return metas, nil
	}
	raw, err := persist.Read(path)
	if errors.Is(err, os.ErrNotExist) {
		return metas, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read channel metadata %s: %w", path, err)
	}
	var file metaFile
	if err := json.Unmarshal(raw, &file); err != nil {
		return nil, fmt.Errorf("parse channel metadata %s: %w", path, err)
	}
	if file.Version != metaFileVersion {
		return nil, fmt.Errorf("channel metadata %s: unsupported version %d", path, file.Version)
	}
	for i, entry := range file.Channels {
		meta, err := normalizeMeta(entry)
		if err != nil {
			return nil, fmt.Errorf("channel metadata %s: entry %d: %w", path, i, err)
		}
		if _, duplicate := metas[meta.AuthIndex]; duplicate {
			return nil, fmt.Errorf("channel metadata %s: duplicate auth_index %s", path, meta.AuthIndex)
		}
		// An entry that only held the cost ratio or balance source of 0.21.0
		// and earlier records nothing now; keeping it would pin the channel
		// to the list after its statistics expire.
		if meta.blank() {
			continue
		}
		metas[meta.AuthIndex] = meta
	}
	return metas, nil
}

// normalizeMeta trims and validates every field.
func normalizeMeta(m Meta) (Meta, error) {
	index, err := normalizeAuthIndex(m.AuthIndex)
	if err != nil {
		return Meta{}, err
	}
	m.AuthIndex = index
	for _, field := range []struct {
		name  string
		value *string
	}{{"display_name", &m.DisplayName}, {"vendor", &m.Vendor}, {"note", &m.Note}} {
		*field.value = strings.TrimSpace(*field.value)
		if utf8.RuneCountInString(*field.value) > maxTextLength {
			return Meta{}, invalidf("%s must be at most %d characters", field.name, maxTextLength)
		}
	}
	return m, nil
}

// blank reports whether nothing beyond the auth_index is recorded.
func (m Meta) blank() bool {
	return m.DisplayName == "" && m.Vendor == "" && m.Note == ""
}

// UpdateMeta applies a patch, validates it (auth_index 16 hex characters in
// any case, text fields trimmed and at most 200 characters) and persists the
// metadata.
// Invalid input wraps ErrInvalid. While CanWrite reports false it returns
// ErrReadOnly; a failed write wraps ErrPersist. Either way the metadata in
// memory is unchanged. A patch that leaves every field empty removes the
// channel's entry.
func (r *Registry) UpdateMeta(p MetaPatch) (Meta, error) {
	index, err := normalizeAuthIndex(p.AuthIndex)
	if err != nil {
		return Meta{}, err
	}
	r.metaWriteMu.Lock()
	defer r.metaWriteMu.Unlock()
	current, _ := r.metaFor(index)
	next := current
	next.AuthIndex = index
	if p.DisplayName != nil {
		next.DisplayName = *p.DisplayName
	}
	if p.Vendor != nil {
		next.Vendor = *p.Vendor
	}
	if p.Note != nil {
		next.Note = *p.Note
	}
	if next, err = normalizeMeta(next); err != nil {
		return Meta{}, err
	}
	if !r.canWrite() {
		return Meta{}, ErrReadOnly
	}
	next.UpdatedAt = r.now().UTC()

	r.metaMu.RLock()
	metas := maps.Clone(r.metas)
	r.metaMu.RUnlock()
	if next.blank() {
		delete(metas, index)
	} else {
		metas[index] = next
	}
	if err := r.writeMeta(metas); err != nil {
		return Meta{}, err
	}
	r.metaMu.Lock()
	r.metas = metas
	r.metaMu.Unlock()
	return next, nil
}

func (r *Registry) writeMeta(metas map[string]Meta) error {
	if r.metaPath == "" {
		return fmt.Errorf("%w: no metadata file is configured", ErrPersist)
	}
	file := metaFile{Version: metaFileVersion, Channels: make([]Meta, 0, len(metas))}
	for _, meta := range metas {
		file.Channels = append(file.Channels, meta)
	}
	sort.Slice(file.Channels, func(i, j int) bool { return file.Channels[i].AuthIndex < file.Channels[j].AuthIndex })
	raw, err := json.MarshalIndent(file, "", "  ")
	if err != nil {
		return fmt.Errorf("%w: encode: %w", ErrPersist, err)
	}
	if err := persist.AtomicWrite(r.metaPath, raw); err != nil {
		return fmt.Errorf("%w: %w", ErrPersist, err)
	}
	return nil
}

func (r *Registry) metaFor(index string) (Meta, bool) {
	r.metaMu.RLock()
	defer r.metaMu.RUnlock()
	meta, ok := r.metas[index]
	return meta, ok
}

// metaSnapshot returns the current metadata map; it is never modified in
// place, so the caller may read it without locks.
func (r *Registry) metaSnapshot() map[string]Meta {
	r.metaMu.RLock()
	defer r.metaMu.RUnlock()
	return r.metas
}
