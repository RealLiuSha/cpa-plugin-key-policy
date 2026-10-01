package policy

import (
	"net/http"
	"strings"
	"time"
)

// Denial explains why an identified key may not send a request right now.
type Denial struct {
	// Reason is rpm_exceeded, daily_exceeded, weekly_exceeded,
	// monthly_exceeded or model_daily_exceeded.
	Reason string
	// Model is the public model whose own daily limit was reached.
	Model string
	// Limit is the RPM or USD limit that was reached.
	Limit float64
	// RetryAt is the earliest time the same request can be admitted.
	RetryAt  time.Time
	Timezone string
}

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
		model, ok := s.modelForKey(key, requested)
		if !ok {
			decision.Reason = "model_not_allowed"
			return decision
		}
		decision.Model = model
	}
	switch admissionStageFor(method, path, headers) {
	case admitAtInterceptor:
		decision.Deferred = true
	case admitQuotaAtAuth:
		if denial := s.admit(key, decision.Model, path, false); denial != nil {
			decision.Denial = denial
			decision.Reason = denial.Reason
			return decision
		}
	default:
		if denial := s.admit(key, decision.Model, path, true); denial != nil {
			decision.Denial = denial
			decision.Reason = denial.Reason
			return decision
		}
	}
	decision.Allowed = true
	decision.Reason = "allowed"
	return decision
}

// AdmitIntercepted runs the admission that authentication deferred, for a
// request CPA is about to send upstream. Requests whose key is not in the
// headers were already admitted during authentication.
func (s *Store) AdmitIntercepted(headers http.Header, requestedModel, requestPath string) *Denial {
	s.lifecycleMu.RLock()
	defer s.lifecycleMu.RUnlock()
	if !s.Enabled() {
		return nil
	}
	key := s.findBySecret(ExtractAPIKey(headers, nil))
	if key == nil || !key.Enabled {
		return nil
	}
	model, _ := s.modelForKey(key, requestedModel)
	return s.admit(key, model, requestPath, true)
}

// admit is the single admission point: RPM, then quota, then the per-call
// precharge for media generation, which CPA may not report usage for.
// Retrieving an already generated video is never blocked by quota.
func (s *Store) admit(key *KeyConfig, model ModelDefinition, path string, countRPM bool) *Denial {
	limiter, ledger := s.runtimeComponents()
	if countRPM && limiter != nil {
		if allowed, retryAt := limiter.Allow(key.ID, key.RPM); !allowed {
			return &Denial{Reason: "rpm_exceeded", Limit: float64(key.RPM), RetryAt: retryAt}
		}
	}
	if ledger != nil && !isMediaResultPath(path) {
		if denial := ledger.quotaDenial(key.ID, model.Name, quotaLimitsForKey(*key)); denial != nil {
			return denial
		}
	}
	if model.BillingMode == "per_call" && isMediaGenerationPath(path) {
		s.recordUsage(key.ID, model.Name, model.TargetModel, false, UsageDetail{}, false)
		s.rememberPrecharge(key.ID, model.Name)
	}
	return nil
}

// Route resolves the public model a key may use to its upstream target.
func (s *Store) Route(headers http.Header, query map[string][]string, requested string) (ModelDefinition, string, bool) {
	if !s.Enabled() {
		return ModelDefinition{}, "", false
	}
	key := s.findBySecret(ExtractAPIKey(headers, query))
	if key == nil || !key.Enabled {
		return ModelDefinition{}, "", false
	}
	model, ok := s.modelForKey(key, requested)
	return model, key.ID, ok
}

func (s *Store) clearPrechargesForKeyLocked(keyID string) {
	prefix := strings.ToLower(strings.TrimSpace(keyID)) + "\x00"
	if prefix == "\x00" {
		return
	}
	for key := range s.precharges {
		if strings.HasPrefix(key, prefix) {
			delete(s.precharges, key)
		}
	}
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
