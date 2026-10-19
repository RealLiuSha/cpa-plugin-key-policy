// Ported from github.com/haowang02/cpa-plugin-codex-candy-eval (MIT, see LICENSE in this directory).

package quality

import (
	"errors"
	"strings"
	"testing"
)

// The checks every run request made before reaching the host: candy and pelican accept an effort suffix,
// fingerprint and ModelTrace do not.
func TestValidModelName(t *testing.T) {
	for _, model := range []string{"", strings.Repeat("m", 201), "model\x00name", "model\nname"} {
		if ValidModelName(model, true) || ValidModelName(model, false) {
			t.Fatalf("accepted invalid model %q", model)
		}
	}
	for _, model := range []string{"gpt-5.5(high)", "m(high)"} {
		if ValidModelName(model, false) || !ValidModelName(model, true) {
			t.Fatalf("effort suffix of %q", model)
		}
	}
	if !ValidModelName("gpt-5.5", false) {
		t.Fatal("rejected a plain model name")
	}
}

func TestParseResponseStream(t *testing.T) {
	// The stream reader joins chunk payloads this way before parsing them.
	join := func(chunks []string) []byte {
		var events eventBuffer
		for _, chunk := range chunks {
			events.add([]byte(chunk))
		}
		return events.events()
	}
	usage := `"usage":{"input_tokens":5,"output_tokens":2,"output_tokens_details":{"reasoning_tokens":1}}`
	answer := func(text string) string {
		return `{"type":"message","content":[{"type":"output_text","text":"` + text + `"}]}`
	}
	final := func(event, status, output string) string {
		return `data: {"type":"` + event + `","response":{"status":"` + status + `","output":[` + output + `],` + usage + `}}`
	}
	for _, tc := range []struct {
		name, answer string
		chunks       []string
		status       int
		err          string
	}{
		// Chunks split lines and skip line breaks; the answer arrives only as output items.
		{"split", "答案是 21", []string{
			`event: response.created`, `data: {"type":"response.created"}`,
			`data: {"type":"response.output_item.done","item":{"type":"reasoning"}}`,
			`data: {"type":"response.output_item.done","item":{"type":"message","content":[{"type":"output_text","text":"答案是 "},{"type":"output_text","text":"2`,
			`1"}]}}`, final("response.completed", "completed", ""),
		}, 200, ""},
		{"incomplete", "", []string{final("response.incomplete", "incomplete", answer("21"))}, 200, ""},
		{"failed", "", []string{final("response.failed", "failed", "")}, 200, ""},
		{"blank", "", []string{final("response.completed", "completed", answer(` \n`))}, 200, ""},
		// A turn that only requests a tool, as in candy-eval's pelican run test.
		{"tool_call", "", []string{final("response.completed", "completed", `{"type":"custom_tool_call"}`)}, 200, "模型调用了工具，没有直接回答"},
		{"error_event", "", []string{`data: {"type":"error","message":"quota exhausted"}`}, 0, ""},
		{"unfinished", "", []string{`data: {"type":"response.output_text.delta","delta":"21"}`}, 0, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, err := ParseResponseStream(join(tc.chunks))
			// An *AnswerError comes with a final response (status 200); other errors mean none arrived (0).
			status := 0
			var answerErr *AnswerError
			if err == nil || errors.As(err, &answerErr) {
				status = 200
			}
			if (err == nil) != (tc.answer != "") || (err == nil && out.Answer != tc.answer) || status != tc.status ||
				(tc.err != "" && (err == nil || err.Error() != tc.err)) {
				t.Fatalf("answer=%q status=%d err=%v", out.Answer, status, err)
			}
			// Any final response reports usage, complete or not.
			if got := [3]int64{out.InputTokens, out.OutputTokens, out.ReasoningTokens}; (got == [3]int64{5, 2, 1}) != (status == 200) {
				t.Fatalf("usage = %v", got)
			}
		})
	}
}
