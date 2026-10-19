package policy

import (
	"net/http"
	"strings"
	"time"
)

// Denial explains why an identified key may not send a request right now.
type Denial struct {
	// Reason is rpm_exceeded, daily_exceeded, weekly_exceeded or monthly_exceeded.
	Reason string
	// Limit is the RPM or USD limit that was reached.
	Limit float64
	// RetryAt is the earliest time the same request can be admitted.
	RetryAt  time.Time
	Timezone string
}

type AuthDecision struct {
	Known     bool
	Allowed   bool
	KeyID     string
	Principal string
	Reason    string
	ModelList bool
	// Denial explains an RPM or quota rejection made during authentication.
	Denial *Denial
	// Deferred means RPM and quota are left to the request interceptor, which
	// can answer with 429 instead of the host's generic 401.
	Deferred bool
}

// Authenticate identifies the key and, where CPA cannot reach the request
// interceptor, admits the request here. The model list is always readable.
func (s *Store) Authenticate(method, path string, headers http.Header, query map[string][]string) AuthDecision {
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
		decision.Allowed = true
		decision.Reason = "models_endpoint"
		return decision
	}
	switch admissionStageFor(method, path, headers) {
	case admitAtInterceptor:
		decision.Deferred = true
	case admitQuotaAtAuth:
		if denial := s.admit(key, path, false); denial != nil {
			decision.Denial = denial
			decision.Reason = denial.Reason
			return decision
		}
	default:
		if isMediaGenerationPath(path) && ExtractAPIKey(headers, nil) == "" {
			// Per-call billing needs the trace id the interceptor records, and
			// the interceptor cannot see a key sent only in the query string.
			decision.Reason = "media_generation_requires_header_key"
			return decision
		}
		if strings.EqualFold(strings.TrimSpace(method), http.MethodGet) &&
			websocketResponsePaths[strings.TrimRight(strings.TrimSpace(path), "/")] &&
			ExtractAPIKey(headers, nil) == "" {
			// Each turn of the socket is admitted by the request interceptor,
			// which gets the handshake headers and no query, so a query key
			// would be checked once and never again.
			decision.Reason = "websocket_requires_header_key"
			return decision
		}
		if denial := s.admit(key, path, true); denial != nil {
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
// request CPA is about to send upstream. On a media generation route the
// admitted request's trace id is remembered so its usage record can be
// billed per call. Requests whose key is not in the headers were already
// admitted during authentication.
func (s *Store) AdmitIntercepted(headers http.Header, requestPath, traceID string) *Denial {
	s.lifecycleMu.RLock()
	defer s.lifecycleMu.RUnlock()
	if !s.Enabled() {
		return nil
	}
	key := s.findBySecret(ExtractAPIKey(headers, nil))
	if key == nil || !key.Enabled {
		return nil
	}
	if denial := s.admit(key, requestPath, true); denial != nil {
		return denial
	}
	if traceID != "" && isMediaGenerationPath(requestPath) {
		s.mu.RLock()
		traces := s.mediaTraces
		s.mu.RUnlock()
		traces.remember(traceID)
		s.journalTrace(traceID, time.Now())
	}
	return nil
}

// admit is the single admission point: RPM, then quota. Retrieving an
// already generated video is never blocked by quota.
func (s *Store) admit(key *KeyConfig, path string, countRPM bool) *Denial {
	limiter, ledger := s.runtimeComponents()
	if countRPM {
		if allowed, retryAt := limiter.Allow(key.ID, key.RPM); !allowed {
			return &Denial{Reason: "rpm_exceeded", Limit: float64(key.RPM), RetryAt: retryAt}
		}
	}
	if isMediaResultPath(path) {
		return nil
	}
	return ledger.quotaDenial(key.ID, quotaLimitsForKey(*key))
}
