package plugin

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"cpa-key-policy/internal/plugin/web"
	"cpa-key-policy/internal/policy"
	"cpa-key-policy/internal/pricingmetadata"
)

type App struct {
	store   *policy.Store
	pricing *pricingmetadata.Client
}

func NewApp() *App {
	return &App{store: policy.NewStore(), pricing: pricingmetadata.NewClient()}
}

func (a *App) SetPricingClient(client *pricingmetadata.Client) {
	if a == nil || client == nil {
		return
	}
	a.pricing = client
}

func (a *App) HandleMethod(method string, request []byte) ([]byte, error) {
	return safePluginCall(func() ([]byte, error) {
		return a.handleMethod(method, request)
	})
}

func (a *App) handleMethod(method string, request []byte) ([]byte, error) {
	switch method {
	case MethodPluginRegister, MethodPluginReconfigure:
		if err := a.configure(request); err != nil {
			return nil, err
		}
		return OKEnvelope(a.registration())
	case MethodFrontendAuthIdentifier:
		return OKEnvelope(IdentifierResponse{Identifier: PluginID})
	case MethodFrontendAuthAuthenticate:
		return a.authenticate(request)
	case MethodModelRoute:
		return a.routeModel(request)
	case MethodRequestInterceptBefore:
		return a.interceptRequest(request)
	case MethodRequestInterceptAfter:
		return OKEnvelope(RequestInterceptResponse{})
	case MethodResponseInterceptAfter:
		return a.interceptResponse(request)
	case MethodUsageHandle:
		return a.handleUsage(request)
	case MethodManagementRegister:
		return OKEnvelope(a.managementRegistration())
	case MethodManagementHandle:
		return a.handleManagement(request)
	default:
		return ErrorEnvelope("unknown_method", "unknown method: "+method, http.StatusNotFound), nil
	}
}

func safePluginCall(call func() ([]byte, error)) (response []byte, err error) {
	defer func() {
		if recover() != nil {
			response = nil
			err = errors.New("plugin panic recovered")
		}
	}()
	return call()
}

func (a *App) configure(raw []byte) error {
	var req LifecycleRequest
	if len(raw) > 0 {
		if err := decodeStrictBody(raw, &req); err != nil {
			return err
		}
	}
	if req.SchemaVersion < MinHostSchemaVersion {
		return fmt.Errorf("unsupported lifecycle schema_version %d; require >= %d", req.SchemaVersion, MinHostSchemaVersion)
	}
	configYAML, err := stripHostConfigMetadata(req.ConfigYAML)
	if err != nil {
		return err
	}
	cfg, err := policy.ParseConfig(configYAML)
	if err != nil {
		return err
	}
	if err := a.store.Configure(cfg); err != nil {
		return err
	}
	a.store.StartUsageFlusher()
	return nil
}

// Shutdown flushes usage. Host calls this on plugin unload.
func (a *App) Shutdown() {
	a.store.StopUsageFlusher()
}

func (a *App) registration() Registration {
	return Registration{
		SchemaVersion: SchemaVersion,
		Metadata: Metadata{
			Name:             PluginName,
			Version:          Version,
			Author:           "cpa-key-policy",
			GitHubRepository: "https://github.com/RealLiuSha/cpa-plugin-key-policy",
			ConfigFields: []ConfigField{
				{Name: "enabled", Type: "boolean", Description: "Enable or disable this plugin without unloading it."},
				{Name: "state_file", Type: "string", Description: "JSON state file used for key policy changes made through the Management API."},
				{Name: "usage_timezone", Type: "string", Description: "IANA timezone used for natural-day usage buckets. Defaults to Asia/Shanghai."},
				{Name: "keys", Type: "array", Description: "First-boot downstream key seeds. State is authoritative after initialization."},
				{Name: "models", Type: "array", Description: "First-boot public model seeds: name, provider, target_model and prices."},
			},
		},
		Capabilities: Capabilities{
			FrontendAuthProvider:          true,
			FrontendAuthProviderExclusive: false,
			ModelRouter:                   true,
			RequestInterceptor:            true,
			ResponseInterceptor:           true,
			UsagePlugin:                   true,
			ManagementAPI:                 true,
		},
	}
}

