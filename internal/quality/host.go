package quality

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

// Host calls back into CPA: the credential list and pinned model streams.
type Host interface {
	Call(method string, payload any) (json.RawMessage, error)
}

// HostError is a host call CPA answered with an error envelope. HTTPStatus is
// the upstream status when CPA reports one.
type HostError struct {
	Code       string
	Message    string
	HTTPStatus int
}

// Error is the message alone: CPA answers every failed callback with the same
// code, host_call_failed.
func (e *HostError) Error() string {
	if e.Message == "" {
		return e.Code
	}
	return e.Message
}

// Credential sources, as candy-eval named them: an auth file, or an API key
// from CPA's config.
const (
	SourceFile     = "auth_files"
	SourceProvider = "ai_providers"
)

// Credential is one CPA credential a test can be pinned to. AuthIndex is the
// value channel statistics key on; Group and BaseURL describe an API key of
// CPA's config.
type Credential struct {
	ID        string `json:"id"`
	AuthIndex string `json:"auth_index,omitempty"`
	Provider  string `json:"provider"`
	Source    string `json:"source"`
	Name      string `json:"name"`
	Group     string `json:"group,omitempty"`
	BaseURL   string `json:"base_url,omitempty"`
	Email     string `json:"email,omitempty"`
	PlanType  string `json:"plan_type,omitempty"`
	Priority  int    `json:"priority"`
	Disabled  bool   `json:"disabled"`
	// CPA marks enabled credentials unavailable while they cool down.
	Unavailable    bool      `json:"unavailable,omitempty"`
	StatusMessage  string    `json:"status_message,omitempty"`
	NextRetryAfter time.Time `json:"next_retry_after,omitzero"`
	// modTime is when CPA last changed an auth file.
	modTime time.Time
}

type hostAuthFile struct {
	ID             string    `json:"id"`
	AuthIndex      string    `json:"auth_index"`
	Name           string    `json:"name"`
	Provider       string    `json:"provider"`
	Type           string    `json:"type"`
	Source         string    `json:"source"`
	Path           string    `json:"path"`
	RuntimeOnly    bool      `json:"runtime_only"`
	Account        string    `json:"account"`
	AccountType    string    `json:"account_type"`
	Email          string    `json:"email"`
	PlanType       string    `json:"plan_type"`
	Priority       int       `json:"priority"`
	Disabled       bool      `json:"disabled"`
	Unavailable    bool      `json:"unavailable"`
	StatusMessage  string    `json:"status_message"`
	NextRetryAfter time.Time `json:"next_retry_after"`
	ModTime        time.Time `json:"modtime"`
}

// listCredentials reads CPA's auth manager list: auth files and runtime
// credentials, which leaves out the API keys of CPA's config. An API key is
// shown only as a masked preview.
func listCredentials(host Host, now time.Time) ([]Credential, error) {
	raw, err := host.Call("host.auth.list", map[string]any{})
	if err != nil {
		return nil, fmt.Errorf("read CPA credentials: %w", err)
	}
	var list struct {
		Files []hostAuthFile `json:"files"`
	}
	if err := json.Unmarshal(raw, &list); err != nil || list.Files == nil {
		return nil, fmt.Errorf("read CPA credentials: unexpected answer")
	}
	byID := make(map[string]Credential, len(list.Files))
	for _, file := range list.Files {
		provider := strings.ToLower(strings.TrimSpace(file.Provider))
		if provider == "" {
			provider = strings.ToLower(strings.TrimSpace(file.Type))
		}
		if strings.TrimSpace(file.ID) == "" || provider == "" {
			continue
		}
		c := Credential{ID: file.ID, AuthIndex: file.AuthIndex, Name: file.Name, Provider: provider, Email: file.Email,
			PlanType: file.PlanType, Priority: file.Priority, Disabled: file.Disabled, Source: SourceFile, modTime: file.ModTime}
		// Like CPA's scheduler, a cooldown is over once its retry time passes,
		// even if the flag remains.
		if file.Unavailable && !file.Disabled && (file.NextRetryAfter.IsZero() || file.NextRetryAfter.After(now)) {
			c.Unavailable, c.StatusMessage, c.NextRetryAfter = true, strings.TrimSpace(file.StatusMessage), file.NextRetryAfter
		}
		source := strings.ToLower(strings.TrimSpace(file.Source))
		if file.RuntimeOnly || source == "config" || strings.HasPrefix(source, "config:") || (source == "memory" && file.Path == "") {
			c.Source, c.Name, c.Email, c.PlanType = SourceProvider, c.ID, "", ""
			if strings.EqualFold(file.AccountType, "api_key") && strings.TrimSpace(file.Account) != "" {
				c.Name = PreviewKey(file.Account)
			}
		}
		if c.Name == "" {
			c.Name = c.ID
		}
		byID[c.ID] = c
	}
	out := make([]Credential, 0, len(byID))
	for _, c := range byID {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Name == out[j].Name {
			return out[i].ID < out[j].ID
		}
		return out[i].Name < out[j].Name
	})
	return out, nil
}

