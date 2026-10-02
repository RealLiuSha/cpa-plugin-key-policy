package plugin

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"cpa-key-policy/internal/policy"
)

// rejectionResponse turns an RPM or quota denial into the terminated request
// CPA writes to the client verbatim: 429, Retry-After, and an error body in the
// shape the caller's SDK parses for its protocol.
func rejectionResponse(sourceFormat string, denial *policy.Denial, now time.Time) RequestInterceptResponse {
	headers := http.Header{"Content-Type": []string{"application/json"}}
	if !denial.RetryAt.IsZero() {
		seconds := int(math.Ceil(denial.RetryAt.Sub(now).Seconds()))
		if seconds < 1 {
			seconds = 1
		}
		headers.Set("Retry-After", strconv.Itoa(seconds))
	}
	return RequestInterceptResponse{
		Terminate:       true,
		StatusCode:      http.StatusTooManyRequests,
		ResponseHeaders: headers,
		ResponseBody:    rejectionBody(sourceFormat, denial, denialMessage(denial, now)),
	}
}

func rejectionBody(sourceFormat string, denial *policy.Denial, message string) []byte {
	format := strings.ToLower(strings.TrimSpace(sourceFormat))
	var payload any
	switch {
	case strings.HasPrefix(format, "claude"):
		payload = map[string]any{"type": "error", "error": map[string]any{"type": "rate_limit_error", "message": message}}
	case strings.HasPrefix(format, "gemini"):
		payload = map[string]any{"error": map[string]any{"code": http.StatusTooManyRequests, "message": message, "status": "RESOURCE_EXHAUSTED"}}
	default:
		errorBody := map[string]any{"message": message, "type": "insufficient_quota", "code": "insufficient_quota"}
		switch {
		case denial.Reason == "rpm_exceeded":
			errorBody["type"], errorBody["code"] = "rate_limit_error", "rate_limit_exceeded"
		case format == "openai-response":
			// Codex shows fixed text for insufficient_quota but names the reset
			// time for usage_limit_reached with resets_at (Unix seconds). code
			// stays insufficient_quota for SDKs that branch on it.
			errorBody["type"] = "usage_limit_reached"
			if !denial.RetryAt.IsZero() {
				errorBody["resets_at"] = denial.RetryAt.Unix()
			}
		}
		payload = map[string]any{"error": errorBody}
	}
	body, _ := json.Marshal(payload)
	return body
}

func denialMessage(denial *policy.Denial, now time.Time) string {
	resetsAt := denial.RetryAt.Format("2006-01-02 15:04")
	if zone := strings.TrimSpace(denial.Timezone); zone != "" {
		resetsAt += " " + zone
	}
	switch denial.Reason {
	case "rpm_exceeded":
		wait := int(math.Ceil(denial.RetryAt.Sub(now).Seconds()))
		if wait < 1 {
			wait = 1
		}
		return fmt.Sprintf("Rate limit reached: this API key's limit is %d RPM. Retry in %ds.", int(denial.Limit), wait)
	case "daily_exceeded":
		return fmt.Sprintf("Daily quota of $%.2f for this API key is used up. It resets at %s.", denial.Limit, resetsAt)
	case "weekly_exceeded":
		return fmt.Sprintf("7-day quota of $%.2f for this API key is used up. It resets at %s.", denial.Limit, resetsAt)
	case "monthly_exceeded":
		return fmt.Sprintf("30-day quota of $%.2f for this API key is used up. It resets at %s.", denial.Limit, resetsAt)
	case "model_daily_exceeded":
		return fmt.Sprintf("Daily quota of $%.2f for model %s on this API key is used up. It resets at %s.", denial.Limit, denial.Model, resetsAt)
	default:
		return "This API key cannot send more requests right now."
	}
}