func (a *App) authenticate(raw []byte) ([]byte, error) {
	var req FrontendAuthRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	decision := a.store.Authenticate(req.Method, req.Path, req.Headers, req.Query, req.Body)
	if !decision.Known {
		return OKEnvelope(FrontendAuthResponse{Authenticated: false})
	}
	if !decision.Allowed {
		return OKEnvelope(FrontendAuthResponse{Authenticated: false})
	}
	meta := map[string]string{
		"provider":        PluginID,
		"key_id":          decision.KeyID,
		"requested_model": decision.Requested,
	}
	if decision.Model.Name != "" {
		meta["public_model"] = decision.Model.Name
		meta["target_provider"] = decision.Model.Provider
		meta["target_model"] = decision.Model.TargetModel
	}
	return OKEnvelope(FrontendAuthResponse{
		Authenticated: true,
		Principal:     decision.Principal,
		Metadata:      meta,
	})
}

// interceptRequest admits a request whose RPM and quota checks authentication
// deferred, terminating it with 429 when the key is over a limit.
func (a *App) interceptRequest(raw []byte) ([]byte, error) {
	var req RequestInterceptRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	requestPath, _ := req.Metadata["request_path"].(string)
	denial := a.store.AdmitIntercepted(req.Headers, req.RequestedModel, requestPath)
	if denial == nil {
		return OKEnvelope(RequestInterceptResponse{})
	}
	return OKEnvelope(rejectionResponse(req.SourceFormat, denial, time.Now()))
}

func (a *App) routeModel(raw []byte) ([]byte, error) {
	var req ModelRouteRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	model, keyID, ok := a.store.Route(req.Headers, req.Query, req.RequestedModel)
	if !ok {
		return OKEnvelope(ModelRouteResponse{Handled: false})
	}
	return OKEnvelope(ModelRouteResponse{
		Handled:     true,
		TargetKind:  "provider",
		Target:      resolveProviderKey(model.Provider, req.AvailableProviders),
		TargetModel: model.TargetModel,
		Reason:      "cpa-key-policy:" + keyID,
	})
}

// resolveProviderKey maps a routed provider to the provider key CPA's
// auth manager uses, so HasBuiltinProvider(target) succeeds.
//
// OpenAI-compatibility providers are registered with auth.Provider
// prefixed as "openai-compatible-<name>" (see synthesizer.config:
// auth.Provider = OpenAICompatibleProviderKey(name)). The plugin's
// ModelTarget.Provider carries the bare name (e.g. "nvidia",
// "opencode"). Returning the bare name makes CPA skip the router
// ("model router returned unavailable provider") and fall back to the
// native path, which fails for non-native public model names like "test1".
//
// We pick the key present in AvailableProviders, trying the bare name
// first (for built-in providers like codex/claude/gemini) then the
// openai-compatible- prefixed form. If neither matches we return the
// bare name and let CPA's availability check skip us (the native path
// still resolves true model names).
func resolveProviderKey(provider string, availableProviders []string) string {
	p := strings.ToLower(strings.TrimSpace(provider))
	if p == "" {
		return p
	}
	if len(availableProviders) == 0 {
		return p
	}
	for _, candidate := range []string{p, "openai-compatible-" + p} {
		for _, avail := range availableProviders {
			if strings.EqualFold(candidate, avail) {
				return candidate
			}
		}
	}
	return p
}

func (a *App) interceptResponse(raw []byte) ([]byte, error) {
	var req ResponseInterceptRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	// NOTE: billing is NOT done here. The host only invokes
	// response.intercept_after for non-streaming responses (the streaming path
	// goes through response.intercept_stream_chunk, which we don't handle).
	// Billing for both paths is centralized in usage.handle (handleUsage),
	// which the host fires after every request completes with already-parsed
	// token counts. Doing it here too would double-bill non-streaming requests.
	if req.Stream {
		// Streaming responses are not safe to rewrite (SSE framing) — return as-is.
		return OKEnvelope(ResponseInterceptResponse{})
	}
	publicModel, ok := a.store.ResponseModel(req.RequestHeaders, nil, req.RequestedModel)
	if !ok {
		return OKEnvelope(ResponseInterceptResponse{})
	}
	body, changed := policy.RewriteTopLevelModel(req.Body, publicModel)
	if !changed {
		return OKEnvelope(ResponseInterceptResponse{})
	}
	return OKEnvelope(ResponseInterceptResponse{Body: body})
}