// PreviewKey masks an API key for display: its first 6 and last 4
// characters, or only stars when it is 12 characters or shorter.
func PreviewKey(value string) string {
	value = strings.TrimSpace(value)
	if len(value) <= 12 {
		return strings.Repeat("*", len(value))
	}
	return value[:6] + "…" + value[len(value)-4:]
}

// planType returns the subscription in the Codex auth file at authIndex: its
// plan_type, else the chatgpt_plan_type claim of its ID token. ok is false
// when the file could not be read. It is a display label, never used for
// authorization.
func planType(host Host, authIndex string) (plan string, ok bool) {
	raw, err := host.Call("host.auth.get", map[string]string{"auth_index": authIndex})
	if err != nil {
		return "", false
	}
	var file struct {
		JSON struct {
			PlanType string `json:"plan_type"`
			IDToken  string `json:"id_token"`
		} `json:"json"`
	}
	if json.Unmarshal(raw, &file) != nil {
		return "", false
	}
	if plan := strings.TrimSpace(file.JSON.PlanType); plan != "" {
		return plan, true
	}
	parts := strings.Split(file.JSON.IDToken, ".")
	if len(parts) != 3 {
		return "", true
	}
	payload, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return "", true
	}
	var claims struct {
		Auth struct {
			PlanType string `json:"chatgpt_plan_type"`
		} `json:"https://api.openai.com/auth"`
	}
	if json.Unmarshal(payload, &claims) != nil {
		return "", true
	}
	return strings.TrimSpace(claims.Auth.PlanType), true
}

