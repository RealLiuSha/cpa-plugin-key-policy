// Package homeadapter keeps the cpa-key-policy management contract
// (/v0/management/plugins/cpa-key-policy/...) on top of CLIProxyAPIHome.
//
// In a Home cluster the key value, the quota windows, pricing and usage all
// live in Home's database, so this package stores nothing itself. One policy
// key is one Home user named Prefix+id, which carries the quota windows, plus
// one Home api_key bound to that user, which carries the key value.
package homeadapter

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	softLimitRatio  = 0.8
	modelGroupLabel = "kp-"
	calendarMode    = "calendar"
)

// Windows maps contract window names to Home period-limit ids.
var windows = [...]struct{ name, home string }{
	{"daily", "1d"},
	{"weekly", "7d"},
	{"monthly", "30d"},
}

// ContractError is an error the HTTP layer reports as-is.
type ContractError struct {
	Status  int
	Code    string
	Message string
}

func (e *ContractError) Error() string { return e.Code + ": " + e.Message }

func notFound(id string) error {
	return &ContractError{http.StatusNotFound, "not_found", fmt.Sprintf("key %q not found", id)}
}

func invalid(format string, args ...any) error {
	return &ContractError{http.StatusBadRequest, "invalid_request", fmt.Sprintf(format, args...)}
}

func unsupported(format string, args ...any) error {
	return &ContractError{http.StatusBadRequest, "unsupported_in_home", fmt.Sprintf(format, args...)}
}

// ModelRef is a model a key may call. Per-model limits do not exist in Home.
type ModelRef struct {
	Name          string  `json:"name"`
	DailyLimitUSD float64 `json:"daily_limit_usd,omitempty"`
}

// KeyWrite is the body of POST and PATCH /keys. Nil fields are not provided.
type KeyWrite struct {
	ID                  string      `json:"id"`
	Name                *string     `json:"name"`
	Enabled             *bool       `json:"enabled"`
	Key                 string      `json:"key"`
	RPM                 *int        `json:"rpm"`
	Models              *[]ModelRef `json:"models"`
	DailyLimitUSD       *float64    `json:"daily_limit_usd"`
	WeeklyLimitUSD      *float64    `json:"weekly_limit_usd"`
	MonthlyLimitUSD     *float64    `json:"monthly_limit_usd"`
	AllowModelsEndpoint *bool       `json:"allow_models_endpoint"`
}

func (w KeyWrite) validate() error {
	if w.ID == "" || strings.ContainsAny(w.ID, " \t\r\n/") {
		return invalid("id is required and must not contain whitespace or '/'")
	}
	if strings.ContainsAny(w.Key, " \t\r\n") {
		return invalid("key must not contain whitespace")
	}
	if w.RPM != nil && *w.RPM > 0 {
		return unsupported("rpm limits are not enforced by Home; send rpm 0")
	}
	if w.Models != nil {
		for _, ref := range *w.Models {
			if ref.DailyLimitUSD > 0 {
				return unsupported("per-model daily limits are not enforced by Home (model %q)", ref.Name)
			}
		}
	}
	for _, limit := range []*float64{w.DailyLimitUSD, w.WeeklyLimitUSD, w.MonthlyLimitUSD} {
		if limit != nil && *limit < 0 {
			return invalid("limits must not be negative")
		}
	}
	return nil
}

// PublicKey is one entry of GET /keys, field-compatible with cpa-key-policy.
type PublicKey struct {
	ID                  string       `json:"id"`
	Name                string       `json:"name"`
	Enabled             bool         `json:"enabled"`
	KeyPreview          string       `json:"key_preview"`
	KeyHash             string       `json:"key_hash,omitempty"`
	RPM                 int          `json:"rpm"`
	Models              []ModelRef   `json:"models"`
	DailyLimitUSD       float64      `json:"daily_limit_usd"`
	WeeklyLimitUSD      float64      `json:"weekly_limit_usd"`
	MonthlyLimitUSD     float64      `json:"monthly_limit_usd"`
	AllowModelsEndpoint bool         `json:"allow_models_endpoint"`
	Usage               UsageSummary `json:"usage"`
	CreatedAt           string       `json:"created_at,omitempty"`
	UpdatedAt           string       `json:"updated_at,omitempty"`
}

