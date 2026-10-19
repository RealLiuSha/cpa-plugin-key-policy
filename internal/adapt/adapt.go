// Package adapt rewrites the request body of one upstream attempt for the
// channel (CPA credential) the attempt was routed to.
//
// CPA calls request.intercept_after once per credential attempt, after it has
// picked the credential and before its executor translates the request, so a
// rule here sees the client-format body and its change applies to that
// attempt only. Rules are code-defined types with fixed parameters: there is
// no scripting. A channel is identified by CPA's auth_index (16 lowercase hex
// characters), which survives proxy, prefix and header edits of the
// credential.
package adapt

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"
	"unicode/utf8"

	"cpa-key-policy/internal/policy/persist"
)

// Rule types.
const (
	// TypeEditsImageField moves the input images of a JSON image edit from
	// images[].image_url to the image field, which some new-api based
	// upstreams require: one image becomes a string, several an array, and
	// images is removed. Bodies that already carry image are left alone.
	TypeEditsImageField = "edits_image_field"
	// TypeSetField sets a top-level JSON field. Without Overwrite it only
	// fills a missing field.
	TypeSetField = "set_field"
	// TypeDeleteField removes a top-level JSON field.
	TypeDeleteField = "delete_field"
)

// Limits of one rule and of the rule list.
const (
	maxRules      = 200
	maxPaths      = 8
	maxPathChars  = 128
	maxFieldChars = 64
	maxValueBytes = 4096
	maxNoteChars  = 200
	fileVersion   = 1
)

var (
	// ErrNotFound reports an unknown rule id.
	ErrNotFound = errors.New("adapter rule not found")
	// ErrInvalidRule wraps every validation failure of a rule or of the rule
	// list; the wrapping message says what to fix.
	ErrInvalidRule = errors.New("invalid adapter rule")
	// ErrReadOnly reports a change refused because this plugin instance may
	// not write the rule file (it no longer owns the data directory).
	ErrReadOnly = errors.New("adapter rules are read-only in this plugin instance")
)

// Rule is one rewrite attached to one channel.
type Rule struct {
	ID      string `json:"id"`
	Channel string `json:"channel"`
	Type    string `json:"type"`
	// Paths are exact CPA request paths (the request_path metadata, such as
	// /v1/images/edits). Empty means the type's default paths.
	Paths []string `json:"paths,omitempty"`
	// Field is the top-level key for set_field and delete_field. A dot is
	// part of the key, not a path into nested objects.
	Field string `json:"field,omitempty"`
	// Value is the JSON value set_field writes. JSON null counts as no
	// value.
	Value     json.RawMessage `json:"value,omitempty"`
	Overwrite bool            `json:"overwrite,omitempty"`
	Enabled   bool            `json:"enabled"`
	Note      string          `json:"note,omitempty"`
	UpdatedAt time.Time       `json:"updated_at"`
}

// RuleType describes a type for the management UI.
type RuleType struct {
	Type         string   `json:"type"`
	DefaultPaths []string `json:"default_paths"`
	NeedsField   bool     `json:"needs_field"`
	NeedsValue   bool     `json:"needs_value"`
}

// Hit counts the attempts a rule changed since the plugin started.
type Hit struct {
	Count  int64     `json:"count"`
	LastAt time.Time `json:"last_at,omitzero"`
}

var ruleTypes = []RuleType{
	{Type: TypeEditsImageField, DefaultPaths: []string{"/v1/images/edits"}},
	{Type: TypeSetField, DefaultPaths: []string{}, NeedsField: true, NeedsValue: true},
	{Type: TypeDeleteField, DefaultPaths: []string{}, NeedsField: true},
}

// Types lists the supported rule types in display order.
func Types() []RuleType {
	types := slices.Clone(ruleTypes)
	for i := range types {
		types[i].DefaultPaths = slices.Clone(types[i].DefaultPaths)
	}
	return types
}