// executeModel sends a streaming Responses request pinned to credential c;
// CPA translates it to the provider's protocol. When ctx ends first the
// stream is closed, which cancels the upstream request, and ctx's cause is
// returned. On failure status is the upstream HTTP status, or 0 when no
// complete response arrived; a stream over ModelStreamMaxBytes is cut off
// with status 200 and an *AnswerError, not worth repeating.
func executeModel(ctx context.Context, host Host, c Credential, model string, body any, headers http.Header) (_ ModelResponse, _ int, err error) {
	// A panic in the host bridge or in parsing fails this request only; the
	// deferred stream close below still runs.
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("模型请求异常：%v", recovered)
		}
	}()
	if strings.TrimSpace(c.ID) == "" || strings.TrimSpace(c.Provider) == "" {
		return ModelResponse{}, 0, errors.New("the credential has no ID or provider to pin the request to")
	}
	// Keep <, > and & literal, as Codex sends them.
	var request bytes.Buffer
	encoder := json.NewEncoder(&request)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(body); err != nil {
		return ModelResponse{}, 0, err
	}
	raw, err := host.Call("host.model.execute_stream", map[string]any{
		"entry_protocol": "openai-response", "exit_protocol": "openai-response",
		"model": model, "stream": true, "body": bytes.TrimSuffix(request.Bytes(), []byte("\n")), "headers": headers,
		"forced_provider": c.Provider, "auth_id": c.ID,
	})
	if err != nil {
		var hostErr *HostError
		if errors.As(err, &hostErr) {
			return ModelResponse{}, hostErr.HTTPStatus, err
		}
		return ModelResponse{}, 0, err
	}
	var stream struct {
		ID string `json:"stream_id"`
	}
	if err := json.Unmarshal(raw, &stream); err != nil || stream.ID == "" {
		return ModelResponse{}, 0, fmt.Errorf("CPA did not open a model stream: %s", truncate(string(raw), 200))
	}
	// CPA closes a stream by itself only once it is read to the end; closing
	// it early ends the pending read and cancels the upstream request.
	closeStream := sync.OnceFunc(func() {
		// context.AfterFunc runs this on a goroutine of its own, and OnceFunc
		// would repeat a panic on every later call.
		defer func() { _ = recover() }()
		_, _ = host.Call("host.model.stream_close", map[string]any{"stream_id": stream.ID})
	})
	defer closeStream()
	stop := context.AfterFunc(ctx, closeStream)
	defer stop()
	var events eventBuffer
	for done := false; !done; {
		raw, err := host.Call("host.model.stream_read", map[string]any{"stream_id": stream.ID})
		if err == nil {
			var chunk struct {
				Payload []byte `json:"payload"`
				Error   string `json:"error"`
				Done    bool   `json:"done"`
			}
			switch {
			case json.Unmarshal(raw, &chunk) != nil:
				err = errors.New("模型响应流格式无效")
			case chunk.Error != "":
				err = fmt.Errorf("模型响应流中断：%s", chunk.Error)
			default:
				events.add(chunk.Payload)
				done = chunk.Done
			}
		}
		if err != nil {
			if ctx.Err() != nil {
				err = context.Cause(ctx)
			}
			return ModelResponse{}, 0, err
		}
		// The response did arrive but is not usable: status 200, so that a
		// probe does not ask for it again.
		if events.size() > ModelStreamMaxBytes {
			return ModelResponse{}, http.StatusOK, &AnswerError{fmt.Sprintf("模型响应超过 %d MB，已中断", ModelStreamMaxBytes>>20)}
		}
	}
	out, status, err := parseModelStream(events.events())
	// CPA may end a stream closed for cancellation with a clean done chunk.
	if err != nil && status == 0 && ctx.Err() != nil {
		err = context.Cause(ctx)
	}
	return out, status, err
}

// parseModelStream reads a complete event stream. A final response without a
// usable answer did arrive, so it has status 200 and is not worth repeating;
// any other failure means no complete response arrived (status 0).
func parseModelStream(events []byte) (ModelResponse, int, error) {
	out, err := ParseResponseStream(events)
	var answerErr *AnswerError
	if err != nil && !errors.As(err, &answerErr) {
		return out, 0, err
	}
	return out, http.StatusOK, err
}

// executeProbe sends a collection probe, waiting for one of slots, and
// retries it at most twice, after 2 s and 4 s, when the upstream failed in a
// way worth retrying. A wrong answer is the caller's to judge.
func executeProbe(ctx context.Context, host Host, c Credential, model string, body any, headers http.Header, slots chan struct{}) (out ModelResponse, attempts int, err error) {
	for attempt := 0; attempt < 3; attempt++ {
		if ctx.Err() != nil {
			return out, attempts, ctx.Err()
		}
		select {
		case slots <- struct{}{}:
		case <-ctx.Done():
			return out, attempts, ctx.Err()
		}
		var status int
		func() {
			defer func() { <-slots }()
			attempts++
			requestCtx, cancel := withModelTimeout(ctx, ProbeTimeout)
			defer cancel()
			var response ModelResponse
			response, status, err = executeModel(requestCtx, host, c, model, body, headers)
			out.Answer = response.Answer
			out.InputTokens += response.InputTokens
			out.OutputTokens += response.OutputTokens
			out.ReasoningTokens += response.ReasoningTokens
		}()
		if err == nil || !RetryableModelStatus(status) || attempt == 2 || ctx.Err() != nil {
			return out, attempts, err
		}
		timer := time.NewTimer(time.Duration(2<<attempt) * time.Second)
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			return out, attempts, ctx.Err()
		}
	}
	return out, attempts, err
}

func withModelTimeout(ctx context.Context, limit time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeoutCause(ctx, limit, fmt.Errorf("模型 %d 分钟内未完成回答，已中断请求", int(limit.Minutes())))
}

func truncate(text string, limit int) string {
	runes := []rune(strings.TrimSpace(text))
	if len(runes) <= limit {
		return string(runes)
	}
	return string(runes[:limit]) + "…"
}