func (a *App) handleUsage(raw []byte) ([]byte, error) {
	var req UsageHandleRequest
	// A malformed record must never break the request path: bill nothing.
	if err := json.Unmarshal(raw, &req); err != nil {
		return OKEnvelope(UsageHandleResponse{})
	}
	requestedModel := req.Alias
	if strings.TrimSpace(requestedModel) == "" {
		requestedModel = req.Model
	}
	_ = a.store.RecordUsage(req.APIKey, requestedModel, req.Model, req.Failed, policy.UsageDetail{
		InputTokens:         req.Detail.InputTokens,
		OutputTokens:        req.Detail.OutputTokens,
		ReasoningTokens:     req.Detail.ReasoningTokens,
		CachedTokens:        req.Detail.CachedTokens,
		CacheReadTokens:     req.Detail.CacheReadTokens,
		CacheCreationTokens: req.Detail.CacheCreationTokens,
		TotalTokens:         req.Detail.TotalTokens,
	})
	return OKEnvelope(UsageHandleResponse{})
}

func (a *App) managementRegistration() ManagementRegistrationResponse {
	base := "/plugins/" + PluginID
	return ManagementRegistrationResponse{
		Routes: []ManagementRoute{
			{Method: http.MethodGet, Path: base + "/keys", Description: "List downstream CPA key policies."},
			{Method: http.MethodPost, Path: base + "/keys", Description: "Create a downstream CPA key policy."},
			{Method: http.MethodPatch, Path: base + "/keys", Description: "Update a downstream CPA key policy by id."},
			{Method: http.MethodDelete, Path: base + "/keys", Description: "Delete a downstream CPA key policy by id."},
			{Method: http.MethodPost, Path: base + "/keys/rotate", Description: "Rotate one downstream CPA key by id."},
			{Method: http.MethodPost, Path: base + "/keys/reset-rpm", Description: "Reset one downstream CPA key RPM counter by id."},
			{Method: http.MethodPost, Path: base + "/keys/reset-usage", Description: "Reset one downstream CPA key daily, weekly, or monthly usage window by id."},
			{Method: http.MethodGet, Path: base + "/keys/usage", Description: "Per-model usage breakdown for one downstream CPA key by id."},
			{Method: http.MethodGet, Path: base + "/keys/history", Description: "Natural-day usage history for one downstream CPA key."},
			{Method: http.MethodGet, Path: base + "/audit", Description: "Read append-only management audit events."},
			{Method: http.MethodGet, Path: base + "/status", Description: "Show cpa-key-policy runtime status."},
			{Method: http.MethodGet, Path: base + "/models", Description: "List public model definitions."},
			{Method: http.MethodPost, Path: base + "/models", Description: "Create or update a public model definition."},
			{Method: http.MethodDelete, Path: base + "/models", Description: "Delete a public model definition by name."},
			{Method: http.MethodPost, Path: base + "/models/import-prices", Description: "Batch-import token prices for existing models (dry_run supported)."},
			{Method: http.MethodPost, Path: base + "/models/pricing-preview", Description: "Preview Models.dev prices for selected CPA models without writing state."},
			{Method: http.MethodPost, Path: base + "/models/import", Description: "Create public models for CPA capabilities at $0; existing names are skipped."},
		},
		Resources: []ResourceRoute{
			// TRADEOFF: host menu is static Chinese only, revisit when CPA supports locale maps
			{Path: web.IndexPath, Menu: "密钥策略", Description: "Web UI for managing downstream CPA key policies (create keys, pick models)."},
		},
	}
}