func lookupType(name string) (RuleType, bool) {
	for _, ruleType := range ruleTypes {
		if ruleType.Type == name {
			return ruleType, true
		}
	}
	return RuleType{}, false
}

// action is what a rule does to a body, apart from where it applies.
type action struct {
	kind      string
	field     string
	value     json.RawMessage
	overwrite bool
}

// newAction validates the type, field, value and overwrite flag of rule and
// returns them normalized: trimmed field, compact value.
func newAction(rule Rule) (action, error) {
	kind := strings.TrimSpace(rule.Type)
	spec, ok := lookupType(kind)
	if !ok {
		return action{}, invalidf("unknown type; use %s, %s or %s", TypeEditsImageField, TypeSetField, TypeDeleteField)
	}
	field := strings.TrimSpace(rule.Field)
	switch {
	case spec.NeedsField && !isField(field):
		return action{}, invalidf("%s needs a field of 1-%d letters, digits, '_', '.' or '-'", kind, maxFieldChars)
	case !spec.NeedsField && field != "":
		return action{}, invalidf("%s takes no field", kind)
	}
	value := bytes.TrimSpace(rule.Value)
	if string(value) == "null" {
		value = nil
	}
	var compact bytes.Buffer
	switch {
	case spec.NeedsValue && len(value) == 0:
		return action{}, invalidf("%s needs a value", kind)
	case spec.NeedsValue:
		if !utf8.Valid(value) {
			return action{}, invalidf("value must be UTF-8 JSON")
		}
		if err := json.Compact(&compact, value); err != nil {
			return action{}, invalidf("value must be valid JSON: %v", err)
		}
		if compact.Len() > maxValueBytes {
			return action{}, invalidf("value must be at most %d bytes", maxValueBytes)
		}
	case len(value) > 0:
		return action{}, invalidf("%s takes no value", kind)
	}
	act := action{kind: kind, field: field, overwrite: rule.Overwrite && kind == TypeSetField}
	if compact.Len() > 0 {
		act.value = compact.Bytes()
	}
	return act, nil
}

// apply rewrites doc in place and reports whether anything changed.
func (a action) apply(doc map[string]json.RawMessage) bool {
	switch a.kind {
	case TypeEditsImageField:
		return moveEditImages(doc)
	case TypeSetField:
		if current, ok := doc[a.field]; ok && (!a.overwrite || bytes.Equal(current, a.value)) {
			return false
		}
		doc[a.field] = a.value
		return true
	case TypeDeleteField:
		if _, ok := doc[a.field]; !ok {
			return false
		}
		delete(doc, a.field)
		return true
	}
	return false
}

// moveEditImages runs edits_image_field: the URLs of images[] go to image,
// a single one as a string and several as an array, and images is removed.
// The URL tokens move as raw JSON strings and are never decoded, because
// they are usually multi-megabyte data URLs.
func moveEditImages(doc map[string]json.RawMessage) bool {
	if _, ok := doc["image"]; ok {
		return false
	}
	images := doc["images"]
	if len(images) == 0 || images[0] != '[' {
		return false
	}
	var urls [][]byte
	eachElement(images, func(element []byte) {
		if url := imageURL(element); url != nil {
			urls = append(urls, url)
		}
	})
	switch len(urls) {
	case 0:
		return false
	case 1:
		doc["image"] = urls[0]
	default:
		doc["image"] = joinArray(urls)
	}
	delete(doc, "images")
	return true
}

// imageURL returns the raw JSON string token holding the URL of one images[]
// element: the element itself when it is a string; for an object its
// image_url (a string, or an object with url), otherwise its url. Elements
// without a non-empty URL return nil.
func imageURL(element []byte) []byte {
	if isString(element) {
		return nonEmptyString(element)
	}
	if !isObject(element) {
		return nil
	}
	var primary, fallback []byte
	if !eachMember(element, func(key string, value []byte) {
		switch key {
		case "image_url":
			primary = value
		case "url":
			fallback = value
		}
	}) {
		return nil
	}
	if isObject(primary) {
		nested := primary
		primary = nil
		if !eachMember(nested, func(key string, value []byte) {
			if key == "url" {
				primary = value
			}
		}) {
			return nil
		}
	}
	if found := nonEmptyString(primary); found != nil {
		return found
	}
	return nonEmptyString(fallback)
}

