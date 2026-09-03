package plugin

import (
	"context"
	"errors"
	"net/http"
	"time"

	"cpa-key-policy/internal/policy"
)

type pricingPreviewRequest struct {
	Models []string `json:"models"`
}

func (a *App) previewModelPrices(raw []byte) ManagementResponse {
	var req pricingPreviewRequest
	if err := decodeStrictBody(raw, &req); err != nil {
		return jsonError(http.StatusBadRequest, "bad_request", err.Error())
	}
	if req.Models == nil {
		req.Models = []string{}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	preview, err := a.pricing.Preview(ctx, req.Models)
	if err != nil {
		return jsonError(http.StatusBadGateway, "pricing_unavailable", err.Error())
	}
	return jsonResponse(http.StatusOK, preview)
}

type modelImportRequest struct {
	DryRun bool                     `json:"dry_run"`
	Items  []policy.ModelImportItem `json:"items"`
}

func (a *App) importModels(raw []byte) ManagementResponse {
	var req modelImportRequest
	if err := decodeStrictBody(raw, &req); err != nil {
		return jsonError(http.StatusBadRequest, "bad_request", err.Error())
	}
	if req.Items == nil {
		req.Items = []policy.ModelImportItem{}
	}
	result, err := a.store.ImportModels(req.Items, req.DryRun)
	if err != nil {
		if errors.Is(err, policy.ErrInvalidModelImport) {
			return jsonError(http.StatusBadRequest, "validation_error", err.Error())
		}
		return jsonError(http.StatusInternalServerError, "import_failed", err.Error())
	}
	return jsonResponse(http.StatusOK, result)
}
