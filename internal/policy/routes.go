package policy

import (
	"net/http"
	"strings"
)

// IsModelsEndpoint reports the model list routes, which every enabled key
// may read without admission. The Gemini list is matched exactly: its
// generation routes live under /v1beta/models/<model>:… and are admitted.
func IsModelsEndpoint(path string) bool {
	path = strings.TrimRight(strings.TrimSpace(path), "/")
	switch {
	case path == "/v1/models":
		return true
	case path == "/openai/v1/models":
		return true
	case path == "/v1beta/models":
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
// including the /openai/v1 routes. Per-call billing is tied to these.
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
// termination before contacting upstream. The speech and TTS routes exist
// from CPA v8.0.18; on older hosts gin answers 404 before authentication.
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
	"/v1/audio/speech":                     true,
	"/v1/tts":                              true,
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