func isString(raw []byte) bool { return len(raw) > 0 && raw[0] == '"' }

func isObject(raw []byte) bool { return len(raw) > 0 && raw[0] == '{' }

// nonEmptyString returns raw when it is a JSON string token other than "".
func nonEmptyString(raw []byte) []byte {
	if len(raw) > 2 && raw[0] == '"' {
		return raw
	}
	return nil
}

func joinArray(values [][]byte) []byte {
	size := len(values) + 1
	for _, value := range values {
		size += len(value)
	}
	out := make([]byte, 0, size)
	out = append(out, '[')
	for i, value := range values {
		if i > 0 {
			out = append(out, ',')
		}
		out = append(out, value...)
	}
	return append(out, ']')
}

// Book is an immutable index of the enabled rules by channel, safe for
// concurrent use by the request path. A nil or zero Book has no rules.
type Book struct {
	channels map[string][]bookRule
}

type bookRule struct {
	id    string
	paths []string
	act   action
}

func newBook(rules []Rule) *Book {
	book := &Book{channels: make(map[string][]bookRule)}
	for _, rule := range rules {
		if !rule.Enabled {
			continue
		}
		book.channels[rule.Channel] = append(book.channels[rule.Channel], bookRule{
			id:    rule.ID,
			paths: rule.Paths,
			act:   action{kind: rule.Type, field: rule.Field, value: rule.Value, overwrite: rule.Overwrite},
		})
	}
	return book
}

// Match reports, without allocating, whether any enabled rule of channel
// applies to path. The request path calls it before decoding the body.
func (b *Book) Match(channel, path string) bool {
	if b == nil {
		return false
	}
	rules := b.channels[channel]
	for i := range rules {
		if slices.Contains(rules[i].paths, path) {
			return true
		}
	}
	return false
}

// Apply runs the matching enabled rules of channel, in rule order, on a
// client-format JSON body. out is nil when no rule changed the body; applied
// lists the ids of the rules that did. A body that is not a JSON object is
// returned unchanged with a nil error. A rewritten body has its top-level
// keys sorted; every value no rule touched keeps its exact bytes.
func (b *Book) Apply(channel, path string, body []byte) (out []byte, applied []string, err error) {
	if !b.Match(channel, path) {
		return nil, nil, nil
	}
	doc, ok := decodeObject(body)
	if !ok {
		return nil, nil, nil
	}
	rules := b.channels[channel]
	for i := range rules {
		if slices.Contains(rules[i].paths, path) && rules[i].act.apply(doc) {
			applied = append(applied, rules[i].id)
		}
	}
	if len(applied) == 0 {
		return nil, nil, nil
	}
	return encodeObject(doc), applied, nil
}

// Preview runs one rule, enabled or not, on a sample body for the
// management UI. It ignores the rule's channel and paths. out is the
// rewritten body when changed is true and body itself otherwise; err wraps
// ErrInvalidRule when the rule's type, field or value is invalid.
func Preview(rule Rule, body []byte) (out []byte, changed bool, err error) {
	act, err := newAction(rule)
	if err != nil {
		return nil, false, err
	}
	doc, ok := decodeObject(body)
	if !ok || !act.apply(doc) {
		return body, false, nil
	}
	return encodeObject(doc), true, nil
}

// decodeObject indexes the members of a JSON object body by key. Values are
// the members' raw bytes inside body, not copies: bodies of image edits are
// tens of megabytes. As with encoding/json, a repeated key keeps its last
// value. ok is false when body is not exactly one valid JSON object.
func decodeObject(body []byte) (doc map[string]json.RawMessage, ok bool) {
	start := skipSpace(body, 0)
	if start == len(body) || body[start] != '{' || !json.Valid(body) {
		return nil, false
	}
	doc = make(map[string]json.RawMessage)
	if !eachMember(body[start:], func(key string, value []byte) { doc[key] = value }) {
		return nil, false
	}
	return doc, true
}