type CycleSummary struct {
	Window             string    `json:"window"`
	StartedAt          time.Time `json:"started_at"`
	ResetsAt           time.Time `json:"resets_at"`
	ResetKind          string    `json:"reset_kind"`
	UsedUSD            float64   `json:"used_usd"`
	LimitUSD           float64   `json:"limit_usd"`
	ResetAfterManualAt time.Time `json:"reset_after_manual_at"`
}

type UsageSummary struct {
	Cycles                   []CycleSummary `json:"cycles"`
	Status                   string         `json:"status"`
	BlockedReason            string         `json:"blocked_reason,omitempty"`
	LimitedModels            []string       `json:"limited_models"`
	DailyUSD                 float64        `json:"daily_usd"`
	WeeklyUSD                float64        `json:"weekly_usd"`
	MonthlyUSD               float64        `json:"monthly_usd"`
	DailyLimitUSD            float64        `json:"daily_limit_usd"`
	WeeklyLimitUSD           float64        `json:"weekly_limit_usd"`
	MonthlyLimitUSD          float64        `json:"monthly_limit_usd"`
	NextAccountingBoundaryAt time.Time      `json:"next_accounting_boundary_at"`
	Timezone                 string         `json:"timezone"`
}

// entry is everything Home knows about one policy key.
type entry struct {
	user    User
	key     *APIKey
	models  []string
	windows []PeriodWindow
}

type snapshot struct {
	at      time.Time
	entries []*entry
}

// Service maps the contract onto Home. Reads go through a short-lived cache so
// clients that poll GET /keys do not fan out one Home call per key each time;
// every write drops the cache of this instance.
type Service struct {
	home     *Client
	prefix   string
	location *time.Location
	ttl      time.Duration
	now      func() time.Time

	mu     sync.Mutex
	cached *snapshot
}

func NewService(home *Client, prefix string, location *time.Location, ttl time.Duration) *Service {
	return &Service{home: home, prefix: prefix, location: location, ttl: ttl, now: time.Now}
}

func (s *Service) username(id string) string { return s.prefix + id }

func (s *Service) invalidate() {
	s.mu.Lock()
	s.cached = nil
	s.mu.Unlock()
}

// List returns every policy key with its current usage.
func (s *Service) List(ctx context.Context) ([]PublicKey, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cached == nil || s.now().Sub(s.cached.at) >= s.ttl {
		snap, err := s.load(ctx, "")
		if err != nil {
			return nil, err
		}
		s.cached = snap
	}
	out := make([]PublicKey, 0, len(s.cached.entries))
	for _, e := range s.cached.entries {
		out = append(out, s.publicKey(e))
	}
	return out, nil
}

// load reads policy keys from Home; a non-empty onlyID limits it to one key.
func (s *Service) load(ctx context.Context, onlyID string) (*snapshot, error) {
	users, err := s.home.ListUsers(ctx)
	if err != nil {
		return nil, err
	}
	keys, err := s.home.ListAPIKeys(ctx)
	if err != nil {
		return nil, err
	}
	groups, details, err := s.home.ListModelGroups(ctx)
	if err != nil {
		return nil, err
	}
	modelsByGroup := groupModels(groups, details)
	keyByUser := make(map[uint]*APIKey)
	for i := range keys {
		k := &keys[i]
		if k.UserID == nil {
			continue
		}
		if current, ok := keyByUser[*k.UserID]; !ok || k.ID < current.ID {
			keyByUser[*k.UserID] = k
		}
	}
	snap := &snapshot{at: s.now()}
	for _, u := range users {
		if !strings.HasPrefix(u.Username, s.prefix) || (onlyID != "" && u.Username != s.username(onlyID)) {
			continue
		}
		e := &entry{user: u, key: keyByUser[u.ID]}
		if e.key != nil {
			e.models = modelsOf(e.key.ModelGroups, modelsByGroup)
		}
		snap.entries = append(snap.entries, e)
	}
	sort.Slice(snap.entries, func(i, j int) bool { return snap.entries[i].user.Username < snap.entries[j].user.Username })

	// Period limits are per user; a bounded fan-out keeps a full refresh short.
	sem := make(chan struct{}, 8)
	errs := make(chan error, len(snap.entries))
	var wg sync.WaitGroup
	for _, e := range snap.entries {
		wg.Add(1)
		sem <- struct{}{}
		go func(e *entry) {
			defer wg.Done()
			defer func() { <-sem }()
			windows, err := s.home.PeriodLimits(ctx, e.user.ID)
			if err != nil {
				errs <- fmt.Errorf("period limits of %s: %w", e.user.Username, err)
				return
			}
			e.windows = windows
		}(e)
	}
	wg.Wait()
	close(errs)
	if err := <-errs; err != nil {
		return nil, err
	}
	return snap, nil
}

