package plugin

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"cpa-key-policy/internal/policy/audit"
	"cpa-key-policy/internal/quality"
)

// qualityLegacyFile is cpa-codex-candy-eval's state file, whose detection
// history the quality module imports once.
const qualityLegacyFile = "cpa-codex-candy-eval-state.json"

// qualityHost gives the quality module the host callbacks: payloads go out as
// JSON, envelopes are unwrapped, and an error envelope keeps its HTTP status.
type qualityHost struct{ app *App }

func (h qualityHost) Call(method string, payload any) (json.RawMessage, error) {
	if h.app.host == nil {
		return nil, errors.New("host callbacks are unavailable")
	}
	request, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	raw, callErr := h.app.host.Call(method, request)
	var env struct {
		OK     bool            `json:"ok"`
		Result json.RawMessage `json:"result"`
		Error  *EnvelopeError  `json:"error"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		if callErr != nil {
			return nil, callErr
		}
		return nil, fmt.Errorf("decode the %s answer: %w", method, err)
	}
	if !env.OK {
		if env.Error == nil {
			return nil, fmt.Errorf("%s failed", method)
		}
		return nil, &quality.HostError{Code: env.Error.Code, Message: env.Error.Message, HTTPStatus: env.Error.HTTPStatus}
	}
	return env.Result, nil
}

// handleQuality serves the detection routes under base+"/quality"; route is
// what follows that prefix.
func (a *App) handleQuality(req ManagementRequest, route string) ManagementResponse {
	set := a.modules.Load()
	if set == nil || set.quality == nil {
		return moduleUnavailable("quality")
	}
	service := set.quality
	switch {
	case req.Method == http.MethodGet && route == "":
		state, err := service.State()
		if err != nil {
			return qualityError(err)
		}
		return jsonResponse(http.StatusOK, state)
	case req.Method == http.MethodPost && route == "/run":
		var run quality.RunRequest
		if err := json.Unmarshal(req.Body, &run); err != nil {
			return jsonError(http.StatusBadRequest, "invalid_json", err.Error())
		}
		summary, err := service.Run(run)
		if err != nil {
			return qualityError(err)
		}
		a.store.RecordChange("run_quality", run.Test, qualityRunChanges(run, summary))
		return jsonResponse(http.StatusOK, summary)
	case req.Method == http.MethodPost && route == "/cancel":
		var cancel struct {
			Test    string   `json:"test"`
			AuthIDs []string `json:"auth_ids"`
			All     bool     `json:"all"`
		}
		if err := json.Unmarshal(req.Body, &cancel); err != nil {
			return jsonError(http.StatusBadRequest, "invalid_json", err.Error())
		}
		count, err := service.Cancel(cancel.Test, cancel.AuthIDs, cancel.All)
		if err != nil {
			return qualityError(err)
		}
		return jsonResponse(http.StatusOK, map[string]int{"cancelled": count})
	case req.Method == http.MethodGet && route == "/record":
		record, err := service.Record(req.Query.Get("test"), req.Query.Get("auth_id"), req.Query.Get("id"))
		if err != nil {
			return qualityError(err)
		}
		return jsonResponse(http.StatusOK, record)
	case req.Method == http.MethodDelete && route == "/results":
		test := strings.TrimSpace(req.Query.Get("test"))
		if err := service.Clear(test); err != nil {
			return qualityError(err)
		}
		a.store.RecordChange("clear_quality", test, nil)
		return jsonResponse(http.StatusOK, map[string]bool{"cleared": true})
	case req.Method == http.MethodPost && route == "/inspection":
		var inspect quality.InspectionRequest
		if err := json.Unmarshal(req.Body, &inspect); err != nil {
			return jsonError(http.StatusBadRequest, "invalid_json", err.Error())
		}
		summary, err := service.Inspect(inspect)
		if err != nil {
			return qualityError(err)
		}
		a.store.RecordChange("start_inspection", inspect.Test, map[string]audit.Change{
			"started": {To: summary.Started}, "include": {To: inspect.Include}, "exclude": {To: inspect.Exclude},
		})
		return jsonResponse(http.StatusOK, summary)
	case req.Method == http.MethodGet && route == "/inspection":
		view, err := service.Inspection(req.Query.Get("test"))
		if err != nil {
			return qualityError(err)
		}
		return jsonResponse(http.StatusOK, view)
	default:
		return jsonError(http.StatusNotFound, "not_found", "unknown management route")
	}
}

// qualityAuditIDs is the most credential ids an audit line lists; a larger
// selection is recorded by its size.
const qualityAuditIDs = 10

// qualityRunChanges is what the audit records of a detection run: the
// credentials it was asked for and how many started. Runs spend the
// credentials' quota, like a try of an adapter rule.
func qualityRunChanges(run quality.RunRequest, summary quality.RunSummary) map[string]audit.Change {
	changes := map[string]audit.Change{"started": {To: summary.Started}}
	switch {
	case run.All:
		changes["all"] = audit.Change{To: true}
	case len(run.AuthIDs) > qualityAuditIDs:
		changes["auth_id_count"] = audit.Change{To: len(run.AuthIDs)}
	default:
		changes["auth_ids"] = audit.Change{To: run.AuthIDs}
	}
	return changes
}

func qualityError(err error) ManagementResponse {
	switch {
	case errors.Is(err, quality.ErrUnavailable):
		return upgrading()
	case errors.Is(err, quality.ErrInvalid):
		return jsonError(http.StatusBadRequest, "invalid_request", err.Error())
	case errors.Is(err, quality.ErrNotFound):
		return jsonError(http.StatusNotFound, "not_found", err.Error())
	case errors.Is(err, quality.ErrHost):
		return jsonError(http.StatusBadGateway, "host_unavailable", err.Error())
	case errors.Is(err, quality.ErrPersist):
		return jsonError(http.StatusInternalServerError, "persist_failed", err.Error())
	default:
		return jsonError(http.StatusInternalServerError, "internal_error", err.Error())
	}
}

// qualityRoutes are the detection routes CPA registers.
func qualityRoutes(base string) []ManagementRoute {
	base += "/quality"
	return []ManagementRoute{
		{Method: http.MethodGet, Path: base, Description: "CPA credentials with their detection progress and history."},
		{Method: http.MethodPost, Path: base + "/run", Description: "Run the candy, fingerprint, ModelTrace or pelican test on credentials (spends their quota)."},
		{Method: http.MethodPost, Path: base + "/cancel", Description: "Stop a running detection test."},
		{Method: http.MethodGet, Path: base + "/record", Description: "One candy, fingerprint, ModelTrace or pelican record in full."},
		{Method: http.MethodDelete, Path: base + "/results", Description: "Clear the history of one detection test or of all."},
		{Method: http.MethodPost, Path: base + "/inspection", Description: "Start a scheduled inspection on credentials selected by name patterns."},
		{Method: http.MethodGet, Path: base + "/inspection", Description: "The latest inspection of a test and each credential's verdict."},
	}
}