// encodeObject writes doc as a JSON object with sorted keys, copying every
// value's bytes unchanged.
func encodeObject(doc map[string]json.RawMessage) []byte {
	keys := slices.Sorted(maps.Keys(doc))
	size := 2
	for _, key := range keys {
		size += len(key) + len(doc[key]) + 4
	}
	out := make([]byte, 0, size)
	out = append(out, '{')
	for i, key := range keys {
		if i > 0 {
			out = append(out, ',')
		}
		out = appendString(out, key)
		out = append(out, ':')
		out = append(out, doc[key]...)
	}
	return append(out, '}')
}

// appendString appends s as a JSON string, escaping only what JSON requires.
func appendString(dst []byte, s string) []byte {
	const digits = "0123456789abcdef"
	dst = append(dst, '"')
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case c == '"' || c == '\\':
			dst = append(dst, '\\', c)
		case c < 0x20:
			dst = append(dst, '\\', 'u', '0', '0', digits[c>>4], digits[c&0xf])
		default:
			dst = append(dst, c)
		}
	}
	return append(dst, '"')
}

// The walker below reads JSON that json.Valid has already accepted, so it
// only tracks where strings and nested values end. On any other input it
// still stays in bounds and terminates; it just yields meaningless slices.

// eachMember calls fn with every key and raw value of the JSON object at the
// start of obj, in order. It reports false when a member cannot be read.
func eachMember(obj []byte, fn func(key string, value []byte)) bool {
	i := skipSpace(obj, 1)
	for i < len(obj) && obj[i] == '"' {
		keyEnd := stringEnd(obj, i)
		key, ok := unquoteKey(obj[i:keyEnd])
		if !ok {
			return false
		}
		i = skipSpace(obj, keyEnd)
		if i == len(obj) || obj[i] != ':' {
			return false
		}
		i = skipSpace(obj, i+1)
		end := valueEnd(obj, i)
		if end == i {
			return false
		}
		fn(key, obj[i:end])
		i = skipSpace(obj, end)
		if i < len(obj) && obj[i] == ',' {
			i = skipSpace(obj, i+1)
		}
	}
	return true
}

// eachElement calls fn with every raw element of the JSON array at the
// start of arr, in order.
func eachElement(arr []byte, fn func(value []byte)) {
	i := skipSpace(arr, 1)
	for i < len(arr) && arr[i] != ']' {
		end := valueEnd(arr, i)
		if end == i {
			return
		}
		fn(arr[i:end])
		i = skipSpace(arr, end)
		if i < len(arr) && arr[i] == ',' {
			i = skipSpace(arr, i+1)
		}
	}
}

// unquoteKey decodes an object key token. Keys without escapes, which is
// nearly all of them, skip encoding/json.
func unquoteKey(raw []byte) (string, bool) {
	if len(raw) < 2 || raw[len(raw)-1] != '"' {
		return "", false
	}
	if bytes.IndexByte(raw, '\\') < 0 {
		return string(raw[1 : len(raw)-1]), true
	}
	var key string
	if err := json.Unmarshal(raw, &key); err != nil {
		return "", false
	}
	return key, true
}

func skipSpace(data []byte, i int) int {
	for i < len(data) {
		switch data[i] {
		case ' ', '\t', '\n', '\r':
			i++
		default:
			return i
		}
	}
	return i
}

