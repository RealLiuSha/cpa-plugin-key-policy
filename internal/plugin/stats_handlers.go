package plugin

import (
	"errors"
	"fmt"
	"net/http"

	"cpa-key-policy/internal/policy"
)

// stats answers GET /stats?days= with the cross-key report for the last days.
func (a *App) stats(days int) ManagementResponse {
	if days < 1 || days > policy.UsageRetentionDays {
		return jsonError(http.StatusBadRequest, "invalid_days", fmt.Sprintf("days must be between 1 and %d", policy.UsageRetentionDays))
	}
	return jsonResponse(http.StatusOK, a.store.Stats(days))
}

// statsExport answers GET /stats/export?from=&to= with a CSV attachment. A
// range over 92 days or 200000 rows is refused with 413 range_too_large.
func (a *App) statsExport(from, to string) ManagementResponse {
	body, err := a.store.StatsCSV(from, to)
	switch {
	case errors.Is(err, policy.ErrRangeTooLarge):
		return jsonError(http.StatusRequestEntityTooLarge, "range_too_large", err.Error())
	case errors.Is(err, policy.ErrInvalidDateRange):
		return jsonError(http.StatusBadRequest, "invalid_range", err.Error())
	case err != nil:
		return jsonError(http.StatusInternalServerError, "export_failed", err.Error())
	}
	return ManagementResponse{
		StatusCode: http.StatusOK,
		Headers: http.Header{
			"Content-Type":        []string{"text/csv; charset=utf-8"},
			"Content-Disposition": []string{"attachment; filename=" + policy.StatsCSVFilename(from, to)},
		},
		Body: body,
	}
}
