package plugin

import (
	"encoding/json"
	"net/http"
	"net/url"
)

const (
	ABIVersion uint32 = 1
	// SchemaVersion is the highest lifecycle contract this plugin speaks. At
	// plugin.register it answers the lower of this and the host's version.
	// Schema 6 makes CPA pass management responses through as they are; below
	// it the host HTML-escapes every JSON string, so a quote in a note or an
	// upstream error reaches the UI as &#34;. Schemas 3 to 5 only change stream
	// chunk interceptors, which this plugin does not register.
	SchemaVersion uint32 = 6
	// MinHostSchemaVersion is the oldest host lifecycle contract this plugin can work
	// with. The host advertises its own ceiling, so anything at or above this is fine;
	// requiring an exact match would make every host upgrade disable the plugin.
	MinHostSchemaVersion uint32 = 2

	MethodPluginRegister    = "plugin.register"
	MethodPluginReconfigure = "plugin.reconfigure"
	// MethodPluginQuiesce precedes a hot replacement: the host is about to
	// load a newer instance and will stop sending calls here once it is in.
	MethodPluginQuiesce = "plugin.quiesce"

	MethodFrontendAuthIdentifier   = "frontend_auth.identifier"
	MethodFrontendAuthAuthenticate = "frontend_auth.authenticate"

	// Request interceptors run before CPA contacts upstream. intercept_before
	// fires once per model execution and may terminate it with any HTTP status;
	// intercept_after fires again for every credential attempt and is answered
	// with an empty response because the capability registers both.
	MethodRequestInterceptBefore = "request.intercept_before"
	MethodRequestInterceptAfter  = "request.intercept_after"

	// MethodModelRoute runs before provider resolution for every model
	// request once the plugin registers model_router; only alpha search
	// requests are answered.
	MethodModelRoute = "model.route"

	// MethodResponseInterceptAfter runs for every successful non-streaming
	// response and model list once the plugin registers response_interceptor;
	// only Codex model catalogs are rewritten.
	MethodResponseInterceptAfter = "response.intercept_after"

	// MethodUsageHandle delivers a finalized usage record (tokens already parsed
	// by CPA) after a request completes, for streaming and non-streaming
	// responses alike. It is the only billing entry point.
	MethodUsageHandle = "usage.handle"

	MethodManagementRegister = "management.register"
	MethodManagementHandle   = "management.handle"
)