func (s *Service) loadOne(ctx context.Context, id string) (*entry, error) {
	snap, err := s.load(ctx, id)
	if err != nil {
		return nil, err
	}
	if len(snap.entries) == 0 {
		return nil, notFound(id)
	}
	return snap.entries[0], nil
}

func groupModels(groups []ModelGroup, details []ModelGroupDetail) map[uint][]string {
	enabled := make(map[uint]bool, len(groups))
	for _, g := range groups {
		enabled[g.ID] = !g.Disabled
	}
	out := make(map[uint][]string)
	for _, d := range details {
		if enabled[d.GroupID] {
			out[d.GroupID] = append(out[d.GroupID], d.ModelID)
		}
	}
	return out
}

func modelsOf(groupIDs []uint, modelsByGroup map[uint][]string) []string {
	seen := make(map[string]bool)
	var out []string
	for _, id := range groupIDs {
		for _, model := range modelsByGroup[id] {
			if lower := strings.ToLower(model); !seen[lower] {
				seen[lower] = true
				out = append(out, model)
			}
		}
	}
	sort.Strings(out)
	return out
}

func (s *Service) publicKey(e *entry) PublicKey {
	id := strings.TrimPrefix(e.user.Username, s.prefix)
	out := PublicKey{
		ID:                  id,
		Name:                id,
		Enabled:             e.key != nil && e.user.CreditsUnlimited,
		RPM:                 0,
		Models:              make([]ModelRef, 0, len(e.models)),
		DailyLimitUSD:       limitUSD(e.user.Limit1d),
		WeeklyLimitUSD:      limitUSD(e.user.Limit7d),
		MonthlyLimitUSD:     limitUSD(e.user.Limit30d),
		AllowModelsEndpoint: true,
		Usage:               s.summary(e),
	}
	for _, model := range e.models {
		out.Models = append(out.Models, ModelRef{Name: model})
	}
	if e.key != nil {
		out.KeyPreview = previewKey(e.key.Value)
		sum := sha256.Sum256([]byte(e.key.Value))
		out.KeyHash = "sha256:" + hex.EncodeToString(sum[:])
		if e.key.DisplayName != nil && strings.TrimSpace(*e.key.DisplayName) != "" {
			out.Name = strings.TrimSpace(*e.key.DisplayName)
		}
	}
	if e.user.CreatedAt != nil {
		out.CreatedAt = e.user.CreatedAt.UTC().Format(time.RFC3339)
	}
	if e.user.UpdatedAt != nil {
		out.UpdatedAt = e.user.UpdatedAt.UTC().Format(time.RFC3339)
	}
	return out
}

func limitUSD(limit *float64) float64 {
	if limit == nil {
		return 0
	}
	return *limit
}

func previewKey(key string) string {
	if len(key) <= 12 {
		return key
	}
	return key[:7] + "..." + key[len(key)-5:]
}

