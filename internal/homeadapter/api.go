package homeadapter

import (
	"crypto/subtle"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"strings"
	"time"
)

// BasePath is where cpa-key-policy served its management API on CPA; keeping
// it lets existing callers switch hosts without changing paths.
const BasePath = "/v0/management/plugins/cpa-key-policy"

// API serves the subset of the cpa-key-policy management contract that its
// callers use. Everything else answers 404, as an unknown route did before.
type API struct {
	svc    *Service
	tokens [][]byte
	log    *log.Logger
}

func NewAPI(svc *Service, tokens []string, logger *log.Logger) *API {
	api := &API{svc: svc, log: logger}
	for _, token := range tokens {
		if token = strings.TrimSpace(token); token != "" {
			api.tokens = append(api.tokens, []byte(token))
		}
	}
	return api
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(status int) {
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

func (a *API) ServeHTTP(rw http.ResponseWriter, r *http.Request) {
	started := time.Now()
	w := &statusWriter{ResponseWriter: rw, status: http.StatusOK}
	a.route(w, r)
	a.log.Printf("%s %s %d %s from=%s", r.Method, r.URL.Path, w.status, time.Since(started).Round(time.Millisecond), clientAddr(r))
}

func (a *API) route(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimRight(r.URL.Path, "/")
	if path == "/healthz" {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
		return
	}
	if !strings.HasPrefix(path, BasePath+"/") {
		writeError(w, &ContractError{http.StatusNotFound, "not_found", "unknown route"})
		return
	}
	if !a.authorized(r) {
		writeError(w, &ContractError{http.StatusUnauthorized, "unauthorized", "invalid management key"})
		return
	}
	ctx := r.Context()
	query := r.URL.Query()
	switch route := strings.TrimPrefix(path, BasePath); {
	case r.Method == http.MethodGet && route == "/keys":
		keys, err := a.svc.List(ctx)
		reply(w, http.StatusOK, map[string]any{"keys": keys}, err)
	case r.Method == http.MethodPost && route == "/keys":
		var body KeyWrite
		if !decode(w, r, &body) {
			return
		}
		key, err := a.svc.Upsert(ctx, body, 0)
		reply(w, http.StatusCreated, map[string]any{"key": key, "generated": false}, err)
	case r.Method == http.MethodPatch && route == "/keys":
		var body KeyWrite
		if !decode(w, r, &body) {
			return
		}
		key, err := a.svc.Patch(ctx, body)
		reply(w, http.StatusOK, map[string]any{"key": key}, err)
	case r.Method == http.MethodDelete && route == "/keys":
		id := strings.TrimSpace(query.Get("id"))
		reply(w, http.StatusOK, map[string]any{"deleted": id}, a.svc.Delete(ctx, id))
	case r.Method == http.MethodGet && route == "/keys/usage":
		usage, err := a.svc.Usage(ctx, strings.TrimSpace(query.Get("id")))
		reply(w, http.StatusOK, usage, err)
	case r.Method == http.MethodPost && route == "/keys/reset-usage":
		var body struct {
			ID     string `json:"id"`
			Window string `json:"window"`
		}
		if !decode(w, r, &body) {
			return
		}
		result, err := a.svc.ResetUsage(ctx, strings.TrimSpace(body.ID), strings.TrimSpace(body.Window))
		reply(w, http.StatusOK, result, err)
	case r.Method == http.MethodGet && route == "/models":
		models, err := a.svc.Models(ctx)
		reply(w, http.StatusOK, map[string]any{"models": models}, err)
	default:
		writeError(w, &ContractError{http.StatusNotFound, "not_found", "route not available in Home mode"})
	}
}

// authorized accepts the same headers CPA's management API accepted.
func (a *API) authorized(r *http.Request) bool {
	provided := strings.TrimSpace(r.Header.Get("X-Management-Key"))
	if header := r.Header.Get("Authorization"); header != "" {
		provided = strings.TrimSpace(header)
		if scheme, token, ok := strings.Cut(provided, " "); ok && strings.EqualFold(scheme, "bearer") {
			provided = strings.TrimSpace(token)
		}
	}
	if provided == "" {
		return false
	}
	for _, token := range a.tokens {
		if subtle.ConstantTimeCompare([]byte(provided), token) == 1 {
			return true
		}
	}
	return false
}

func decode(w http.ResponseWriter, r *http.Request, target any) bool {
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(target); err != nil {
		writeError(w, invalid("invalid JSON body: %v", err))
		return false
	}
	return true
}

func reply(w http.ResponseWriter, status int, payload any, err error) {
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, status, payload)
}

func writeError(w http.ResponseWriter, err error) {
	contract := asContractError(err)
	writeJSON(w, contract.Status, map[string]any{"error": map[string]string{"code": contract.Code, "message": contract.Message}})
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func clientAddr(r *http.Request) string {
	if forwarded := r.Header.Get("X-Forwarded-For"); forwarded != "" {
		first, _, _ := strings.Cut(forwarded, ",")
		return strings.TrimSpace(first)
	}
	return r.RemoteAddr
}
