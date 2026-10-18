package plugin

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

// The bodies below are what the two external writers send today, verbatim:
// loboo-mono (packages/domain-account/src/cpa.ts, policyFields) and
// reeka-ai-install (main.go, cpaKeyWriteBody). Both still carry models and
// allow_models_endpoint, which must be accepted and ignored.
const (
	lobooMonoCreateBody = `{"id":"ak-1","key":"cpa_loboo_ak_1","name":"Alice","enabled":true,"models":[{"name":"gpt-5.6"},{"name":"gpt-6-sol"}],"weekly_limit_usd":25,"daily_limit_usd":0,"monthly_limit_usd":0,"allow_models_endpoint":true}`
	lobooMonoPatchBody  = `{"id":"ak-1","enabled":false,"models":[{"name":"gpt-5.6"}],"weekly_limit_usd":30,"daily_limit_usd":0,"monthly_limit_usd":0,"allow_models_endpoint":true}`
	reekaCreateBody     = `{"id":"abc123","name":"abc123","key":"sk-reeka-abc123","enabled":true,"rpm":0,"models":[{"name":"gpt-6-luna","daily_limit_usd":0},{"name":"gpt-5.6-terra","daily_limit_usd":0},{"name":"gpt-image-2","daily_limit_usd":0}],"daily_limit_usd":0,"weekly_limit_usd":200,"monthly_limit_usd":0,"allow_models_endpoint":true}`
)

func TestExternalWritersPayloadsAreAccepted(t *testing.T) {
	app := newTestApp(t, appOptions{})
	created := managementCall(t, app, http.MethodPost, "/keys", nil, []byte(lobooMonoCreateBody))
	if created.StatusCode != http.StatusCreated {
		t.Fatalf("loboo-mono create status=%d body=%s", created.StatusCode, created.Body)
	}
	var createdBody struct {
		Key struct {
			ID             string  `json:"id"`
			Name           string  `json:"name"`
			Enabled        bool    `json:"enabled"`
			WeeklyLimitUSD float64 `json:"weekly_limit_usd"`
		} `json:"key"`
		PlainKey string `json:"plain_key"`
	}
	decode(t, created.Body, &createdBody)
	if createdBody.Key.ID != "ak-1" || createdBody.Key.Name != "Alice" || !createdBody.Key.Enabled || createdBody.Key.WeeklyLimitUSD != 25 || createdBody.PlainKey != "cpa_loboo_ak_1" {
		t.Fatalf("loboo-mono create body = %s", created.Body)
	}
	for _, ignored := range []string{"models", "allow_models_endpoint"} {
		if strings.Contains(string(created.Body), `"`+ignored+`"`) {
			t.Fatalf("ignored field %q echoed back: %s", ignored, created.Body)
		}
	}

	patched := managementCall(t, app, http.MethodPatch, "/keys", nil, []byte(lobooMonoPatchBody))
	if patched.StatusCode != http.StatusOK {
		t.Fatalf("loboo-mono patch status=%d body=%s", patched.StatusCode, patched.Body)
	}
	var patchedBody struct {
		Key struct {
			Enabled        bool    `json:"enabled"`
			WeeklyLimitUSD float64 `json:"weekly_limit_usd"`
		} `json:"key"`
	}
	decode(t, patched.Body, &patchedBody)
	if patchedBody.Key.Enabled || patchedBody.Key.WeeklyLimitUSD != 30 {
		t.Fatalf("loboo-mono patch body = %s", patched.Body)
	}

	reeka := managementCall(t, app, http.MethodPost, "/keys", nil, []byte(reekaCreateBody))
	if reeka.StatusCode != http.StatusCreated {
		t.Fatalf("reeka-ai-install create status=%d body=%s", reeka.StatusCode, reeka.Body)
	}
	decode(t, reeka.Body, &createdBody)
	if createdBody.Key.ID != "abc123" || createdBody.Key.WeeklyLimitUSD != 200 || createdBody.PlainKey != "sk-reeka-abc123" {
		t.Fatalf("reeka-ai-install create body = %s", reeka.Body)
	}
	// Every enabled key may use every model; the ignored lists change nothing.
	if response := authenticate(t, app, http.MethodPost, "/v1/chat/completions", bearer("sk-reeka-abc123"), nil); !response.Authenticated {
		t.Fatalf("reeka key denied: %+v", response)
	}
	if response := authenticate(t, app, http.MethodGet, "/v1/models", bearer("sk-reeka-abc123"), nil); !response.Authenticated {
		t.Fatalf("reeka key cannot list models: %+v", response)
	}
}

