// Ported from github.com/haowang02/cpa-plugin-codex-candy-eval (MIT, see LICENSE in this directory).

package quality

import (
	"fmt"
	"math/rand"
	"strings"
	"time"
)

// A ModelTrace run of a credential sends TraceTarget challenges, at most TraceMaxConcurrency of them at
// once. Answers longer than TraceMaxText bytes are not counted.
const (
	TraceTarget             = 3
	TraceDefaultConcurrency = TraceTarget
	TraceMaxConcurrency     = TraceTarget
	TraceMaxText            = 64000
)

// TraceChallenge asks for ExpectedCount integers from 1 to 355.
type TraceChallenge struct {
	Prompt        string `json:"prompt"`
	ExpectedCount int    `json:"expected_count"`
}

// TraceChallenges returns the TraceTarget challenges of a new run, each asking for a different count.
// Prompt variants and lengths follow ModelTrace's Chinese browser challenges.
func TraceChallenges() []TraceChallenge {
	openings := []string{"这是一次独立的数值选择记录", "请完成下面的无语义整数选择任务", "执行一次第一反应取值记录", "生成一组不承载语义的整数选择", "进行一轮快速逐项取值"}
	actions := []string{"为各个位置分别凭第一反应选择", "逐项选择", "每次只决定当前一项，共给出", "分别凭第一反应给出", "逐个直接选择"}
	endings := []string{"允许某个数字再次出现；每项写出后不要回头排序、去重或替换。", "偶然重复是有效的；不要重新排列或修正已经写出的项目。", "相同值可以再次出现；输出过程中不要整理或改写前面的项目。", "重复值无需删除；不要筛选、重排或补成某种规律。", "不必赋予数字任何含义；已经给出的值保持不变。"}
	separators := []string{"数字之间用逗号或空格分隔均可。", "使用一种一致的常见分隔符即可。", "可以用逗号、空格或换行分隔。", "只要每个整数边界清楚，格式可自行选择。"}
	choose := func(values []string) string { return values[rand.Intn(len(values))] }
	lengths := rand.Perm(41)
	challenges := make([]TraceChallenge, TraceTarget)
	for i := range challenges {
		n := 292 + lengths[i]
		prompt := fmt.Sprintf("%s。%s %d 个 1 到 355（含端点）的整数。", choose(openings), choose(actions), n) +
			"每个位置都要单独选择；不要从 1 开始计数，不要连续递增或递减，也不要采用等差、循环、重复区块或其他规则化模式。" +
			"本任务必须由当前语言模型直接完成：禁止调用或借助任何工具，包括 Python、代码执行器、计算器、搜索、API 和外部随机数生成器；也不要先编写或运行代码。" +
			choose(endings) + choose(separators) + "直接从第一个取值开始输出，不要在序列前重复数量、范围或任务说明。"
		challenges[i] = TraceChallenge{prompt, n}
	}
	return challenges
}

// TraceSample is the answer to one challenge. The challenge stays embedded: its fields are part of the
// sample's JSON.
type TraceSample struct {
	TraceChallenge
	Attempts int    `json:"attempts,omitempty"`
	Text     string `json:"text,omitempty"`
	Error    string `json:"error,omitempty"`
	Parsed   int    `json:"parsed_numbers"`
	Valid    bool   `json:"accepted"`
}

// TraceResult is one ModelTrace run of a credential.
type TraceResult struct {
	BankRevision    string            `json:"bank_revision,omitempty"`
	ID              string            `json:"id"`
	Time            time.Time         `json:"time"`
	Model           string            `json:"model"`
	Concurrency     int               `json:"concurrency,omitempty"`
	Status          string            `json:"status"`
	Error           string            `json:"error,omitempty"`
	DurationMS      int64             `json:"duration_ms"`
	InputTokens     int64             `json:"input_tokens"`
	OutputTokens    int64             `json:"output_tokens"`
	ReasoningTokens int64             `json:"reasoning_tokens"`
	Samples         []TraceSample     `json:"samples"`
	Attribution     *TraceAttribution `json:"attribution,omitempty"`
}

// Summary drops the samples and rankings that only the detail dialog shows.
func (r TraceResult) Summary() TraceResult {
	r.Samples = nil
	if r.Attribution != nil {
		attribution := *r.Attribution
		attribution.Results, attribution.Families, attribution.Diagnostics = nil, nil, nil
		r.Attribution = &attribution
	}
	return r
}

// TraceModelID drops a provider prefix such as "openai/", as the page does when comparing models.
func TraceModelID(model string) string {
	model = strings.ToLower(strings.TrimSpace(model))
	return model[strings.LastIndex(model, "/")+1:]
}