func (a *App) handleManagement(raw []byte) ([]byte, error) {
	var req ManagementRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	path := strings.TrimRight(req.Path, "/")

	// Plugin resource GETs (unauthenticated browser UI) are dispatched through
	// the same management.handle method by CPA's ServeResourceHTTP.
	resourcePrefix := "/v0/resource/plugins/" + PluginID
	if req.Method == http.MethodGet && strings.HasPrefix(path, resourcePrefix) {
		status, headers, body := web.Serve(strings.TrimPrefix(path, resourcePrefix))
		return OKEnvelope(ManagementResponse{StatusCode: status, Headers: headers, Body: body})
	}

	base := "/v0/management/plugins/" + PluginID
	switch {
	case req.Method == http.MethodGet && path == base+"/keys":
		return OKEnvelope(jsonResponse(http.StatusOK, map[string]any{"keys": a.publicKeys(a.store.Keys())}))
	case req.Method == http.MethodPost && path == base+"/keys":
		return OKEnvelope(a.createKey(req.Body))
	case req.Method == http.MethodPatch && path == base+"/keys":
		return OKEnvelope(a.patchKey(req.Body))
	case req.Method == http.MethodDelete && path == base+"/keys":
		return OKEnvelope(a.deleteKey(strings.TrimSpace(req.Query.Get("id"))))
	case req.Method == http.MethodPost && path == base+"/keys/rotate":
		id, err := idFromBody(req.Body)
		if err != nil {
			return OKEnvelope(jsonError(http.StatusBadRequest, "invalid_json", err.Error()))
		}
		return OKEnvelope(a.rotateKey(id))
	case req.Method == http.MethodPost && path == base+"/keys/reset-rpm":
		id, err := idFromBody(req.Body)
		if err != nil {
			return OKEnvelope(jsonError(http.StatusBadRequest, "invalid_json", err.Error()))
		}
		return OKEnvelope(a.resetRPM(id))
	case req.Method == http.MethodPost && path == base+"/keys/reset-usage":
		return OKEnvelope(a.resetUsage(req.Body))
	case req.Method == http.MethodGet && path == base+"/keys/usage":
		return OKEnvelope(a.keyUsage(strings.TrimSpace(req.Query.Get("id"))))
	case req.Method == http.MethodGet && path == base+"/keys/history":
		return OKEnvelope(a.keyHistory(strings.TrimSpace(req.Query.Get("id")), intQuery(req.Query, "days", 30)))
	case req.Method == http.MethodGet && path == base+"/audit":
		return OKEnvelope(a.auditEvents(strings.TrimSpace(req.Query.Get("key_id")), intQuery(req.Query, "limit", 100)))
	case req.Method == http.MethodGet && path == base+"/status":
		return OKEnvelope(jsonResponse(http.StatusOK, a.store.Status()))
	case req.Method == http.MethodGet && path == base+"/models":
		return OKEnvelope(jsonResponse(http.StatusOK, map[string]any{"models": a.store.ModelsSnapshotWithRefs()}))
	case req.Method == http.MethodPost && path == base+"/models":
		return OKEnvelope(a.upsertModel(req.Body))
	case req.Method == http.MethodDelete && path == base+"/models":
		return OKEnvelope(a.deleteModel(req.Body))
	case req.Method == http.MethodPost && path == base+"/models/import-prices":
		return OKEnvelope(a.importModelPrices(req.Body))
	case req.Method == http.MethodPost && path == base+"/models/pricing-preview":
		return OKEnvelope(a.previewModelPrices(req.Body))
	case req.Method == http.MethodPost && path == base+"/models/import":
		return OKEnvelope(a.importModels(req.Body))
	default:
		return OKEnvelope(jsonError(http.StatusNotFound, "not_found", "unknown management route"))
	}
}

type keyWriteRequest struct {
	ID                  string               `json:"id"`
	Name                *string              `json:"name,omitempty"`
	Enabled             *bool                `json:"enabled,omitempty"`
	Key                 string               `json:"key,omitempty"`
	RPM                 *int                 `json:"rpm,omitempty"`
	Models              []policy.KeyModelRef `json:"models,omitempty"`
	DailyLimitUSD       *float64             `json:"daily_limit_usd,omitempty"`
	WeeklyLimitUSD      *float64             `json:"weekly_limit_usd,omitempty"`
	MonthlyLimitUSD     *float64             `json:"monthly_limit_usd,omitempty"`
	AllowModelsEndpoint *bool                `json:"allow_models_endpoint,omitempty"`
}

type publicKey struct {
	ID                  string               `json:"id"`
	Name                string               `json:"name"`
	Enabled             bool                 `json:"enabled"`
	KeyPreview          string               `json:"key_preview"`
	RPM                 int                  `json:"rpm"`
	Models              []policy.KeyModelRef `json:"models"`
	DailyLimitUSD       float64              `json:"daily_limit_usd"`
	WeeklyLimitUSD      float64              `json:"weekly_limit_usd"`
	MonthlyLimitUSD     float64              `json:"monthly_limit_usd"`
	AllowModelsEndpoint bool                 `json:"allow_models_endpoint,omitempty"`
	Usage               policy.UsageSummary  `json:"usage"`
	CreatedAt           string               `json:"created_at,omitempty"`
	UpdatedAt           string               `json:"updated_at,omitempty"`
}

