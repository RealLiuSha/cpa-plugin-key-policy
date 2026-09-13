package policy

import (
	"net/http"
	"strings"
	"time"
)

func (s *Store) Authenticate(method, path string, headers http.Header, query map[string][]string, body []byte) AuthDecision {
	s.lifecycleMu.RLock()
	defer s.lifecycleMu.RUnlock()
	rawKey := ExtractAPIKey(headers, query)
	key, enabled := s.findBySecretWhenEnabled(rawKey)
	if !enabled {
		return AuthDecision{Reason: "plugin_disabled"}
	}
	if key == nil {
		return AuthDecision{Reason: "unknown_key"}
	}
	decision := AuthDecision{Known: true, KeyID: key.ID, Principal: key.ID, ModelList: IsModelsEndpoint(path)}
	if !key.Enabled {
		decision.Reason = "key_disabled"
		return decision
	}
	if decision.ModelList {
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
		route, ok := s.resolveRouteForModel(key, requested)
		if !ok {
			decision.Reason = "model_not_allowed"
			return decision
		}
		decision.Route = route
	}
	limiter, usageLedger := s.runtimeComponents()
	if limiter != nil {
		allowed := limiter.Allow(key.ID, key.RPM)
		if !allowed {
			decision.RateLimited = true
			decision.Reason = "rpm_exceeded"
			return decision
		}
	}
	if usageLedger != nil {
		if reason, _ := usageLedger.OverLimit(key.ID, decision.Route.PublicModel, quotaLimitsForKey(*key)); reason != "" {
			decision.CostLimited = true
			decision.Reason = reason
			return decision
		}
	}
	decision.Allowed = true
	decision.Reason = "allowed"
	if requested != "" {
		s.rememberPick(key.ID, requested, decision.Route)
	}

	if decision.Route.BillingMode == "per_call" && IsImageVideoEndpoint(path) {
		publicModel := decision.Route.PublicModel
		if publicModel == "" {
			publicModel = requested
		}
		targetModel := decision.Route.TargetModel
		if targetModel == "" {
			targetModel = publicModel
		}
		s.recordUsage(key.ID, publicModel, targetModel, false, UsageDetail{}, false)
		s.rememberPrecharge(key.ID, publicModel)
		decision.PreCharged = true
	}
	return decision
}

func (s *Store) Route(headers http.Header, query map[string][]string, requested string) (ResolvedModelRoute, string, bool) {
	if !s.Enabled() {
		return ResolvedModelRoute{}, "", false
	}
	key := s.findBySecret(ExtractAPIKey(headers, query))
	if key == nil || !key.Enabled {
		return ResolvedModelRoute{}, "", false
	}
	if route, ok := s.takePick(key.ID, requested); ok {
		return route, key.ID, true
	}
	route, ok := s.resolveRouteForModel(key, requested)
	if !ok {
		return ResolvedModelRoute{}, key.ID, false
	}
	return route, key.ID, true
}

func (s *Store) resolveRouteForModel(key *KeyConfig, requested string) (ResolvedModelRoute, bool) {
	model, ok := s.modelForKey(key, requested)
	if !ok || len(model.Targets) == 0 {
		return ResolvedModelRoute{}, false
	}
	if len(model.Targets) == 1 || model.Dispatch == "priority" {
		return resolveModelRoute(model, model.Targets[0]), true
	}
	s.mu.Lock()
	modelName := strings.ToLower(model.Name)
	index := s.rrCounters[modelName]
	if index >= len(model.Targets) {
		index = 0
	}
	s.rrCounters[modelName] = (index + 1) % len(model.Targets)
	s.mu.Unlock()
	return resolveModelRoute(model, model.Targets[index]), true
}

func pendingPickKey(keyID, model string) string {
	return strings.ToLower(strings.TrimSpace(keyID)) + "\x00" + strings.ToLower(strings.TrimSpace(model))
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

func (s *Store) rememberPick(keyID, model string, route ResolvedModelRoute) {
	if strings.TrimSpace(keyID) == "" || strings.TrimSpace(model) == "" {
		return
	}
	key := pendingPickKey(keyID, model)
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	queue := s.prunePendingLocked(s.pendingPicks[key], now)
	queue = append(queue, pendingPick{route: route, at: now})
	if len(queue) > pendingPickMaxQueue {
		queue = queue[len(queue)-pendingPickMaxQueue:]
	}
	s.pendingPicks[key] = queue
}

func (s *Store) takePick(keyID, model string) (ResolvedModelRoute, bool) {
	if strings.TrimSpace(keyID) == "" || strings.TrimSpace(model) == "" {
		return ResolvedModelRoute{}, false
	}
	key := pendingPickKey(keyID, model)
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	queue := s.prunePendingLocked(s.pendingPicks[key], now)
	if len(queue) == 0 {
		delete(s.pendingPicks, key)
		return ResolvedModelRoute{}, false
	}
	pick := queue[0]
	queue = queue[1:]
	if len(queue) == 0 {
		delete(s.pendingPicks, key)
	} else {
		s.pendingPicks[key] = queue
	}
	return pick.route, true
}

func (s *Store) prunePendingLocked(queue []pendingPick, now time.Time) []pendingPick {
	index := 0
	for index < len(queue) && now.Sub(queue[index].at) > pendingPickTTL {
		index++
	}
	if index >= len(queue) {
		return nil
	}
	return queue[index:]
}

func (s *Store) ResponseModel(headers http.Header, query map[string][]string, requested string) (string, bool) {
	if !s.Enabled() {
		return "", false
	}
	key := s.findBySecret(ExtractAPIKey(headers, query))
	if key == nil || !key.Enabled {
		return "", false
	}
	model, ok := s.modelForKey(key, requested)
	if !ok {
		return "", false
	}
	return model.Name, true
}
