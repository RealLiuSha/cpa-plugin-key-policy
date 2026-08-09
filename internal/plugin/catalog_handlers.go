package plugin

import (
	"net/http"

	"cpa-key-policy/internal/policy"
)

type catalogRequest struct {
	Credentials []policy.CatalogCredential `json:"credentials"`
	Rules       []policy.ClassifyRule      `json:"rules,omitempty"`
}

type catalogResponse struct {
	Entries []policy.CatalogEntry `json:"entries"`
}

func (a *App) buildCatalog(raw []byte) ManagementResponse {
	var req catalogRequest
	if err := decodeStrictBody(raw, &req); err != nil {
		return jsonError(http.StatusBadRequest, "bad_request", err.Error())
	}
	rules := req.Rules
	if len(rules) == 0 {
		rules = a.store.ClassifyRulesSnapshot()
	}
	entries := policy.BuildCatalogEntries(req.Credentials, rules)
	if entries == nil {
		entries = []policy.CatalogEntry{}
	}
	return jsonResponse(http.StatusOK, catalogResponse{Entries: entries})
}