func (a *App) createKey(body []byte) ManagementResponse {
	var req keyWriteRequest
	if err := decodeStrictBody(body, &req); err != nil {
		return jsonError(http.StatusBadRequest, "invalid_json", err.Error())
	}
	req.ID = strings.TrimSpace(req.ID)
	if req.ID == "" {
		return jsonError(http.StatusBadRequest, "missing_id", "id is required")
	}
	plain := strings.TrimSpace(req.Key)
	generated := false
	var err error
	if plain == "" {
		plain, err = policy.GenerateKey()
		if err != nil {
			return jsonError(http.StatusInternalServerError, "key_generation_failed", err.Error())
		}
		generated = true
	}
	hash, err := policy.HashKey(plain)
	if err != nil {
		return jsonError(http.StatusBadRequest, "invalid_key", err.Error())
	}
	enabled := true
	if req.Enabled != nil {
		enabled = *req.Enabled
	}
	rpm := 0
	if req.RPM != nil {
		rpm = *req.RPM
	}
	name := req.ID
	if req.Name != nil && strings.TrimSpace(*req.Name) != "" {
		name = strings.TrimSpace(*req.Name)
	}
	item := policy.KeyConfig{
		ID:                  req.ID,
		Name:                name,
		Enabled:             enabled,
		KeyHash:             hash,
		KeyPreview:          policy.PreviewKey(plain),
		RPM:                 rpm,
		Models:              req.Models,
		DailyLimitUSD:       applyFloat64(req.DailyLimitUSD, 0),
		WeeklyLimitUSD:      applyFloat64(req.WeeklyLimitUSD, 0),
		MonthlyLimitUSD:     applyFloat64(req.MonthlyLimitUSD, 0),
		AllowModelsEndpoint: applyBool(req.AllowModelsEndpoint, false),
	}
	if err := a.store.UpsertKey(item, true); err != nil {
		return jsonError(http.StatusBadRequest, "invalid_policy", err.Error())
	}
	if stored := a.keyByID(item.ID); stored != nil {
		item = *stored
	}
	bodyMap := map[string]any{
		"key":       a.publicKeyFromConfig(item),
		"plain_key": plain,
		"generated": generated,
	}
	return jsonResponse(http.StatusCreated, bodyMap)
}

func (a *App) patchKey(body []byte) ManagementResponse {
	var req keyWriteRequest
	if err := decodeStrictBody(body, &req); err != nil {
		return jsonError(http.StatusBadRequest, "invalid_json", err.Error())
	}
	id := strings.TrimSpace(req.ID)
	if id == "" {
		return jsonError(http.StatusBadRequest, "missing_id", "id is required")
	}
	keys := a.store.Keys()
	var current *policy.KeyConfig
	for i := range keys {
		if keys[i].ID == id {
			copy := keys[i]
			current = &copy
			break
		}
	}
	if current == nil {
		return jsonError(http.StatusNotFound, "not_found", "key not found")
	}
	if req.Name != nil {
		current.Name = strings.TrimSpace(*req.Name)
	}
	if req.Enabled != nil {
		current.Enabled = *req.Enabled
	}
	if req.RPM != nil {
		current.RPM = *req.RPM
	}
	if req.DailyLimitUSD != nil {
		current.DailyLimitUSD = *req.DailyLimitUSD
	}
	if req.WeeklyLimitUSD != nil {
		current.WeeklyLimitUSD = *req.WeeklyLimitUSD
	}
	if req.MonthlyLimitUSD != nil {
		current.MonthlyLimitUSD = *req.MonthlyLimitUSD
	}
	if req.AllowModelsEndpoint != nil {
		current.AllowModelsEndpoint = *req.AllowModelsEndpoint
	}
	if req.Models != nil {
		current.Models = req.Models
	}
	if strings.TrimSpace(req.Key) != "" {
		hash, err := policy.HashKey(req.Key)
		if err != nil {
			return jsonError(http.StatusBadRequest, "invalid_key", err.Error())
		}
		current.KeyHash = hash
		current.KeyPreview = policy.PreviewKey(req.Key)
	}
	if err := a.store.UpsertKey(*current, true); err != nil {
		return jsonError(http.StatusBadRequest, "invalid_policy", err.Error())
	}
	if stored := a.keyByID(current.ID); stored != nil {
		current = stored
	}
	return jsonResponse(http.StatusOK, map[string]any{"key": a.publicKeyFromConfig(*current)})
}

