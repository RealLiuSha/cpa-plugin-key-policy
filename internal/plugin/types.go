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

	// Request interceptors run before CPA contacts upstream. intercept_before
	// fires once per model execution and may terminate it with any HTTP status;
	// intercept_after fires again for every credential attempt and is answered
	// with an empty response because the capability registers both.
	MethodRequestInterceptBefore = "request.intercept_before"
	MethodRequestInterceptAfter  = "request.intercept_after"

	// MethodUsageHandle delivers a finalized usage record (tokens already parsed
	// by CPA) after a request completes, for streaming and non-streaming
	// responses alike. It is the only billing entry point.
	MethodUsageHandle = "usage.handle"

	MethodManagementRegister = "management.register"
	MethodManagementHandle   = "management.handle"
)

const (
	PluginID   = "cpa-key-policy"
	PluginName = "cpa-key-policy"
	Version    = "0.15.0"
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

// Capabilities lists what this plugin registers. Routing and response
// rewriting are not among them: CPA routes by its own registry and the
// response model is whatever upstream reports.
type Capabilities struct {
	FrontendAuthProvider          bool `json:"frontend_auth_provider"`
	FrontendAuthProviderExclusive bool `json:"frontend_auth_provider_exclusive,omitempty"`
	RequestInterceptor            bool `json:"request_interceptor"`
	UsagePlugin                   bool `json:"usage_plugin"`
	ManagementAPI                 bool `json:"management_api"`
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
// this plugin reads. TraceID is the inbound HTTP request id, which links a
// media generation to its usage record. Headers are a clone of the inbound
// request headers; there is no query string. Metadata carries request_path,
// the matched CPA route.
type RequestInterceptRequest struct {
	TraceID      string         `json:"TraceID"`
	SourceFormat string         `json:"SourceFormat"`
	Headers      http.Header    `json:"Headers"`
	Metadata     map[string]any `json:"Metadata"`
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

// UsageHandleRequest is the part of CPA's pluginapi.UsageRecord this plugin
// bills on. The host's Generate flag is deliberately not read: a client that
// sends generate=false still has its input processed upstream. Latency and
// TTFT arrive as int64 nanoseconds and are not needed.
type UsageHandleRequest struct {
	// TraceID identifies the inbound HTTP request (8-character hex).
	TraceID      string `json:"TraceID"`
	Provider     string `json:"Provider"`
	ExecutorType string `json:"ExecutorType"`
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
