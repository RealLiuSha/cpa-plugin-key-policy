package policy

import (
	"encoding/json"
	"net/http"
	"net/url"
	"regexp"
	"strings"
)

var geminiModelPathPattern = regexp.MustCompile(`/models/([^/:]+)(?::|/)`)

// ExtractRequestedModel reads the body's top-level "model" without
// materializing the rest of the body, then the query and the path. A nested
// "model" comes last: nested values can be conversation content, such as a
// tool call argument in a Gemini request whose model is in the path.
func ExtractRequestedModel(path string, query url.Values, body []byte) string {
	if model := topLevelModel(body); model != "" {
		return model
	}
	if model := strings.TrimSpace(query.Get("model")); model != "" {
		return model
	}
	if model := extractModelFromPath(path); model != "" {
		return model
	}
	return nestedModel(body)
}

func topLevelModel(body []byte) string {
	var probe struct {
		Model string `json:"model"`
	}
	if len(body) == 0 || json.Unmarshal(body, &probe) != nil {
		return ""
	}
	return strings.TrimSpace(probe.Model)
}

func nestedModel(body []byte) string {
	var payload any
	if len(body) == 0 || json.Unmarshal(body, &payload) != nil {
		return ""
	}
	return findModelValue(payload)
}

func findModelValue(value any) string {
	switch v := value.(type) {
	case map[string]any:
		if raw, ok := v["model"]; ok {
			if model, ok := raw.(string); ok {
				return strings.TrimSpace(model)
			}
		}
		for _, raw := range v {
			if model := findModelValue(raw); model != "" {
				return model
			}
		}
	case []any:
		for _, raw := range v {
			if model := findModelValue(raw); model != "" {
				return model
			}
		}
	}
	return ""
}

func extractModelFromPath(path string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return ""
	}
	if match := geminiModelPathPattern.FindStringSubmatch(path); len(match) == 2 {
		if decoded, err := url.PathUnescape(match[1]); err == nil {
			return strings.TrimSpace(decoded)
		}
		return strings.TrimSpace(match[1])
	}
	return ""
}

func IsModelsEndpoint(path string) bool {
	path = strings.TrimRight(strings.TrimSpace(path), "/")
	switch {
	case path == "/v1/models":
		return true
	case path == "/openai/v1/models":
		return true
	case strings.HasSuffix(path, "/v1/models"):
		return true
	default:
		return false
	}
}

var mediaGenerationSuffixes = []string{
	"/v1/images/generations",
	"/v1/images/edits",
	"/v1/videos",
	"/v1/videos/generations",
	"/v1/videos/edits",
	"/v1/videos/extensions",
}

// isMediaGenerationPath reports an image or video generation endpoint,
// including the /openai/v1 routes. Per-call models are precharged here.
func isMediaGenerationPath(path string) bool {
	path = strings.TrimRight(strings.TrimSpace(path), "/")
	for _, suffix := range mediaGenerationSuffixes {
		if strings.HasSuffix(path, suffix) {
			return true
		}
	}
	return false
}

// isMediaResultPath reports retrieval of a generated video or its content.
// It matches both request URLs (/v1/videos/abc) and CPA route patterns
// (/v1/videos/:request_id) as the interceptor reports them.
func isMediaResultPath(path string) bool {
	path = strings.TrimRight(strings.TrimSpace(path), "/")
	if isMediaGenerationPath(path) {
		return false
	}
	return strings.Contains(path, "/v1/videos/")
}

// admissionStage says where RPM and quota are enforced for a request. CPA can
// only turn a frontend-auth rejection into 401, while its request interceptor
// can return any status. Requests therefore move to the interceptor wherever
// CPA runs it before the upstream call, the client receives the HTTP status,
// and the key is in a header the interceptor can read (it receives no query).
type admissionStage int

const (
	admitAtAuth admissionStage = iota
	// WebSocket turns are intercepted one by one, but a rejected turn only
	// closes the socket, so the handshake checks quota and leaves RPM per turn.
	admitQuotaAtAuth
	admitAtInterceptor
)

// Routes on which CPA (v7.2.103 and later) applies request interceptors with
// termination before contacting upstream.
var interceptedPOSTPaths = map[string]bool{
	"/v1/chat/completions":                 true,
	"/v1/completions":                      true,
	"/v1/images/generations":               true,
	"/v1/images/edits":                     true,
	"/v1/videos":                           true,
	"/v1/videos/generations":               true,
	"/v1/videos/edits":                     true,
	"/v1/videos/extensions":                true,
	"/openai/v1/videos":                    true,
	"/v1/messages":                         true,
	"/v1/messages/count_tokens":            true,
	"/v1/responses":                        true,
	"/v1/responses/compact":                true,
	"/backend-api/codex/responses":         true,
	"/backend-api/codex/responses/compact": true,
	"/v1beta/interactions":                 true,
}

var websocketResponsePaths = map[string]bool{
	"/v1/responses":                true,
	"/backend-api/codex/responses": true,
}

func admissionStageFor(method, path string, headers http.Header) admissionStage {
	path = strings.TrimRight(strings.TrimSpace(path), "/")
	if ExtractAPIKey(headers, nil) == "" {
		return admitAtAuth
	}
	switch strings.ToUpper(strings.TrimSpace(method)) {
	case http.MethodPost:
		if interceptedPOSTPaths[path] || strings.HasPrefix(path, "/v1beta/models/") {
			return admitAtInterceptor
		}
	case http.MethodGet:
		if websocketResponsePaths[path] {
			return admitQuotaAtAuth
		}
		if isMediaResultPath(path) {
			return admitAtInterceptor
		}
	}
	return admitAtAuth
}

func RewriteTopLevelModel(body []byte, model string) ([]byte, bool) {
	model = strings.TrimSpace(model)
	if model == "" || len(body) == 0 || !json.Valid(body) {
		return nil, false
	}
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, false
	}
	if _, ok := payload["model"]; !ok {
		return nil, false
	}
	payload["model"] = model
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, false
	}
	return raw, true
}
