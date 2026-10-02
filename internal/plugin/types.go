package plugin

import (
	"encoding/json"
	"net/http"
	"net/url"
)

const (
	ABIVersion uint32 = 1
	// SchemaVersion is the lifecycle contract this plugin speaks, echoed back to the
	// host at plugin.register so it knows how to talk to us. Staying at 2 is
	// deliberate: schema 3 only omits OriginalRequest/RequestBody on payload stream
	// chunks, and this plugin registers no stream chunk interceptor.
	SchemaVersion uint32 = 2
	// MinHostSchemaVersion is the oldest host lifecycle contract this plugin can work
	// with. The host advertises its own ceiling, so anything at or above this is fine;
	// requiring an exact match would make every host upgrade disable the plugin.
	MinHostSchemaVersion uint32 = 2

	MethodPluginRegister    = "plugin.register"
	MethodPluginReconfigure = "plugin.reconfigure"

	MethodFrontendAuthIdentifier   = "frontend_auth.identifier"
	MethodFrontendAuthAuthenticate = "frontend_auth.authenticate"

	MethodModelRoute = "model.route"

	// Request interceptors run before CPA contacts upstream. intercept_before
	// fires once per model execution and may terminate it with any HTTP status;
	// intercept_after fires again for every credential attempt.
	MethodRequestInterceptBefore = "request.intercept_before"
	MethodRequestInterceptAfter  = "request.intercept_after"

	MethodResponseInterceptAfter = "response.intercept_after"

	// MethodUsageHandle is the host->plugin call that delivers a finalized
	// usage record (tokens already parsed by CPA) after a request completes.
	// This is the billing entry point that ALSO fires for streaming responses
	// (unlike response.intercept_after, which the host never invokes on the
	// streaming path — it only invokes response.intercept_stream_chunk).
	MethodUsageHandle = "usage.handle"

	MethodManagementRegister = "management.register"
	MethodManagementHandle   = "management.handle"
)

const (
	PluginID   = "cpa-key-policy"
	PluginName = "cpa-key-policy"
	Version    = "0.11.0"
)

type Envelope struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *EnvelopeError  `json:"error,omitempty"`
}

type EnvelopeError struct {
	Code       string `json:"code"`
	Message    string `json:"message"`
	Retryable  bool   `json:"retryable,omitempty"`
	HTTPStatus int    `json:"http_status,omitempty"`
}

type LifecycleRequest struct {
	ConfigYAML    []byte `json:"config_yaml"`
	SchemaVersion uint32 `json:"schema_version"`
}

type Registration struct {
	SchemaVersion uint32       `json:"schema_version"`
	Metadata      Metadata     `json:"metadata"`
	Capabilities  Capabilities `json:"capabilities"`
}

type Metadata struct {
	Name             string        `json:"Name"`
	Version          string        `json:"Version"`
	Author           string        `json:"Author"`
	GitHubRepository string        `json:"GitHubRepository"`
	Logo             string        `json:"Logo,omitempty"`
	ConfigFields     []ConfigField `json:"ConfigFields"`
}

type ConfigField struct {
	Name        string   `json:"Name"`
	Type        string   `json:"Type"`
	EnumValues  []string `json:"EnumValues,omitempty"`
	Description string   `json:"Description"`
}

type Capabilities struct {
	FrontendAuthProvider          bool `json:"frontend_auth_provider"`
	FrontendAuthProviderExclusive bool `json:"frontend_auth_provider_exclusive,omitempty"`
	ModelRouter                   bool `json:"model_router"`
	RequestInterceptor            bool `json:"request_interceptor"`
	ResponseInterceptor           bool `json:"response_interceptor"`
	UsagePlugin                   bool `json:"usage_plugin"`
	ManagementAPI                 bool `json:"management_api"`
}

type IdentifierResponse struct {
	Identifier string `json:"identifier"`
}

type FrontendAuthRequest struct {
	Method  string      `json:"Method"`
	Path    string      `json:"Path"`
	Headers http.Header `json:"Headers"`
	Query   url.Values  `json:"Query"`
	Body    []byte      `json:"Body"`
}

type FrontendAuthResponse struct {
	Authenticated bool              `json:"Authenticated"`
	Principal     string            `json:"Principal,omitempty"`
	Metadata      map[string]string `json:"Metadata,omitempty"`
}

// ModelRouteRequest mirrors CPA's pluginapi.ModelRouteRequest without the
// request body the host also sends: routing never reads it, and leaving it out
// spares decoding a copy of every request.
type ModelRouteRequest struct {
	SourceFormat       string         `json:"SourceFormat"`
	RequestedModel     string         `json:"RequestedModel"`
	Stream             bool           `json:"Stream"`
	Headers            http.Header    `json:"Headers"`
	Query              url.Values     `json:"Query"`
	Metadata           map[string]any `json:"Metadata"`
	AvailableProviders []string       `json:"AvailableProviders"`
}

type ModelRouteResponse struct {
	Handled     bool   `json:"Handled"`
	TargetKind  string `json:"TargetKind,omitempty"`
	Target      string `json:"Target,omitempty"`
	TargetModel string `json:"TargetModel,omitempty"`
	Reason      string `json:"Reason,omitempty"`
}