const (
	// PluginID is the host-local identity: the library file name, the
	// plugins.configs key and every management and resource path. It stays
	// cpa-key-policy so callers and data files keep working.
	PluginID = "cpa-key-policy"
	// PluginName is what the CPA panel shows for the plugin and its menu.
	PluginName = "萝卜"
	Version    = "0.29.0"
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

// Capabilities lists what this plugin registers. CPA routes by its own
// registry and the response model is whatever upstream reports; the search
// module alone routes alpha search and rewrites the Codex catalog.
type Capabilities struct {
	FrontendAuthProvider          bool `json:"frontend_auth_provider"`
	FrontendAuthProviderExclusive bool `json:"frontend_auth_provider_exclusive,omitempty"`
	// ModelRouter is declared only while alpha search routing is switched
	// on: once any plugin is a router, every model request crosses into it
	// with its full body.
	ModelRouter bool `json:"model_router,omitempty"`
	// ResponseInterceptor is declared with the search module too, for native
	// search: every successful non-streaming response then crosses into the
	// plugin, which answers all but the Codex catalog from the first members.
	ResponseInterceptor bool `json:"response_interceptor,omitempty"`
	RequestInterceptor  bool `json:"request_interceptor"`
	UsagePlugin         bool `json:"usage_plugin"`
	ManagementAPI       bool `json:"management_api"`
}

type IdentifierResponse struct {
	Identifier string `json:"identifier"`
}

// FrontendAuthRequest is the part of CPA's pluginapi.FrontendAuthRequest this
// plugin reads. The host also sends the request body; nothing here depends on
// it, so it is not decoded.
type FrontendAuthRequest struct {
	Method  string      `json:"Method"`
	Path    string      `json:"Path"`
	Headers http.Header `json:"Headers"`
	Query   url.Values  `json:"Query"`
}

type FrontendAuthResponse struct {
	Authenticated bool              `json:"Authenticated"`
	Principal     string            `json:"Principal,omitempty"`
	Metadata      map[string]string `json:"Metadata,omitempty"`
}

// RequestInterceptRequest is the part of CPA's pluginapi.RequestInterceptRequest
// request.intercept_before reads. TraceID is the inbound HTTP request id,
// which links a media generation to its usage record. Headers are a clone of
// the inbound request headers; there is no query string.
type RequestInterceptRequest struct {
	TraceID      string            `json:"TraceID"`
	SourceFormat string            `json:"SourceFormat"`
	Headers      http.Header       `json:"Headers"`
	Metadata     InterceptMetadata `json:"Metadata"`
}

// AttemptInterceptRequest is the part request.intercept_after reads for one
// upstream attempt: the model the client asked for and, in Metadata, the
// attempt's credential. The headers, most of what there is to decode, are
// left out.
type AttemptInterceptRequest struct {
	RequestedModel string            `json:"RequestedModel"`
	Metadata       InterceptMetadata `json:"Metadata"`
}

// InterceptMetadata is what the plugin reads of an intercept request's
// Metadata: request_path, the matched CPA route, and selected_auth_index, the
// credential of an attempt. The other values are not decoded.
type InterceptMetadata struct {
	RequestPath       metadataString `json:"request_path"`
	SelectedAuthIndex metadataString `json:"selected_auth_index"`
}

// metadataString is a Metadata value read as a string; a value of another
// type reads as empty instead of failing the request.
type metadataString string

func (s *metadataString) UnmarshalJSON(raw []byte) error {
	var value string
	if json.Unmarshal(raw, &value) == nil {
		*s = metadataString(value)
	}
	return nil
}

// RequestInterceptResponse mirrors pluginapi.RequestInterceptResponse. An empty
// response leaves the request unchanged. ResponseBody must stay []byte: the
// host decodes it as base64 and lets the request through when it cannot.
// A non-empty Body replaces the request body of this attempt only; it is in
// the client's format, since the executor translates it afterwards.
type RequestInterceptResponse struct {
	Body            []byte      `json:"Body,omitempty"`
	Terminate       bool        `json:"Terminate,omitempty"`
	StatusCode      int         `json:"StatusCode,omitempty"`
	ResponseHeaders http.Header `json:"ResponseHeaders,omitempty"`
	ResponseBody    []byte      `json:"ResponseBody,omitempty"`
}

// ResponseInterceptResponse mirrors pluginapi.ResponseInterceptResponse. An
// empty response leaves the response unchanged; a non-empty Body replaces it.
type ResponseInterceptResponse struct {
	Body []byte `json:"Body,omitempty"`
}

// UsageHandleRequest is the part of CPA's pluginapi.UsageRecord this plugin
// bills on and builds channel statistics from. CPA sends one record per
// upstream attempt. The host's Generate flag is deliberately not read: a
// client that sends generate=false still has its input processed upstream.
type UsageHandleRequest struct {
	// TraceID identifies the inbound HTTP request (8-character hex).
	TraceID      string `json:"TraceID"`
	Provider     string `json:"Provider"`
	BaseURL      string `json:"BaseURL"`
	ExecutorType string `json:"ExecutorType"`
	// AuthID and AuthIndex identify the credential the attempt ran on.
	AuthID    string `json:"AuthID"`
	AuthIndex string `json:"AuthIndex"`
	// RequestedAt is a time.Time on the wire (RFC 3339), read as text and
	// parsed by the handler: a time.Time field that fails to decode stops
	// the whole record, a text field only leaves itself empty.
	RequestedAt string `json:"RequestedAt"`
	// Latency and TTFT are time.Duration values, nanoseconds on the wire.
	// A non-stream response arrives whole, so its TTFT is about its latency;
	// Stream tells the two apart.
	Latency int64        `json:"Latency"`
	TTFT    int64        `json:"TTFT"`
	Stream  bool         `json:"Stream"`
	Failure UsageFailure `json:"Failure"`
	// Model is the upstream model id the request ran on.
	Model string `json:"Model"`
	// This host field carries the model name the client requested. Its name is
	// fixed by the CPA transport and is not exposed to the policy domain.
	Alias string `json:"Alias"`
	// APIKey is the auth principal (the key id) or the plaintext key.
	APIKey string      `json:"APIKey"`
	Failed bool        `json:"Failed"`
	Detail UsageDetail `json:"Detail"`
}

// UsageFailure mirrors pluginapi.UsageFailure.
type UsageFailure struct {
	StatusCode int    `json:"StatusCode"`
	Body       string `json:"Body"`
}

// ModelRouteRequest is the part of pluginapi.ModelRouteRequest the alpha
// search router reads; all of it precedes the request body on the wire.
type ModelRouteRequest struct {
	SourceFormat   string      `json:"SourceFormat"`
	RequestedModel string      `json:"RequestedModel"`
	Headers        http.Header `json:"Headers"`
}

// UsageDetail mirrors CPA's usage token breakdown.
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
	// HostCallbackID ties a host callback made for this request to the
	// management HTTP request's context, so a client that goes away cancels it.
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

type ManagementResponse struct {
	StatusCode int         `json:"StatusCode,omitempty"`
	Headers    http.Header `json:"Headers,omitempty"`
	Body       []byte      `json:"Body,omitempty"`
}

func OKEnvelope(v any) ([]byte, error) {
	// One pass, with the bytes of Envelope{OK: true, Result: <v marshaled>}:
	// marshaling v first and then the Envelope would scan the whole result
	// again as a json.RawMessage, about ten times the first pass for a
	// rewritten multi-megabyte body.
	return json.Marshal(struct {
		OK     bool `json:"ok"`
		Result any  `json:"result"`
	}{OK: true, Result: v})
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
