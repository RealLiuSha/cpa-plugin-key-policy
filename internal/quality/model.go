// Ported from github.com/haowang02/cpa-plugin-codex-candy-eval (MIT, see LICENSE in this directory).

package quality

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"
	"unicode"
)

// ModelResponse is the answer and token usage of a model request.
type ModelResponse struct {
	Answer          string
	InputTokens     int64
	OutputTokens    int64
	ReasoningTokens int64
}

// ValidModelName reports whether model may be requested. Unless allowEffort, it must not carry a
// reasoning effort suffix such as "(high)": any parenthesis counts as one.
func ValidModelName(model string, allowEffort bool) bool {
	return model != "" && len(model) <= 200 && strings.IndexFunc(model, unicode.IsControl) < 0 &&
		(allowEffort || !strings.ContainsAny(model, "()"))
}

// RetryableModelStatus reports whether a request that failed with status is worth repeating; status 0
// means no complete response arrived.
func RetryableModelStatus(status int) bool {
	return status == 0 || status == http.StatusTooManyRequests || status >= http.StatusInternalServerError
}

// Time limits of a model request, as a model stuck repeating itself streams until the upstream cuts it off.
// Probes normally answer within a minute.
const (
	ProbeTimeout  = 3 * time.Minute
	AnswerTimeout = 10 * time.Minute
)

// AnswerError is the error of a final response that holds no usable answer: unfinished, failed, only
// calling tools, or without text. The response did arrive, so the request has HTTP status 200 and is not
// worth repeating. Any other error of ParseResponseStream means no complete response arrived (status 0).
type AnswerError struct{ message string }

func (e *AnswerError) Error() string { return e.message }

// ParseResponseStream returns the answer and token usage in the complete event stream of a streaming
// Responses request: the payloads of all its chunks, joined as StartsSSEField describes. Usage comes
// from the final response, so it is also reported with an *AnswerError.
func ParseResponseStream(events []byte) (ModelResponse, error) {
	var result ModelResponse
	out, err := parseResponseEvents(events)
	if err != nil {
		return result, err
	}
	result.InputTokens = out.Usage.InputTokens
	result.OutputTokens = out.Usage.OutputTokens
	result.ReasoningTokens = out.Usage.OutputTokensDetails.ReasoningTokens
	if (out.Status != "" && out.Status != "completed") || (len(out.Error) > 0 && string(out.Error) != "null") {
		return result, &AnswerError{fmt.Sprintf("模型响应未完成（%s）：%s", out.Status, out.Error)}
	}
	var answer strings.Builder
	for _, item := range out.Output {
		if item.Type != "message" {
			continue
		}
		for _, part := range item.Content {
			if part.Type == "output_text" || part.Type == "text" {
				answer.WriteString(part.Text)
			}
		}
	}
	result.Answer = answer.String()
	if strings.TrimSpace(result.Answer) == "" {
		// Codex turns list tools, and items such as function_call or custom_tool_call request one.
		if slices.ContainsFunc(out.Output, func(item responseItem) bool { return strings.HasSuffix(item.Type, "_call") }) {
			return result, &AnswerError{"模型调用了工具，没有直接回答"}
		}
		return result, &AnswerError{"模型未返回文本"}
	}
	return result, nil
}

type responseItem struct {
	Type    string `json:"type"`
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
}

type responseBody struct {
	Status string          `json:"status"`
	Error  json.RawMessage `json:"error"`
	Output []responseItem  `json:"output"`
	Usage  struct {
		InputTokens         int64 `json:"input_tokens"`
		OutputTokens        int64 `json:"output_tokens"`
		OutputTokensDetails struct {
			ReasoningTokens int64 `json:"reasoning_tokens"`
		} `json:"output_tokens_details"`
	} `json:"usage"`
}

// parseResponseEvents returns the final response of a complete Responses event stream.
func parseResponseEvents(events []byte) (out responseBody, err error) {
	var items []responseItem
	final := false
	for _, line := range bytes.Split(events, []byte("\n")) {
		data, ok := bytes.CutPrefix(bytes.TrimSpace(line), []byte("data:"))
		if !ok {
			continue
		}
		var event struct {
			Type     string          `json:"type"`
			Response json.RawMessage `json:"response"`
			Item     json.RawMessage `json:"item"`
		}
		if json.Unmarshal(data, &event) != nil {
			continue
		}
		switch event.Type {
		case "response.output_item.done":
			var item responseItem
			if json.Unmarshal(event.Item, &item) == nil {
				items = append(items, item)
			}
		case "response.completed", "response.incomplete", "response.failed":
			if err := json.Unmarshal(event.Response, &out); err != nil {
				return out, fmt.Errorf("解析模型响应失败：%w", err)
			}
			final = true
		case "error":
			return out, fmt.Errorf("模型返回错误：%s", bytes.TrimSpace(data))
		}
	}
	if !final {
		return out, fmt.Errorf("模型响应流在完成前中断")
	}
	// Codex streams the output items but may leave the final response's output empty.
	if len(out.Output) == 0 {
		out.Output = items
	}
	return out, nil
}

// StartsSSEField reports whether a stream chunk begins with an event-stream field. A chunk may end
// mid-line, or start the next field without a line break, so a reader joining chunk payloads puts a
// "\n" before a chunk that starts a field when the stream read so far does not end with one.
func StartsSSEField(chunk []byte) bool {
	chunk = bytes.TrimLeft(chunk, " \t")
	for _, field := range []string{"data:", "event:", "id:", "retry:", ":"} {
		if bytes.HasPrefix(chunk, []byte(field)) {
			return true
		}
	}
	return false
}

// ModelStreamMaxBytes caps the events a model request keeps; a model stuck repeating itself is cut off
// here.
const ModelStreamMaxBytes = 16 << 20

// eventBuffer joins chunk payloads as StartsSSEField describes and keeps only the lines
// parseResponseEvents reads, not the deltas. A non-empty line is a stream read so far that does not end
// with "\n".
type eventBuffer struct{ kept, line []byte }

func (b *eventBuffer) add(chunk []byte) {
	if len(b.line) > 0 && StartsSSEField(chunk) {
		b.flush()
	}
	for {
		i := bytes.IndexByte(chunk, '\n')
		if i < 0 {
			b.line = append(b.line, chunk...)
			return
		}
		b.line = append(b.line, chunk[:i]...)
		b.flush()
		chunk = chunk[i+1:]
	}
}

func (b *eventBuffer) flush() {
	if data, ok := bytes.CutPrefix(bytes.TrimSpace(b.line), []byte("data:")); ok {
		var event struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(data, &event) == nil {
			switch event.Type {
			case "response.output_item.done", "response.completed", "response.incomplete", "response.failed", "error":
				b.kept = append(append(b.kept, b.line...), '\n')
			}
		}
	}
	b.line = b.line[:0]
}

func (b *eventBuffer) size() int { return len(b.kept) + len(b.line) }

func (b *eventBuffer) events() []byte {
	b.flush()
	return b.kept
}