func (a *App) keyByID(id string) *policy.KeyConfig {
	for _, key := range a.store.Keys() {
		if key.ID == id {
			copy := key
			return &copy
		}
	}
	return nil
}

func (a *App) deleteKey(id string) ManagementResponse {
	if err := a.store.DeleteKey(id); err != nil {
		return storeError(err)
	}
	return jsonResponse(http.StatusOK, map[string]any{"deleted": true, "id": strings.TrimSpace(id)})
}

func (a *App) rotateKey(id string) ManagementResponse {
	plain, item, err := a.store.RotateKey(id)
	if err != nil {
		return storeError(err)
	}
	return jsonResponse(http.StatusOK, map[string]any{
		"key":       a.publicKeyFromConfig(item),
		"plain_key": plain,
		"generated": true,
	})
}

func (a *App) resetRPM(id string) ManagementResponse {
	if err := a.store.ResetRPM(id); err != nil {
		return jsonError(http.StatusBadRequest, "invalid_request", err.Error())
	}
	return jsonResponse(http.StatusOK, map[string]any{"reset": true, "id": strings.TrimSpace(id)})
}

func (a *App) resetUsage(body []byte) ManagementResponse {
	var req struct {
		ID       string                        `json:"id"`
		Window   policy.UsageResetWindow       `json:"window"`
		Expected *policy.UsageResetExpectation `json:"expected,omitempty"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		return jsonError(http.StatusBadRequest, "invalid_json", err.Error())
	}
	req.ID = strings.TrimSpace(req.ID)
	if req.ID == "" {
		return jsonError(http.StatusBadRequest, "missing_id", "id is required")
	}
	result, err := a.store.ResetUsageWindow(req.ID, req.Window, req.Expected)
	if err != nil {
		switch {
		case errors.Is(err, policy.ErrUnknownKey):
			return jsonError(http.StatusNotFound, "not_found", "key not found")
		case errors.Is(err, policy.ErrInvalidUsageResetWindow):
			return jsonError(http.StatusBadRequest, "invalid_window", "window must be daily, weekly, or monthly")
		case errors.Is(err, policy.ErrInvalidUsageResetExpectation):
			return jsonError(http.StatusBadRequest, "invalid_expectation", err.Error())
		case errors.Is(err, policy.ErrUsageResetChanged):
			return jsonError(http.StatusConflict, "quota_changed", "Quota period changed; refresh and confirm the new reset date")
		default:
			return jsonError(http.StatusInternalServerError, "reset_failed", err.Error())
		}
	}
	log.Printf("cpa-key-policy: usage reset key_id=%q window=%s before_daily_usd=%.6f before_weekly_usd=%.6f before_monthly_usd=%.6f", result.KeyID, result.Window, result.BeforeDailyUSD, result.BeforeWeeklyUSD, result.BeforeMonthlyUSD)
	return jsonResponse(http.StatusOK, result)
}

// keyUsage returns the per-model usage breakdown for one downstream key.
func (a *App) keyUsage(id string) ManagementResponse {
	id = strings.TrimSpace(id)
	if id == "" {
		return jsonError(http.StatusBadRequest, "missing_id", "id is required")
	}
	key, models, ok := a.store.ModelUsageFor(id)
	if !ok {
		return jsonError(http.StatusNotFound, "not_found", "key not found")
	}
	return jsonResponse(http.StatusOK, map[string]any{
		"key_id":            key.ID,
		"key_name":          key.Name,
		"daily_limit_usd":   key.DailyLimitUSD,
		"weekly_limit_usd":  key.WeeklyLimitUSD,
		"monthly_limit_usd": key.MonthlyLimitUSD,
		"models":            models,
		"usage":             a.store.UsageSummaryFor(key),
	})
}

func (a *App) keyHistory(id string, days int) ManagementResponse {
	id = strings.TrimSpace(id)
	if id == "" {
		return jsonError(http.StatusBadRequest, "missing_id", "id is required")
	}
	if days < 1 || days > 35 {
		return jsonError(http.StatusBadRequest, "invalid_days", "days must be between 1 and 35")
	}
	key, history, ok := a.store.UsageHistoryFor(id, days)
	if !ok {
		return jsonError(http.StatusNotFound, "not_found", "key not found")
	}
	return jsonResponse(http.StatusOK, map[string]any{
		"key_id":   key.ID,
		"timezone": a.store.UsageSummaryFor(key).Timezone,
		"days":     history,
	})
}

func (a *App) auditEvents(keyID string, limit int) ManagementResponse {
	if limit < 1 || limit > 1000 {
		return jsonError(http.StatusBadRequest, "invalid_limit", "limit must be between 1 and 1000")
	}
	events, err := a.store.AuditEvents(keyID, limit)
	if err != nil {
		return jsonError(http.StatusInternalServerError, "audit_read_failed", err.Error())
	}
	return jsonResponse(http.StatusOK, map[string]any{"events": events})
}

func intQuery(query map[string][]string, name string, fallback int) int {
	values := query[name]
	if len(values) == 0 || strings.TrimSpace(values[0]) == "" {
		return fallback
	}
	value, err := strconv.Atoi(strings.TrimSpace(values[0]))
	if err != nil {
		return -1
	}
	return value
}

func storeError(err error) ManagementResponse {
	if errors.Is(err, policy.ErrUnknownKey) {
		return jsonError(http.StatusNotFound, "not_found", "key not found")
	}
	return jsonError(http.StatusBadRequest, "invalid_request", err.Error())
}

func idFromBody(body []byte) (string, error) {
	var payload struct {
		ID string `json:"id"`
	}
	if err := decodeStrictBody(body, &payload); err != nil {
		return "", err
	}
	return strings.TrimSpace(payload.ID), nil
}

func (a *App) publicKeys(keys []policy.KeyConfig) []publicKey {
	out := make([]publicKey, 0, len(keys))
	for _, key := range keys {
		out = append(out, a.publicKeyFromConfig(key))
	}
	return out
}

func (a *App) publicKeyFromConfig(key policy.KeyConfig) publicKey {
	out := publicKey{
		ID:                  key.ID,
		Name:                key.Name,
		Enabled:             key.Enabled,
		KeyPreview:          key.KeyPreview,
		RPM:                 key.RPM,
		Models:              append([]policy.KeyModelRef{}, key.Models...),
		DailyLimitUSD:       key.DailyLimitUSD,
		WeeklyLimitUSD:      key.WeeklyLimitUSD,
		MonthlyLimitUSD:     key.MonthlyLimitUSD,
		AllowModelsEndpoint: key.AllowModelsEndpoint,
		Usage:               a.store.UsageSummaryFor(key),
	}
	if !key.CreatedAt.IsZero() {
		out.CreatedAt = key.CreatedAt.UTC().Format("2006-01-02T15:04:05Z07:00")
	}
	if !key.UpdatedAt.IsZero() {
		out.UpdatedAt = key.UpdatedAt.UTC().Format("2006-01-02T15:04:05Z07:00")
	}
	return out
}

func applyFloat64(v *float64, def float64) float64 {
	if v == nil {
		return def
	}
	return *v
}

func applyBool(v *bool, def bool) bool {
	if v == nil {
		return def
	}
	return *v
}

func jsonResponse(status int, payload any) ManagementResponse {
	body, err := json.Marshal(payload)
	if err != nil {
		return jsonError(http.StatusInternalServerError, "json_error", err.Error())
	}
	return ManagementResponse{
		StatusCode: status,
		Headers:    http.Header{"Content-Type": []string{"application/json; charset=utf-8"}},
		Body:       body,
	}
}

func jsonError(status int, code, message string) ManagementResponse {
	if status <= 0 {
		status = http.StatusInternalServerError
	}
	body, _ := json.Marshal(map[string]any{
		"error": map[string]string{
			"code":    strings.TrimSpace(code),
			"message": strings.TrimSpace(message),
		},
	})
	return ManagementResponse{
		StatusCode: status,
		Headers:    http.Header{"Content-Type": []string{"application/json; charset=utf-8"}},
		Body:       body,
	}
}

func decodeStrictBody(raw []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		if err == nil {
			return errors.New("multiple JSON values are not allowed")
		}
		return err
	}
	return nil
}

func (a *App) Store() *policy.Store {
	if a == nil {
		return nil
	}
	return a.store
}

func DebugEnvelope(raw []byte) string {
	var env Envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return fmt.Sprintf("invalid envelope: %v", err)
	}
	if env.Error != nil {
		return env.Error.Message
	}
	return string(env.Result)
}