// summary projects Home period windows onto the contract's quota cycles.
func (s *Service) summary(e *entry) UsageSummary {
	now := s.now().In(s.location)
	out := UsageSummary{
		Status:                   "normal",
		LimitedModels:            []string{},
		Timezone:                 s.location.String(),
		NextAccountingBoundaryAt: time.Date(now.Year(), now.Month(), now.Day()+1, 0, 0, 0, 0, s.location),
	}
	warning := false
	for _, window := range windows {
		cycle := CycleSummary{Window: window.name, ResetKind: "automatic"}
		if w := findWindow(e.windows, window.home); w != nil {
			cycle.UsedUSD, cycle.LimitUSD = w.Used, limitUSD(w.Limit)
			if w.WindowStart != nil {
				cycle.StartedAt = *w.WindowStart
			}
			if w.ResetAt != nil {
				cycle.ResetsAt = *w.ResetAt
			}
			if w.UsageEpoch != nil && w.UsageEpoch.After(cycle.StartedAt) {
				cycle.StartedAt, cycle.ResetKind = *w.UsageEpoch, "manual"
			}
		}
		// A Home counter reset keeps the calendar anchor, so a manual reset
		// never moves the next reset date.
		cycle.ResetAfterManualAt = cycle.ResetsAt
		out.Cycles = append(out.Cycles, cycle)
		switch window.name {
		case "daily":
			out.DailyUSD, out.DailyLimitUSD = cycle.UsedUSD, cycle.LimitUSD
		case "weekly":
			out.WeeklyUSD, out.WeeklyLimitUSD = cycle.UsedUSD, cycle.LimitUSD
		case "monthly":
			out.MonthlyUSD, out.MonthlyLimitUSD = cycle.UsedUSD, cycle.LimitUSD
		}
		if cycle.LimitUSD > 0 && cycle.UsedUSD >= cycle.LimitUSD && out.BlockedReason == "" {
			out.BlockedReason = window.name + "_exceeded"
		}
		if cycle.LimitUSD > 0 && cycle.UsedUSD >= cycle.LimitUSD*softLimitRatio {
			warning = true
		}
	}
	switch {
	case out.BlockedReason != "":
		out.Status = "limited"
	case warning:
		out.Status = "warning"
	}
	return out
}

func findWindow(list []PeriodWindow, id string) *PeriodWindow {
	for i := range list {
		if list[i].ID == id {
			return &list[i]
		}
	}
	return nil
}

func homeWindow(name string) (string, bool) {
	for _, window := range windows {
		if window.name == name {
			return window.home, true
		}
	}
	return "", false
}

// weekday converts to Home's week_reset_day, 1 = Monday ... 7 = Sunday.
func weekday(t time.Time) int {
	if day := int(t.Weekday()); day != 0 {
		return day
	}
	return 7
}

// Upsert implements POST /keys: it creates the key or overwrites the existing
// key with the same id, like cpa-key-policy did. A new key's weekly window
// starts on resetDay at 00:00; resetDay 0 means the current local weekday.
func (s *Service) Upsert(ctx context.Context, w KeyWrite, resetDay int) (PublicKey, error) {
	if err := w.validate(); err != nil {
		return PublicKey{}, err
	}
	if w.Key == "" {
		return PublicKey{}, invalid("key is required; Home stores the key value given by the caller")
	}
	defer s.invalidate()
	user, err := s.findUser(ctx, w.ID)
	if err != nil {
		return PublicKey{}, err
	}
	write := s.policyWrite(w, true)
	if user == nil {
		if resetDay == 0 {
			resetDay = weekday(s.now().In(s.location))
		}
		username, timezone, mode, hour := s.username(w.ID), s.location.String(), calendarMode, 0
		write.Username, write.Timezone, write.WeekResetDay, write.WeekResetHour = &username, &timezone, &resetDay, &hour
		write.WindowMode1d, write.WindowMode7d, write.WindowMode30d = &mode, &mode, &mode
		created, err := s.home.CreateUser(ctx, write)
		if err != nil {
			return PublicKey{}, err
		}
		user = &created
	} else if _, err := s.home.UpdateUser(ctx, user.ID, write); err != nil {
		return PublicKey{}, err
	}
	var models []ModelRef
	if w.Models != nil {
		models = *w.Models
	}
	groupID, err := s.ensureGroup(ctx, models)
	if err != nil {
		return PublicKey{}, err
	}
	if err := s.bindKey(ctx, user.ID, w.Key, displayName(w), []uint{groupID}); err != nil {
		return PublicKey{}, err
	}
	return s.get(ctx, w.ID)
}