// valueEnd returns the index just past the JSON value starting at data[i].
func valueEnd(data []byte, i int) int {
	if i >= len(data) {
		return len(data)
	}
	switch data[i] {
	case '"':
		return stringEnd(data, i)
	case '{', '[':
		depth := 0
		for i < len(data) {
			switch data[i] {
			case '"':
				i = stringEnd(data, i)
				continue
			case '{', '[':
				depth++
			case '}', ']':
				depth--
				if depth == 0 {
					return i + 1
				}
			}
			i++
		}
		return len(data)
	default:
		// A number, true, false or null runs to the next delimiter.
		for i < len(data) {
			switch data[i] {
			case ',', '}', ']', ' ', '\t', '\n', '\r':
				return i
			}
			i++
		}
		return i
	}
}

// stringEnd returns the index just past the JSON string whose opening quote
// is data[i]. A quote closes the string unless an odd run of backslashes
// precedes it; bytes.IndexByte makes long base64 strings cheap to skip.
func stringEnd(data []byte, i int) int {
	start := i + 1
	for from := start; from < len(data); {
		quote := bytes.IndexByte(data[from:], '"')
		if quote < 0 {
			break
		}
		quote += from
		backslashes := 0
		for j := quote - 1; j >= start && data[j] == '\\'; j-- {
			backslashes++
		}
		if backslashes%2 == 0 {
			return quote + 1
		}
		from = quote + 1
	}
	return len(data)
}

func invalidf(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalidRule, fmt.Sprintf(format, args...))
}

// normalizeRule validates rule and returns its canonical form: trimmed text,
// lowercase channel, compact value and deduplicated paths, or the type's
// default paths when it has none. ID and UpdatedAt are carried over.
func normalizeRule(rule Rule) (Rule, error) {
	channel := strings.ToLower(strings.TrimSpace(rule.Channel))
	if !isLowerHex(channel, 16) {
		return Rule{}, invalidf("channel must be a CPA auth_index of 16 hex characters")
	}
	act, err := newAction(rule)
	if err != nil {
		return Rule{}, err
	}
	spec, _ := lookupType(act.kind)
	paths, err := normalizePaths(rule.Paths, spec)
	if err != nil {
		return Rule{}, err
	}
	note := strings.TrimSpace(rule.Note)
	if utf8.RuneCountInString(note) > maxNoteChars {
		return Rule{}, invalidf("note must be at most %d characters", maxNoteChars)
	}
	return Rule{
		ID:        strings.TrimSpace(rule.ID),
		Channel:   channel,
		Type:      act.kind,
		Paths:     paths,
		Field:     act.field,
		Value:     act.value,
		Overwrite: act.overwrite,
		Enabled:   rule.Enabled,
		Note:      note,
		UpdatedAt: rule.UpdatedAt,
	}, nil
}

func normalizePaths(paths []string, spec RuleType) ([]string, error) {
	out := make([]string, 0, min(len(paths), maxPaths))
	for i, path := range paths {
		path = strings.TrimSpace(path)
		switch {
		case !strings.HasPrefix(path, "/"):
			return nil, invalidf("paths[%d] must start with /", i)
		case strings.ContainsFunc(path, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }):
			return nil, invalidf("paths[%d] must not contain spaces", i)
		case utf8.RuneCountInString(path) > maxPathChars:
			return nil, invalidf("paths[%d] must be at most %d characters", i, maxPathChars)
		case slices.Contains(out, path):
			continue
		case len(out) == maxPaths:
			return nil, invalidf("at most %d paths", maxPaths)
		}
		out = append(out, path)
	}
	if len(out) > 0 {
		return out, nil
	}
	if len(spec.DefaultPaths) == 0 {
		return nil, invalidf("%s needs at least one path", spec.Type)
	}
	return slices.Clone(spec.DefaultPaths), nil
}

func isLowerHex(s string, length int) bool {
	if len(s) != length {
		return false
	}
	for i := 0; i < len(s); i++ {
		if c := s[i]; !('0' <= c && c <= '9' || 'a' <= c && c <= 'f') {
			return false
		}
	}
	return true
}

func isField(s string) bool {
	if s == "" || len(s) > maxFieldChars {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !('a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || '0' <= c && c <= '9' || c == '_' || c == '.' || c == '-') {
			return false
		}
	}
	return true
}

