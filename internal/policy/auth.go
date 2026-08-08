package policy

import (
	"net/http"
	"strings"
	"time"
)

func (s *Store) Authenticate(method, path string, headers http.Header, query map[string][]string, body []byte) AuthDecision {
	rawKey := ExtractAPIKey(headers, query)
	key, enabled := s.findBySecretWhenEnabled(rawKey)
	if !enabled {
		return AuthDecision{Known: false, Reason: "plugin_disabled"}
	}
	if key == nil {
		return AuthDecision{Known: false, Reason: "unknown_key"}
	}
	decision := AuthDecision{
		Known:     true,
		KeyID:     key.ID,
		Principal: key.ID,
		ModelList: IsModelsEndpoint(path),
	}
	if !key.Enabled {
		decision.Reason = "key_disabled"
		return decision
	}
	if decision.ModelList {
		// Per-key override: a key with AllowModelsEndpoint=true may reach the
		// global /v1/models list; otherwise it's 401. We still cannot filter the
		// list contents per key (CPA limitation above), only hide/show it.
		if key.AllowModelsEndpoint {
			decision.Allowed = true
			decision.Reason = "models_endpoint_allowed"
			return decision
		}
		decision.Reason = "models_endpoint_disabled"
		return decision
	}
	requested := ExtractRequestedModel(path, query, body)
	decision.Requested = requested
	if requested != "" {
		// Must use the same multi-target selection as Route (priority /
		// round-robin), not ModelForAlias which always returns the first
		// match. Otherwise metadata["group"] can pin the wrong tier while
		// model.route forwards a different target.
		rule, ok := s.resolveRuleForAlias(key, requested)
		if !ok {
			decision.Reason = "model_not_allowed"
			return decision
		}
		decision.Rule = rule
	}
	limiter, usageLedger := s.runtimeComponents()
	if limiter != nil && !limiter.Allow(key.ID, key.RPM) {
		decision.RateLimited = true
		decision.Reason = "rpm_exceeded"
		return decision
	}
	// Dollar usage limit check (daily / trailing 7 days / trailing 30 days).
	// Only enforced when a limit is
	// set (>0). This is a pre-request gate; the request that pushes usage over
	// the limit is allowed through, and the next request is rejected — matching
	// the RPM limiter's "off-by-one" semantics.
	if usageLedger != nil {
		if reason, _ := usageLedger.OverLimit(key.ID, decision.Rule.Alias, quotaLimitsForKey(*key)); reason != "" {
			decision.CostLimited = true
			decision.Reason = reason
			return decision
		}
	}
	decision.Allowed = true
	decision.Reason = "allowed"

	// Remember this request's selected target so Route reuses it (same group /
	// provider / model). Only stash when the request is actually allowed — a
	// rate/cost-limited request never reaches model.route.
	if requested != "" {
		s.rememberPick(key.ID, requested, decision.Rule)
	}

	// Per-call image/video pre-charge workaround. CPA's XAI executor does not
	// emit usage records for /v1/images/* and /v1/videos/* (executeImages and
	// executeVideos lack a UsageReporter), so usage.handle never fires and the
	// plugin would never bill these. When the matched rule is per_call and the
	// path is an image/video endpoint, charge now, at access time. This is
	// unconditional (we cannot observe the upstream outcome here), so failed
	// requests are also charged — a known trade-off surfaced in the UI.
	if decision.Rule.BillingMode == "per_call" && IsImageVideoEndpoint(path) {
		alias := decision.Rule.Alias
		if alias == "" {
			alias = decision.Requested
		}
		model := decision.Rule.TargetModel
		if model == "" {
			model = alias
		}
		// failed=false so the per_call branch charges PerCallUSD. This is the
		// intended behavior for this workaround (no refund on upstream failure).
		s.recordUsage(key.ID, alias, model, false, UsageDetail{}, false)
		s.rememberPrecharge(key.ID, alias)
		decision.PreCharged = true
	}

	return decision
}

func (s *Store) Route(headers http.Header, query map[string][]string, requested string) (ModelRule, string, bool) {
	if !s.Enabled() {
		return ModelRule{}, "", false
	}
	key := s.findBySecret(ExtractAPIKey(headers, query))
	if key == nil || !key.Enabled {
		return ModelRule{}, "", false
	}
	// Prefer the selection Authenticate already made for this request so the
	// routed provider/model and the group stamped into scheduler metadata stay
	// aligned (critical for multi-target aliases with different groups).
	if rule, ok := s.takePick(key.ID, requested); ok {
		return rule, key.ID, true
	}
	rule, ok := s.resolveRuleForAlias(key, requested)
	if !ok {
		return ModelRule{}, key.ID, false
	}
	return rule, key.ID, true
}

