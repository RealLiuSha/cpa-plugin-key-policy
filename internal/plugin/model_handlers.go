package plugin

import (
	"errors"
	"net/http"
	"strings"

	"cpa-key-policy/internal/policy"
)

type modelUpsertRequest struct {
	Name                     string               `json:"name"`
	Targets                  []policy.ModelTarget `json:"targets"`
	Dispatch                 string               `json:"dispatch"`
	BillingMode              string               `json:"billing_mode"`
	Free                     bool                 `json:"free"`
	InputPricePerMillion     float64              `json:"input_price_per_million"`
	OutputPricePerMillion    float64              `json:"output_price_per_million"`
	CacheReadPricePerMillion float64              `json:"cache_read_price_per_million"`
	PerCallUSD               float64              `json:"per_call_usd"`
}

func (a *App) upsertModel(raw []byte) ManagementResponse {
	var req modelUpsertRequest
	if err := decodeStrictBody(raw, &req); err != nil {
		return jsonError(http.StatusBadRequest, "bad_request", err.Error())
	}
	model := policy.ModelDefinition{
		Name:                     req.Name,
		Targets:                  req.Targets,
		Dispatch:                 req.Dispatch,
		BillingMode:              req.BillingMode,
		Free:                     req.Free,
		InputPricePerMillion:     req.InputPricePerMillion,
		OutputPricePerMillion:    req.OutputPricePerMillion,
		CacheReadPricePerMillion: req.CacheReadPricePerMillion,
		PerCallUSD:               req.PerCallUSD,
	}
	if err := a.store.UpsertModel(model); err != nil {
		return jsonError(http.StatusBadRequest, "validation_error", err.Error())
	}
	for _, stored := range a.store.ModelsSnapshot() {
		if strings.EqualFold(stored.Name, strings.TrimSpace(model.Name)) {
			return jsonResponse(http.StatusOK, map[string]any{"model": stored})
		}
	}
	return jsonError(http.StatusInternalServerError, "model_publish_failed", "saved model is missing from the runtime index")
}

type modelDeleteRequest struct {
	Name string `json:"name"`
}

func (a *App) deleteModel(raw []byte) ManagementResponse {
	var req modelDeleteRequest
	if err := decodeStrictBody(raw, &req); err != nil {
		return jsonError(http.StatusBadRequest, "bad_request", err.Error())
	}
	if err := a.store.DeleteModel(req.Name); err != nil {
		return jsonError(http.StatusBadRequest, "delete_failed", err.Error())
	}
	return jsonResponse(http.StatusOK, map[string]any{"deleted": true})
}

type importPricesRequest struct {
	DryRun  bool                      `json:"dry_run"`
	Matches []policy.PriceImportMatch `json:"matches"`
}

func (a *App) importModelPrices(raw []byte) ManagementResponse {
	var req importPricesRequest
	if err := decodeStrictBody(raw, &req); err != nil {
		return jsonError(http.StatusBadRequest, "bad_request", err.Error())
	}
	if req.Matches == nil {
		req.Matches = []policy.PriceImportMatch{}
	}
	result, err := a.store.ImportModelPrices(req.Matches, req.DryRun)
	if err != nil {
		if errors.Is(err, policy.ErrInvalidModelPriceImport) {
			return jsonError(http.StatusBadRequest, "validation_error", err.Error())
		}
		return jsonError(http.StatusInternalServerError, "import_failed", err.Error())
	}
	return jsonResponse(http.StatusOK, result)
}