// Patch implements PATCH /keys: only the provided fields change.
func (s *Service) Patch(ctx context.Context, w KeyWrite) (PublicKey, error) {
	if err := w.validate(); err != nil {
		return PublicKey{}, err
	}
	defer s.invalidate()
	e, err := s.loadOne(ctx, w.ID)
	if err != nil {
		return PublicKey{}, err
	}
	if _, err := s.home.UpdateUser(ctx, e.user.ID, s.policyWrite(w, false)); err != nil {
		return PublicKey{}, err
	}
	groups, name := []uint(nil), ""
	if e.key != nil {
		groups, name = e.key.ModelGroups, strings.TrimPrefix(e.user.Username, s.prefix)
		if e.key.DisplayName != nil {
			name = *e.key.DisplayName
		}
	}
	if w.Name != nil {
		name = strings.TrimSpace(*w.Name)
	}
	if w.Models != nil {
		groupID, err := s.ensureGroup(ctx, *w.Models)
		if err != nil {
			return PublicKey{}, err
		}
		groups = []uint{groupID}
	}
	value := w.Key
	if value == "" && e.key != nil {
		value = e.key.Value
	}
	if value != "" {
		if len(groups) == 0 {
			groupID, err := s.ensureGroup(ctx, nil)
			if err != nil {
				return PublicKey{}, err
			}
			groups = []uint{groupID}
		}
		if name == "" {
			name = w.ID
		}
		if err := s.bindKey(ctx, e.user.ID, value, name, groups); err != nil {
			return PublicKey{}, err
		}
	}
	return s.get(ctx, w.ID)
}

// policyWrite maps the contract policy fields onto a Home user update. A
// disabled key keeps its value but loses its credits, which Home rejects.
func (s *Service) policyWrite(w KeyWrite, overwrite bool) UserWrite {
	var write UserWrite
	enabled := w.Enabled
	if enabled == nil && overwrite {
		value := true
		enabled = &value
	}
	if enabled != nil {
		zero := 0.0
		write.CreditsUnlimited, write.Credits = enabled, &zero
	}
	limits := []struct {
		value  *float64
		target **Limit
	}{{w.DailyLimitUSD, &write.Limit1d}, {w.WeeklyLimitUSD, &write.Limit7d}, {w.MonthlyLimitUSD, &write.Limit30d}}
	for _, l := range limits {
		switch {
		case l.value != nil:
			limit := Limit(*l.value)
			*l.target = &limit
		case overwrite:
			// POST replaces the whole policy, so an omitted limit means none.
			limit := Limit(0)
			*l.target = &limit
		}
	}
	return write
}

func displayName(w KeyWrite) string {
	if w.Name != nil && strings.TrimSpace(*w.Name) != "" {
		return strings.TrimSpace(*w.Name)
	}
	return w.ID
}

func (s *Service) findUser(ctx context.Context, id string) (*User, error) {
	users, err := s.home.ListUsers(ctx)
	if err != nil {
		return nil, err
	}
	for i := range users {
		if users[i].Username == s.username(id) {
			return &users[i], nil
		}
	}
	return nil, nil
}

// bindKey makes value the only key of the user. A value that already belongs
// to someone else is a conflict, never a silent move.
func (s *Service) bindKey(ctx context.Context, userID uint, value, name string, groups []uint) error {
	keys, err := s.home.ListAPIKeys(ctx)
	if err != nil {
		return err
	}
	bound := false
	for _, k := range keys {
		if k.Value == value {
			if k.UserID == nil || *k.UserID != userID {
				return &ContractError{http.StatusConflict, "key_conflict", "the key value already belongs to another Home user"}
			}
			bound = true
			if err := s.home.UpdateAPIKey(ctx, k.ID, name, groups); err != nil {
				return err
			}
		}
	}
	for _, k := range keys {
		if k.UserID != nil && *k.UserID == userID && k.Value != value {
			if err := s.home.DeleteAPIKey(ctx, k.ID); err != nil {
				return err
			}
		}
	}
	if bound {
		return nil
	}
	return s.home.CreateAPIKey(ctx, value, name, userID, groups)
}

// ensureGroup returns the model group holding exactly these models; nil or
// empty means every model that has a Home price. Groups are named after a
// hash of their content, so equal model sets share one group.
func (s *Service) ensureGroup(ctx context.Context, refs []ModelRef) (uint, error) {
	var names []string
	for _, ref := range refs {
		if name := strings.TrimSpace(ref.Name); name != "" {
			names = append(names, name)
		}
	}
	if len(names) == 0 {
		priced, err := s.Models(ctx)
		if err != nil {
			return 0, err
		}
		for _, model := range priced {
			names = append(names, model.Name)
		}
		if len(names) == 0 {
			return 0, invalid("no model has a Home price yet; configure prices before creating keys")
		}
	}
	names = uniqueFold(names)
	sum := sha256.Sum256([]byte(strings.ToLower(strings.Join(names, "\n"))))
	label := modelGroupLabel + hex.EncodeToString(sum[:6])
	groups, details, err := s.home.ListModelGroups(ctx)
	if err != nil {
		return 0, err
	}
	var groupID uint
	for _, g := range groups {
		if g.Name == label && !g.Disabled && (groupID == 0 || g.ID < groupID) {
			groupID = g.ID
		}
	}
	if groupID == 0 {
		created, err := s.home.CreateModelGroup(ctx, label)
		if err != nil {
			return 0, err
		}
		groupID = created.ID
	}
	present := make(map[string]bool)
	for _, d := range details {
		if d.GroupID == groupID {
			present[strings.ToLower(d.ModelID)] = true
		}
	}
	for _, name := range names {
		if !present[strings.ToLower(name)] {
			if err := s.home.CreateModelGroupDetail(ctx, groupID, name); err != nil {
				return 0, err
			}
		}
	}
	return groupID, nil
}