const idPrefix = "r_"

func isRuleID(id string) bool {
	return strings.HasPrefix(id, idPrefix) && isLowerHex(id[len(idPrefix):], 16)
}

func newRuleID(rules []Rule) (string, error) {
	for {
		var raw [8]byte
		if _, err := rand.Read(raw[:]); err != nil {
			return "", fmt.Errorf("generate adapter rule id: %w", err)
		}
		id := idPrefix + hex.EncodeToString(raw[:])
		if indexOf(rules, id) < 0 {
			return id, nil
		}
	}
}

func indexOf(rules []Rule, id string) int {
	return slices.IndexFunc(rules, func(rule Rule) bool { return rule.ID == id })
}

func cloneRule(rule Rule) Rule {
	rule.Paths = slices.Clone(rule.Paths)
	rule.Value = slices.Clone(rule.Value)
	return rule
}

// ruleFile is the on-disk document.
type ruleFile struct {
	Version int    `json:"version"`
	Rules   []Rule `json:"rules"`
}

// Manager owns the rule list, its file and the current Book.
type Manager struct {
	path     string
	canWrite func() bool
	now      func() time.Time
	book     atomic.Pointer[Book]

	// mu serializes changes and guards rules, which is replaced, never
	// modified in place: Books and callers share its slices.
	mu    sync.Mutex
	rules []Rule

	// hitsMu guards hits, which has an entry for every existing rule, so a
	// late RecordHits for a deleted rule cannot bring its counter back.
	hitsMu sync.Mutex
	hits   map[string]Hit
}

// NewManager loads the rules stored at path (a missing file means no rules).
// canWrite gates every save: the plugin instance that no longer owns the
// data directory must not write. now is the clock for UpdatedAt and hit
// times; RecordHits calls it from request goroutines, so it must be safe for
// concurrent use. A nil canWrite always allows saving and a nil now means
// time.Now.
func NewManager(path string, canWrite func() bool, now func() time.Time) (*Manager, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("adapter rules path is required")
	}
	if canWrite == nil {
		canWrite = func() bool { return true }
	}
	if now == nil {
		now = time.Now
	}
	rules, err := loadRules(path)
	if err != nil {
		return nil, fmt.Errorf("load adapter rules %s: %w", path, err)
	}
	m := &Manager{path: path, canWrite: canWrite, now: now, rules: rules, hits: make(map[string]Hit, len(rules))}
	for _, rule := range rules {
		m.hits[rule.ID] = Hit{}
	}
	m.book.Store(newBook(rules))
	return m, nil
}

// loadRules reads and validates the rule file. Rules on disk pass the same
// validation as Upsert, so a hand-edited file cannot bypass it.
func loadRules(path string) ([]Rule, error) {
	raw, err := persist.Read(path)
	if errors.Is(err, fs.ErrNotExist) {
		return []Rule{}, nil
	}
	if err != nil {
		return nil, err
	}
	var file ruleFile
	if err := decodeStrict(raw, &file); err != nil {
		return nil, err
	}
	if file.Version != fileVersion {
		return nil, fmt.Errorf("unsupported version %d", file.Version)
	}
	if len(file.Rules) > maxRules {
		return nil, invalidf("at most %d rules", maxRules)
	}
	rules := make([]Rule, 0, len(file.Rules))
	for i, stored := range file.Rules {
		rule, err := normalizeRule(stored)
		if err == nil && !isRuleID(rule.ID) {
			err = invalidf("id must be %s followed by 16 hex characters", idPrefix)
		}
		if err == nil && indexOf(rules, rule.ID) >= 0 {
			err = invalidf("duplicate id %s", rule.ID)
		}
		if err != nil {
			return nil, fmt.Errorf("rules[%d]: %w", i, err)
		}
		rules = append(rules, rule)
	}
	return rules, nil
}

