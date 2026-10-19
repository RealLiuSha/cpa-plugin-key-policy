// Ported from github.com/haowang02/cpa-plugin-codex-candy-eval (MIT, see LICENSE in this directory).

package quality

import "time"

// CandyMaxRuns is the most candy questions one run asks each credential.
const CandyMaxRuns = 10

// CandyPrompt is the candy question; CandyCorrect judges the answer.
const CandyPrompt = `不使用任何外部工具回答以下问题：

在一个黑色的袋子里放有三种口味的糖果，每种糖果有两种不同的形状（圆形和五角星形，不同的形状靠手感可以分辨）。现已知不同口味的糖和不同形状的数量统计如下表。参赛者需要在活动前决定摸出的糖果数目，那么，最少取出多少个糖果才能保证手中同时拥有不同形状的苹果味和桃子味的糖？（同时手中有圆形苹果味匹配五角星桃子味糖果，或者有圆形桃子味匹配五角星苹果味糖果都满足要求）

        苹果味  桃子味  西瓜味
圆形       7      9      8
五角星形   7      6      4
`

// CandyResult is one candy question asked of a credential.
type CandyResult struct {
	Skipped         bool      `json:"skipped,omitempty"`
	Time            time.Time `json:"time"`
	Model           string    `json:"model"`
	Effort          string    `json:"effort"`
	OK              bool      `json:"ok"`
	Answer          string    `json:"answer,omitempty"`
	Error           string    `json:"error,omitempty"`
	InputTokens     int64     `json:"input_tokens"`
	OutputTokens    int64     `json:"output_tokens"`
	ReasoningTokens int64     `json:"reasoning_tokens"`
	DurationMS      int64     `json:"duration_ms"`
}

// Summary drops the answer, which the detail dialog loads per record.
func (r CandyResult) Summary() CandyResult {
	r.Answer = ""
	return r
}

// CandyCorrect reports whether answer gives the right count: "21" appears without adjacent digits.
func CandyCorrect(answer string) bool {
	isDigit := func(i int) bool { return i >= 0 && i < len(answer) && answer[i] >= '0' && answer[i] <= '9' }
	for i := 0; i+1 < len(answer); i++ {
		if answer[i] == '2' && answer[i+1] == '1' && !isDigit(i-1) && !isDigit(i+2) {
			return true
		}
	}
	return false
}