func uniqueFold(names []string) []string {
	seen := make(map[string]bool)
	var out []string
	for _, name := range names {
		if lower := strings.ToLower(name); !seen[lower] {
			seen[lower] = true
			out = append(out, name)
		}
	}
	sort.Slice(out, func(i, j int) bool { return strings.ToLower(out[i]) < strings.ToLower(out[j]) })
	return out
}

func (s *Service) get(ctx context.Context, id string) (PublicKey, error) {
	e, err := s.loadOne(ctx, id)
	if err != nil {
		return PublicKey{}, err
	}
	return s.publicKey(e), nil
}

// Delete removes the key value first, then the user, so a half-finished delete
// leaves an unusable user instead of a key without limits.
func (s *Service) Delete(ctx context.Context, id string) error {
	defer s.invalidate()
	e, err := s.loadOne(ctx, id)
	if err != nil {
		return err
	}
	keys, err := s.home.ListAPIKeys(ctx)
	if err != nil {
		return err
	}
	for _, k := range keys {
		if k.UserID != nil && *k.UserID == e.user.ID {
			if err := s.home.DeleteAPIKey(ctx, k.ID); err != nil {
				return err
			}
		}
	}
	return s.home.DeleteUser(ctx, e.user.ID)
}

// ResetUsage restarts the counter of one window without moving its calendar.
func (s *Service) ResetUsage(ctx context.Context, id, window string) (map[string]any, error) {
	home, ok := homeWindow(window)
	if !ok {
		return nil, invalid("window must be daily, weekly or monthly")
	}
	defer s.invalidate()
	e, err := s.loadOne(ctx, id)
	if err != nil {
		return nil, err
	}
	before := s.summary(e)
	if err := s.home.ResetPeriod(ctx, e.user.ID, []string{home}); err != nil {
		return nil, err
	}
	after, err := s.get(ctx, id)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"id": id, "window": window,
		"before_daily_usd": before.DailyUSD, "before_weekly_usd": before.WeeklyUSD, "before_monthly_usd": before.MonthlyUSD,
		"after_daily_usd": after.Usage.DailyUSD, "after_weekly_usd": after.Usage.WeeklyUSD, "after_monthly_usd": after.Usage.MonthlyUSD,
		"next_accounting_boundary_at": after.Usage.NextAccountingBoundaryAt,
	}, nil
}

// UsageWindow is one window of the per-model breakdown.
type UsageWindow struct {
	TotalUSD     float64   `json:"total_usd"`
	WindowStart  time.Time `json:"window_start,omitempty"`
	InputTokens  int64     `json:"input_tokens,omitempty"`
	OutputTokens int64     `json:"output_tokens,omitempty"`
	CallCount    int64     `json:"call_count,omitempty"`
}

type ModelUsage struct {
	Name     string      `json:"name"`
	InConfig bool        `json:"in_config"`
	Daily    UsageWindow `json:"daily"`
	Weekly   UsageWindow `json:"weekly"`
	Monthly  UsageWindow `json:"monthly"`
}

