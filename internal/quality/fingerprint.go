// Ported from github.com/haowang02/cpa-plugin-codex-candy-eval (MIT, see LICENSE in this directory).

package quality

import (
	"math/rand"
	"time"
)

// Probe requests a fingerprint collection runs at once per credential. FingerprintMaxConcurrency also
// caps the probe requests of all collections together.
const (
	FingerprintDefaultConcurrency = 2
	FingerprintMaxConcurrency     = 6
)

// FingerprintMode asks each of the first Cells of FingerprintProbes Samples times.
type FingerprintMode struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Cells   int    `json:"cells"`
	Samples int    `json:"samples_per_cell"`
}

// FingerprintModes are the collection sizes a run can choose.
var FingerprintModes = []FingerprintMode{{"quick", "快速", 4, 15}, {"standard", "标准", 8, 25}, {"strict", "严格", 16, 25}}

// FingerprintResult is one fingerprint collection of a credential.
type FingerprintResult struct {
	ID              string                 `json:"id"`
	Time            time.Time              `json:"time"`
	Model           string                 `json:"model"`
	Mode            string                 `json:"mode"`
	Concurrency     int                    `json:"concurrency,omitempty"`
	Effort          string                 `json:"effort"`
	Status          string                 `json:"status"`
	Total           int                    `json:"total"`
	Done            int                    `json:"done"`
	Valid           int                    `json:"valid"`
	Errors          int                    `json:"errors"`
	DurationMS      int64                  `json:"duration_ms"`
	InputTokens     *int64                 `json:"input_tokens"`
	OutputTokens    *int64                 `json:"output_tokens"`
	ReasoningTokens *int64                 `json:"reasoning_tokens"`
	Error           string                 `json:"error,omitempty"`
	Attribution     FingerprintAttribution `json:"attribution"`
}

// Summary drops the comparison tables that only the detail dialog shows.
func (r FingerprintResult) Summary() FingerprintResult {
	r.Attribution.Comparisons, r.Attribution.Warnings = nil, nil
	return r
}

// FingerprintRequest returns the Responses body of one sample of probe: one of its prompts, chosen at
// random. It is sent without extra headers.
func FingerprintRequest(model string, probe FingerprintProbe) any {
	prompt := probe.Prompts[rand.Intn(len(probe.Prompts))]
	return map[string]any{
		"model": model, "stream": true,
		"instructions": probe.Instructions, "input": prompt,
		"temperature": 1.0, "reasoning": map[string]string{"effort": "low"},
		"store": false,
	}
}