func decodeStrict(raw []byte, target any) error {
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

func (m *Manager) save(rules []Rule) error {
	var buf bytes.Buffer
	encoder := json.NewEncoder(&buf)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(ruleFile{Version: fileVersion, Rules: rules}); err != nil {
		return fmt.Errorf("encode adapter rules: %w", err)
	}
	if err := persist.AtomicWrite(m.path, buf.Bytes()); err != nil {
		return fmt.Errorf("save adapter rules: %w", err)
	}
	return nil
}

// Book returns the current immutable index. It is never nil.
func (m *Manager) Book() *Book { return m.book.Load() }

// Rules returns every rule, enabled or not, in stable order: creation order,
// which is also the order rules of one channel run in.
func (m *Manager) Rules() []Rule {
	m.mu.Lock()
	defer m.mu.Unlock()
	rules := make([]Rule, len(m.rules))
	for i, rule := range m.rules {
		rules[i] = cloneRule(rule)
	}
	return rules
}

// Upsert validates and normalizes rule, assigns an id when it has none,
// persists the list and swaps the Book. An unknown id is ErrNotFound.
// Validation failures wrap ErrInvalidRule and a refused write is
// ErrReadOnly; on any error nothing changes.
func (m *Manager) Upsert(rule Rule) (Rule, error) {
	rule, err := normalizeRule(rule)
	if err != nil {
		return Rule{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.canWrite() {
		return Rule{}, ErrReadOnly
	}
	rule.UpdatedAt = m.now().UTC()
	next := slices.Clone(m.rules)
	created := rule.ID == ""
	if created {
		if len(next) >= maxRules {
			return Rule{}, invalidf("at most %d rules", maxRules)
		}
		if rule.ID, err = newRuleID(next); err != nil {
			return Rule{}, err
		}
		next = append(next, rule)
	} else {
		i := indexOf(next, rule.ID)
		if i < 0 {
			return Rule{}, fmt.Errorf("%w: %q", ErrNotFound, rule.ID)
		}
		next[i] = rule
	}
	if err := m.save(next); err != nil {
		return Rule{}, err
	}
	if created {
		// The counter exists before any Book can apply the rule.
		m.hitsMu.Lock()
		m.hits[rule.ID] = Hit{}
		m.hitsMu.Unlock()
	}
	m.rules = next
	m.book.Store(newBook(next))
	return cloneRule(rule), nil
}

// Delete removes a rule and persists the list. An unknown id is
// ErrNotFound and a refused write is ErrReadOnly; on any error nothing
// changes. The rule's hit counter goes with it.
func (m *Manager) Delete(id string) error {
	id = strings.TrimSpace(id)
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.canWrite() {
		return ErrReadOnly
	}
	i := indexOf(m.rules, id)
	if i < 0 {
		return fmt.Errorf("%w: %q", ErrNotFound, id)
	}
	next := slices.Delete(slices.Clone(m.rules), i, i+1)
	if err := m.save(next); err != nil {
		return err
	}
	m.rules = next
	m.book.Store(newBook(next))
	m.hitsMu.Lock()
	delete(m.hits, id)
	m.hitsMu.Unlock()
	return nil
}

// RecordHits counts one changed attempt for each applied rule id. Ids of
// rules deleted since the attempt took its Book are ignored.
func (m *Manager) RecordHits(applied []string) {
	if len(applied) == 0 {
		return
	}
	now := m.now().UTC()
	m.hitsMu.Lock()
	defer m.hitsMu.Unlock()
	for _, id := range applied {
		hit, ok := m.hits[id]
		if !ok {
			continue
		}
		hit.Count++
		hit.LastAt = now
		m.hits[id] = hit
	}
}

// Hits returns the per-rule counters of the rules that changed at least one
// attempt; a rule without an entry has no hits.
func (m *Manager) Hits() map[string]Hit {
	m.hitsMu.Lock()
	defer m.hitsMu.Unlock()
	hits := make(map[string]Hit)
	for id, hit := range m.hits {
		if hit.Count > 0 {
			hits[id] = hit
		}
	}
	return hits
}
