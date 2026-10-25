package native

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// Probe limits. A probe asks for one search through CPA, pinned to the
// channel, the way Codex declares the tool when it does not use Responses
// Lite.
const (
	probeTimeout     = 2 * time.Minute
	probeConcurrency = 4
	// probeUserAgent is a Codex client's, which upstreams behind Cloudflare
	// accept; CPA sends its own cloaked one where it applies.
	probeUserAgent = "codex-tui/0.160.0 (Ubuntu 26.4.0; aarch64) ghostty/1.3.1 (codex-tui; 0.160.0)"
	probePrompt    = "Use web search to look up the latest stable Go release version. Reply with the version number only."
	errorLimit     = 300
)

// Probe is one probe of a channel: when, which model, and what came back.
type Probe struct {
	At           time.Time `json:"at"`
	Model        string    `json:"model"`
	Result       Result    `json:"result"`
	Status       int       `json:"status,omitempty"`
	Error        string    `json:"error,omitempty"`
	LatencyMS    int64     `json:"latency_ms"`
	Searches     int       `json:"searches,omitempty"`
	InputTokens  int64     `json:"input_tokens,omitempty"`
	OutputTokens int64     `json:"output_tokens,omitempty"`
}

// probeRequest is a minimal Responses request with the hosted web_search tool
// as Codex declares it by default (cached results, text and images).
func probeRequest(model string) (any, http.Header) {
	session := newSessionID()
	body := map[string]any{
		"model":        model,
		"instructions": "",
		"input": []any{map[string]any{
			"type": "message", "role": "user",
			"content": []any{map[string]any{"type": "input_text", "text": probePrompt}},
		}},
		"tools":               []any{map[string]any{"type": "web_search", "external_web_access": false, "search_content_types": []string{"text", "image"}}},
		"tool_choice":         "auto",
		"parallel_tool_calls": false,
		"reasoning":           map[string]any{"effort": "low"},
		"store":               false,
		"stream":              true,
		"include":             []string{},
	}
	headers := http.Header{
		"Accept":       {"text/event-stream"},
		"Content-Type": {"application/json"},
		"Originator":   {"codex-tui"},
		"User-Agent":   {probeUserAgent},
		"Session-Id":   {session},
	}
	return body, headers
}

// probeRetryDelay is the pause before a probe's second attempt; a variable
// only so that tests need not wait.
var probeRetryDelay = 3 * time.Second

func newSessionID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6], b[8] = b[6]&0x0f|0x40, b[8]&0x3f|0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[:4], b[4:6], b[6:8], b[8:10], b[10:])
}

// readEvents classifies a complete event stream: a completed response that ran
// the tool searched, one that did not ignored it, and anything else is an
// error with the upstream's message.
func readEvents(events []byte) (result Result, searches int, input, output int64, message string) {
	final := false
	for _, line := range bytes.Split(events, []byte("\n")) {
		data, ok := bytes.CutPrefix(bytes.TrimSpace(line), []byte("data:"))
		if !ok {
			continue
		}
		var event struct {
			Type string `json:"type"`
			Item struct {
				Type string `json:"type"`
			} `json:"item"`
			Response struct {
				Status            string          `json:"status"`
				Error             json.RawMessage `json:"error"`
				IncompleteDetails json.RawMessage `json:"incomplete_details"`
				Usage             struct {
					InputTokens  int64 `json:"input_tokens"`
					OutputTokens int64 `json:"output_tokens"`
				} `json:"usage"`
			} `json:"response"`
		}
		if json.Unmarshal(bytes.TrimSpace(data), &event) != nil {
			continue
		}
		switch event.Type {
		case "response.output_item.done":
			if event.Item.Type == "web_search_call" {
				searches++
			}
		case "response.completed", "response.incomplete", "response.failed":
			final = true
			input, output = event.Response.Usage.InputTokens, event.Response.Usage.OutputTokens
			if event.Response.Status != "" && event.Response.Status != "completed" {
				detail := event.Response.Error
				if len(detail) == 0 || string(detail) == "null" {
					detail = event.Response.IncompleteDetails
				}
				return ResultError, searches, input, output, fmt.Sprintf("response %s: %s", event.Response.Status, detail)
			}
		case "error":
			return ResultError, searches, input, output, "upstream error: " + string(bytes.TrimSpace(data))
		}
	}
	switch {
	case !final:
		return ResultError, searches, input, output, "the stream ended before the response completed"
	case searches > 0:
		return ResultSearch, searches, input, output, ""
	}
	return ResultNoSearch, 0, input, output, ""
}

// probeOutcome says what became of one probe attempt.
type probeOutcome int

const (
	outcomeRecorded probeOutcome = iota
	// outcomeNotServing: CPA has no route to the model on this channel.
	outcomeNotServing
	// outcomeCancelled: the round was stopped; nothing is learned.
	outcomeCancelled
)

// retryable reports a failure worth one more try: no complete response
// (status 0), 429 or a server error.
func retryable(status int) bool {
	return status == 0 || status == http.StatusTooManyRequests || status >= http.StatusInternalServerError
}

// probeChannel asks channel c for one search with model, retrying once after
// a failure worth repeating.
func probeChannel(ctx context.Context, upstream Upstream, c Credential, model string, now func() time.Time) (Probe, probeOutcome) {
	var probe Probe
	for attempt := 0; attempt < 2; attempt++ {
		started := now()
		body, headers := probeRequest(model)
		requestCtx, cancel := context.WithTimeoutCause(ctx, probeTimeout, fmt.Errorf("no answer within %s", probeTimeout))
		events, status, err := upstream.Stream(requestCtx, c, model, body, headers)
		cause := context.Cause(requestCtx)
		cancel()
		if ctx.Err() != nil {
			return Probe{}, outcomeCancelled
		}
		if errors.Is(err, ErrNotServing) {
			return Probe{}, outcomeNotServing
		}
		probe = Probe{At: started.UTC(), Model: model, Status: status, LatencyMS: now().Sub(started).Milliseconds()}
		if err != nil {
			if cause != nil && errors.Is(err, context.DeadlineExceeded) {
				err = cause
			}
			probe.Result, probe.Error = ResultError, truncate(err.Error())
		} else {
			var message string
			probe.Result, probe.Searches, probe.InputTokens, probe.OutputTokens, message = readEvents(events)
			probe.Error = truncate(message)
		}
		if probe.Result != ResultError || !retryable(probe.Status) || attempt == 1 {
			return probe, outcomeRecorded
		}
		timer := time.NewTimer(probeRetryDelay)
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			return Probe{}, outcomeCancelled
		}
	}
	return probe, outcomeRecorded
}

func truncate(text string) string {
	text = strings.TrimSpace(text)
	runes := []rune(text)
	if len(runes) <= errorLimit {
		return text
	}
	return string(runes[:errorLimit]) + "…"
}