// RequestInterceptRequest is the part of CPA's pluginapi.RequestInterceptRequest
// this plugin reads. Headers are a clone of the inbound request headers; there
// is no query string. Metadata carries request_path, the matched CPA route.
type RequestInterceptRequest struct {
	SourceFormat   string         `json:"SourceFormat"`
	RequestedModel string         `json:"RequestedModel"`
	Headers        http.Header    `json:"Headers"`
	Metadata       map[string]any `json:"Metadata"`
}

// RequestInterceptResponse mirrors pluginapi.RequestInterceptResponse. An empty
// response leaves the request unchanged. ResponseBody must stay []byte: the
// host decodes it as base64 and lets the request through when it cannot.
type RequestInterceptResponse struct {
	Terminate       bool        `json:"Terminate,omitempty"`
	StatusCode      int         `json:"StatusCode,omitempty"`
	ResponseHeaders http.Header `json:"ResponseHeaders,omitempty"`
	ResponseBody    []byte      `json:"ResponseBody,omitempty"`
}

// ResponseInterceptRequest mirrors CPA's pluginapi.ResponseInterceptRequest
// without the original and translated request bodies the host repeats: the
// model rewrite only reads the response body, and leaving them out spares
// decoding both on every non-streaming response.
type ResponseInterceptRequest struct {
	SourceFormat    string         `json:"SourceFormat"`
	Model           string         `json:"Model"`
	RequestedModel  string         `json:"RequestedModel"`
	Stream          bool           `json:"Stream"`
	RequestHeaders  http.Header    `json:"RequestHeaders"`
	ResponseHeaders http.Header    `json:"ResponseHeaders"`
	Body            []byte         `json:"Body"`
	StatusCode      int            `json:"StatusCode"`
	Metadata        map[string]any `json:"Metadata"`
}

// UsageHandleRequest is the payload of the host->plugin usage.handle call.
// CPA parses the token counts itself (from the upstream response, including
// the final usage frame of a stream) and delivers them here after a request
// completes — both streaming and non-streaming. This is the reliable billing
// entry point: the host never invokes response.intercept_after on streaming
// responses, so the plugin cannot rely on that alone to bill streams.
type UsageHandleRequest struct {
	// Model is the resolved upstream model id.
	Model string `json:"Model"`
	// This host field carries the requested public model. Its name is fixed by
	// the current CPA transport and is not exposed to the policy domain.
	Alias string `json:"Alias"`
	// APIKey is the client's downstream key (the cpa_... value), when available.
	// We hash it to find the owning key config — same lookup path as auth.
	APIKey string      `json:"APIKey"`
	Failed bool        `json:"Failed"`
	Detail UsageDetail `json:"Detail"`
}

// UsageDetail mirrors CPA's usage token breakdown. Only the fields we bill on.
type UsageDetail struct {
	InputTokens         int64 `json:"InputTokens"`
	OutputTokens        int64 `json:"OutputTokens"`
	ReasoningTokens     int64 `json:"ReasoningTokens"`
	CachedTokens        int64 `json:"CachedTokens"`
	CacheReadTokens     int64 `json:"CacheReadTokens"`
	CacheCreationTokens int64 `json:"CacheCreationTokens"`
	TotalTokens         int64 `json:"TotalTokens"`
}

// UsageHandleResponse is empty: usage.handle is a fire-and-forget notification.
type UsageHandleResponse struct{}

type ResponseInterceptResponse struct {
	Headers      http.Header `json:"Headers,omitempty"`
	Body         []byte      `json:"Body,omitempty"`
	ClearHeaders []string    `json:"ClearHeaders,omitempty"`
}

type ManagementRegistrationRequest struct {
	BasePath         string `json:"BasePath"`
	ResourceBasePath string `json:"ResourceBasePath"`
}

type ManagementRegistrationResponse struct {
	Routes    []ManagementRoute `json:"Routes"`
	Resources []ResourceRoute   `json:"Resources,omitempty"`
}

type ManagementRoute struct {
	Method      string `json:"Method"`
	Path        string `json:"Path"`
	Description string `json:"Description,omitempty"`
}

// ResourceRoute declares a browser-navigable, unauthenticated GET resource that
// CPA serves under /v0/resource/plugins/<pluginID><Path>.
type ResourceRoute struct {
	Path        string `json:"Path"`
	Menu        string `json:"Menu,omitempty"`
	Description string `json:"Description,omitempty"`
}

type ManagementRequest struct {
	Method  string      `json:"Method"`
	Path    string      `json:"Path"`
	Headers http.Header `json:"Headers"`
	Query   url.Values  `json:"Query"`
	Body    []byte      `json:"Body"`
}

type ManagementResponse struct {
	StatusCode int         `json:"StatusCode,omitempty"`
	Headers    http.Header `json:"Headers,omitempty"`
	Body       []byte      `json:"Body,omitempty"`
}

func OKEnvelope(v any) ([]byte, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return json.Marshal(Envelope{OK: true, Result: raw})
}

func ErrorEnvelope(code, message string, status int) []byte {
	raw, _ := json.Marshal(Envelope{
		OK: false,
		Error: &EnvelopeError{
			Code:       code,
			Message:    message,
			HTTPStatus: status,
		},
	})
	return raw
}
