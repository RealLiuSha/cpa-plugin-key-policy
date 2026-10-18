package plugin

import (
	"context"
	"errors"
	"net/http"
	"time"

	"cpa-key-policy/internal/policy"
)

// listModels answers GET /models: the price table and the models that were
// billed without any price row in the retention window.
func (a *App) listModels() ManagementResponse {
	return jsonResponse(http.StatusOK, map[string]any{
		"models":   a.store.PriceRows(),
		"unpriced": a.store.UnpricedModels(),
	})
}

// upsertModel answers POST /models. The body is a price row; provider is an
// optional qualifier and an all-zero row is allowed (it bills $0, unpriced).
func (a *App) upsertModel(raw []byte) ManagementResponse {
	var price policy.ModelPrice
	if err := decodeStrictBody(raw, &price); err != nil {
		return jsonError(http.StatusBadRequest, "bad_request", err.Error())
	}
	if err := a.store.UpsertModel(price); err != nil {
		if errors.Is(err, policy.ErrStatePersist) {
			return persistError(err)
		}
		return jsonError(http.StatusBadRequest, "validation_error", err.Error())
	}
	row, ok := a.store.PriceRowFor(price.Name, price.Provider)
	if !ok {
		return jsonError(http.StatusInternalServerError, "model_publish_failed", "saved price row is missing from the runtime table")
	}
	return jsonResponse(http.StatusOK, map[string]any{"model": row})
}

type modelDeleteRequest struct {
	Name     string `json:"name"`
	Provider string `json:"provider"`
}

func (a *App) deleteModel(raw []byte) ManagementResponse {
	var req modelDeleteRequest
	if err := decodeStrictBody(raw, &req); err != nil {
		return jsonError(http.StatusBadRequest, "bad_request", err.Error())
	}
	if err := a.store.DeleteModel(req.Name, req.Provider); err != nil {
		switch {
		case errors.Is(err, policy.ErrUnknownModel):
			return jsonError(http.StatusNotFound, "not_found", err.Error())
		case errors.Is(err, policy.ErrStatePersist):
			return persistError(err)
		default:
			return jsonError(http.StatusBadRequest, "delete_failed", err.Error())
		}
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
		switch {
		case errors.Is(err, policy.ErrInvalidModelPriceImport):
			return jsonError(http.StatusBadRequest, "validation_error", err.Error())
		case errors.Is(err, policy.ErrStatePersist):
			return persistError(err)
		default:
			return jsonError(http.StatusInternalServerError, "import_failed", err.Error())
		}
	}
	return jsonResponse(http.StatusOK, result)
}

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
	Items []policy.ModelImportItem `json:"items"`
}

// importModels answers POST /models/import: $0 rows for upstream model ids
// that have no row yet; existing rows are reported as skipped.
func (a *App) importModels(raw []byte) ManagementResponse {
	var req modelImportRequest
	if err := decodeStrictBody(raw, &req); err != nil {
		return jsonError(http.StatusBadRequest, "bad_request", err.Error())
	}
	result, err := a.store.ImportModels(req.Items)
	if err != nil {
		switch {
		case errors.Is(err, policy.ErrInvalidModelImport):
			return jsonError(http.StatusBadRequest, "validation_error", err.Error())
		case errors.Is(err, policy.ErrStatePersist):
			return persistError(err)
		default:
			return jsonError(http.StatusInternalServerError, "import_failed", err.Error())
		}
	}
	return jsonResponse(http.StatusOK, result)
}