// resolveRuleForAlias selects the ModelRule for the requested alias, applying
// the global alias's dispatch mode. For "round-robin" with multiple targets,
// it rotates through the targets using a global counter (shared across all
// keys). For "priority", it always returns the first target.
func (s *Store) resolveRuleForAlias(key *KeyConfig, requested string) (ModelRule, bool) {
	// Collect all ModelRules matching the alias (a multi-target alias expands
	// to multiple rules with the same alias but different provider/model/group).
	var matches []ModelRule
	for _, rule := range key.Models {
		if strings.EqualFold(rule.Alias, requested) {
			matches = append(matches, rule)
		}
	}
	if len(matches) == 0 {
		return ModelRule{}, false
	}
	if len(matches) == 1 {
		return matches[0], true
	}
	// Multiple targets: dispatch mode + RR counter need the store lock.
	s.mu.Lock()
	defer s.mu.Unlock()
	aliasName := strings.ToLower(strings.TrimSpace(requested))
	alias := s.aliases[aliasName]
	if alias != nil && strings.EqualFold(alias.Dispatch, "priority") {
		return matches[0], true // static priority: always first
	}
	// Round-robin (default): rotate using a global counter.
	idx := s.rrCounters[aliasName]
	if idx >= len(matches) {
		idx = 0
	}
	s.rrCounters[aliasName] = (idx + 1) % len(matches)
	return matches[idx], true
}

func pendingPickKey(keyID, alias string) string {
	return strings.ToLower(strings.TrimSpace(keyID)) + "\x00" + strings.ToLower(strings.TrimSpace(alias))
}

func (s *Store) clearPendingPicksForKeyLocked(keyID string) {
	prefix := strings.ToLower(strings.TrimSpace(keyID)) + "\x00"
	if prefix == "\x00" {
		return
	}
	for key := range s.pendingPicks {
		if strings.HasPrefix(key, prefix) {
			delete(s.pendingPicks, key)
		}
	}
	for key := range s.precharges {
		if strings.HasPrefix(key, prefix) {
			delete(s.precharges, key)
		}
	}
}

// rememberPick stores Authenticate's selected rule for a later Route call.
func (s *Store) rememberPick(keyID, alias string, rule ModelRule) {
	if strings.TrimSpace(keyID) == "" || strings.TrimSpace(alias) == "" {
		return
	}
	k := pendingPickKey(keyID, alias)
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.pendingPicks == nil {
		s.pendingPicks = make(map[string][]pendingPick)
	}
	q := s.prunePendingLocked(s.pendingPicks[k], now)
	q = append(q, pendingPick{rule: rule, at: now})
	if len(q) > pendingPickMaxQueue {
		q = q[len(q)-pendingPickMaxQueue:]
	}
	s.pendingPicks[k] = q
}

// takePick consumes the oldest non-expired selection for this key+alias.
// Returns false when nothing is pending (Route-only callers / tests).
func (s *Store) takePick(keyID, alias string) (ModelRule, bool) {
	if strings.TrimSpace(keyID) == "" || strings.TrimSpace(alias) == "" {
		return ModelRule{}, false
	}
	k := pendingPickKey(keyID, alias)
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	q := s.prunePendingLocked(s.pendingPicks[k], now)
	if len(q) == 0 {
		delete(s.pendingPicks, k)
		return ModelRule{}, false
	}
	pick := q[0]
	q = q[1:]
	if len(q) == 0 {
		delete(s.pendingPicks, k)
	} else {
		s.pendingPicks[k] = q
	}
	return pick.rule, true
}

// prunePendingLocked drops expired entries. Caller must hold s.mu.
func (s *Store) prunePendingLocked(q []pendingPick, now time.Time) []pendingPick {
	if len(q) == 0 {
		return q
	}
	i := 0
	for i < len(q) && now.Sub(q[i].at) > pendingPickTTL {
		i++
	}
	if i == 0 {
		return q
	}
	if i >= len(q) {
		return nil
	}
	return q[i:]
}

func (s *Store) ResponseAlias(headers http.Header, query map[string][]string, requested string) (string, bool) {
	rule, _, ok := s.Route(headers, query, requested)
	if !ok {
		return "", false
	}
	return rule.Alias, true
}