func TestKeyWritesStillRejectOtherUnknownFields(t *testing.T) {
	app := newTestApp(t, appOptions{})
	for name, body := range map[string]string{
		"routing":           `{"id":"x","target_model":"gpt"}`,
		"limits_changed_at": `{"id":"x","limits_changed_at":"2026-01-01T00:00:00Z"}`,
		"price override":    `{"id":"x","input_price_per_million":1}`,
		"typo":              `{"id":"x","weekly_limit":1}`,
	} {
		for _, method := range []string{http.MethodPost, http.MethodPatch} {
			response := managementCall(t, app, method, "/keys", nil, []byte(body))
			if response.StatusCode != http.StatusBadRequest || errorCode(t, response) != "invalid_json" {
				t.Fatalf("%s %s: status=%d body=%s", method, name, response.StatusCode, response.Body)
			}
		}
	}
}

// keeperPublicKey and keeperKeysResponse are copied from cpa-usage-keeper
// (internal/cpa/dto/cpaapikeys/key_policy_keys.go).
type keeperPublicKey struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Enabled    *bool  `json:"enabled"`
	KeyPreview string `json:"key_preview"`
}

type keeperKeysResponse struct {
	Keys []keeperPublicKey `json:"keys"`
}

// adminAccount, adminLedger and adminCycle are copied from loboo-admin
// (internal/cpausage/client.go).
type adminLedger struct {
	Daily        *float64     `json:"daily_usd"`
	Weekly       *float64     `json:"weekly_usd"`
	Monthly      *float64     `json:"monthly_usd"`
	DailyLimit   *float64     `json:"daily_limit_usd"`
	WeeklyLimit  *float64     `json:"weekly_limit_usd"`
	MonthlyLimit *float64     `json:"monthly_limit_usd"`
	Timezone     string       `json:"timezone"`
	Boundary     time.Time    `json:"next_accounting_boundary_at"`
	Cycles       []adminCycle `json:"cycles"`
}

type adminCycle struct {
	Window    string    `json:"window"`
	StartedAt time.Time `json:"started_at"`
	ResetsAt  time.Time `json:"resets_at"`
}

type adminAccount struct {
	ID      string      `json:"id"`
	Enabled *bool       `json:"enabled"`
	KeyHash string      `json:"key_hash"`
	Usage   adminLedger `json:"usage"`
}

func TestExternalReadersDecodeKeysCatalog(t *testing.T) {
	app := newTestApp(t, appOptions{Clock: testClock(t)})
	managementOK(t, app, http.MethodPatch, "/keys", nil, []byte(`{"id":"priced","weekly_limit_usd":25}`))
	recordUsage(t, app, UsageHandleRequest{Provider: "codex", Model: "gpt-5-codex", APIKey: testKeyID, Detail: UsageDetail{InputTokens: 200_000, OutputTokens: 100_000, TotalTokens: 300_000}})
	body := managementOK(t, app, http.MethodGet, "/keys", nil, nil)

	var keeper keeperKeysResponse
	if err := json.Unmarshal(body, &keeper); err != nil {
		t.Fatalf("cpa-usage-keeper decode: %v", err)
	}
	if len(keeper.Keys) != 1 || keeper.Keys[0].ID != testKeyID || keeper.Keys[0].Name != "Priced" || keeper.Keys[0].Enabled == nil || !*keeper.Keys[0].Enabled || keeper.Keys[0].KeyPreview != "cpa_pr...ced" {
		t.Fatalf("cpa-usage-keeper view = %+v", keeper.Keys)
	}

	var admin struct {
		Keys []adminAccount `json:"keys"`
	}
	if err := json.Unmarshal(body, &admin); err != nil {
		t.Fatalf("loboo-admin decode: %v", err)
	}
	account := admin.Keys[0]
	usage := account.Usage
	if account.ID != testKeyID || account.Enabled == nil || usage.Daily == nil || !nearly(*usage.Daily, 0.30) || usage.Weekly == nil || !nearly(*usage.Weekly, 0.30) || usage.WeeklyLimit == nil || *usage.WeeklyLimit != 25 || usage.Timezone != "Asia/Shanghai" {
		t.Fatalf("loboo-admin view = %+v", account)
	}
	boundary := time.Date(2026, 10, 21, 0, 0, 0, 0, time.FixedZone("CST", 8*3600))
	if !usage.Boundary.Equal(boundary) || len(usage.Cycles) != 3 || usage.Cycles[0].Window != "daily" || !usage.Cycles[0].ResetsAt.Equal(boundary) || usage.Cycles[1].Window != "weekly" || usage.Cycles[2].Window != "monthly" || usage.Cycles[1].StartedAt.IsZero() {
		t.Fatalf("loboo-admin cycles = %+v (boundary %s)", usage.Cycles, usage.Boundary)
	}
}