// Usage implements GET /keys/usage: per-model spend inside the current windows.
func (s *Service) Usage(ctx context.Context, id string) (map[string]any, error) {
	e, err := s.loadOne(ctx, id)
	if err != nil {
		return nil, err
	}
	summary := s.summary(e)
	var starts [len(windows)]time.Time
	var from time.Time
	for i, cycle := range summary.Cycles {
		starts[i] = cycle.StartedAt
		if !cycle.StartedAt.IsZero() && (from.IsZero() || cycle.StartedAt.Before(from)) {
			from = cycle.StartedAt
		}
	}
	byModel := make(map[string]*ModelUsage)
	order := []string{}
	add := func(name string, inConfig bool) *ModelUsage {
		key := strings.ToLower(name)
		if m, ok := byModel[key]; ok {
			return m
		}
		m := &ModelUsage{Name: name, InConfig: inConfig}
		m.Daily.WindowStart, m.Weekly.WindowStart, m.Monthly.WindowStart = starts[0], starts[1], starts[2]
		byModel[key] = m
		order = append(order, key)
		return m
	}
	for _, model := range e.models {
		add(model, true)
	}
	if !from.IsZero() {
		charges, err := s.home.ListCharges(ctx, e.user.ID, from, s.now())
		if err != nil {
			return nil, err
		}
		for _, c := range charges {
			m := add(c.Model, false)
			for i, w := range []*UsageWindow{&m.Daily, &m.Weekly, &m.Monthly} {
				if starts[i].IsZero() || c.CreatedAt.Before(starts[i]) {
					continue
				}
				w.TotalUSD += c.Amount
				w.InputTokens += c.InputTokens
				w.OutputTokens += c.OutputTokens
				w.CallCount++
			}
		}
	}
	models := make([]ModelUsage, 0, len(order))
	for _, key := range order {
		models = append(models, *byModel[key])
	}
	pk := s.publicKey(e)
	return map[string]any{
		"key_id": pk.ID, "key_name": pk.Name,
		"daily_limit_usd": pk.DailyLimitUSD, "weekly_limit_usd": pk.WeeklyLimitUSD, "monthly_limit_usd": pk.MonthlyLimitUSD,
		"models": models, "usage": summary,
	}, nil
}

// PublicModel is one entry of GET /models: a model that has a Home price.
type PublicModel struct {
	Name                      string  `json:"name"`
	Provider                  string  `json:"provider"`
	TargetModel               string  `json:"target_model"`
	BillingMode               string  `json:"billing_mode"`
	InputPricePerMillion      float64 `json:"input_price_per_million"`
	OutputPricePerMillion     float64 `json:"output_price_per_million"`
	CacheReadPricePerMillion  float64 `json:"cache_read_price_per_million"`
	CacheWritePricePerMillion float64 `json:"cache_write_price_per_million"`
	PerCallUSD                float64 `json:"per_call_usd,omitempty"`
}

// Models lists the models callers may sell: every enabled Home price rule of
// the default tier. Home owns prices, so the adapter never keeps a copy.
func (s *Service) Models(ctx context.Context) ([]PublicModel, error) {
	prices, err := s.home.ListModelPrices(ctx)
	if err != nil {
		return nil, err
	}
	seen := make(map[string]bool)
	var out []PublicModel
	for _, p := range prices {
		tier := strings.ToLower(strings.TrimSpace(p.ServiceTier))
		if !p.Enabled || p.MinInputTokens != 0 || (tier != "*" && tier != "" && tier != "standard") || seen[strings.ToLower(p.Model)] {
			continue
		}
		seen[strings.ToLower(p.Model)] = true
		mode := "tokens"
		if p.Request > 0 && p.Input == 0 && p.Output == 0 {
			mode = "per_call"
		}
		out = append(out, PublicModel{
			Name: p.Model, Provider: p.Provider, TargetModel: p.Model, BillingMode: mode,
			InputPricePerMillion: p.Input, OutputPricePerMillion: p.Output,
			CacheReadPricePerMillion: p.CacheRead, CacheWritePricePerMillion: p.CacheWrite, PerCallUSD: p.Request,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// asContractError reports Home failures without leaking transport details as
// client errors: a 4xx from Home is the caller's input, anything else is ours.
func asContractError(err error) *ContractError {
	var contract *ContractError
	if errors.As(err, &contract) {
		return contract
	}
	var homeErr *HomeError
	if errors.As(err, &homeErr) && homeErr.Status >= 400 && homeErr.Status < 500 {
		status := http.StatusBadRequest
		if homeErr.Status == http.StatusConflict || homeErr.Status == http.StatusNotFound {
			status = homeErr.Status
		}
		return &ContractError{status, homeErr.Code, homeErr.Message}
	}
	return &ContractError{http.StatusBadGateway, "home_unavailable", err.Error()}
}
