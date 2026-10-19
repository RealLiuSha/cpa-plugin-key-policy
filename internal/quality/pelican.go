// Ported from github.com/haowang02/cpa-plugin-codex-candy-eval (MIT, see LICENSE in this directory).

package quality

import (
	"regexp"
	"strings"
	"time"
)

// PelicanMaxHTML is the largest HTML page, in bytes, a pelican result keeps.
const PelicanMaxHTML = 256 << 10

// PelicanPrompt is Simon Willison's pelican-on-a-bicycle test, framed so a Codex model answers in text
// instead of writing a file.
const PelicanPrompt = "不使用任何工具，也不要创建或修改文件，直接在回复中完成以下任务：\n\n" +
	"创建一个 HTML，内容是 SVG 绘制一个鹈鹕骑自行车的 2D 动画，你不需要任何测试，不要使用外部资产。\n\n" +
	"在一个 ```html 代码块中给出完整的 HTML 文件。"

// PelicanResult is one pelican page asked of a credential.
type PelicanResult struct {
	ID              string    `json:"id"`
	Time            time.Time `json:"time"`
	Model           string    `json:"model"`
	Effort          string    `json:"effort"`
	Skipped         bool      `json:"skipped,omitempty"`
	HTML            string    `json:"html,omitempty"`
	Error           string    `json:"error,omitempty"`
	InputTokens     int64     `json:"input_tokens"`
	OutputTokens    int64     `json:"output_tokens"`
	ReasoningTokens int64     `json:"reasoning_tokens"`
	DurationMS      int64     `json:"duration_ms"`
}

// Summary drops the HTML, which previews load per record.
func (r PelicanResult) Summary() PelicanResult {
	r.HTML = ""
	return r
}

var codeBlock = regexp.MustCompile("(?s)```[ \\t]*([\\w-]*)[^\\n]*\\n(.*?)\\n[ \\t]*```")

// ExtractHTML returns the HTML document in a model answer: its html code block, else the first code block
// with markup, else the answer itself when it is bare markup.
func ExtractHTML(answer string) string {
	blocks := codeBlock.FindAllStringSubmatch(answer, -1)
	for _, block := range blocks {
		if strings.EqualFold(block[1], "html") {
			return strings.TrimSpace(block[2])
		}
	}
	for _, block := range blocks {
		if code := strings.TrimSpace(block[2]); strings.HasPrefix(code, "<") {
			return code
		}
	}
	if text := strings.TrimSpace(answer); len(blocks) == 0 && strings.HasPrefix(text, "<") {
		return text
	}
	return ""
}
