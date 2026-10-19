// Ported from github.com/haowang02/cpa-plugin-codex-candy-eval (MIT, see LICENSE in this directory).

package quality

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"reflect"
	"strings"
	"testing"
)

func TestModelTraceUpstreamParity(t *testing.T) {
	// Golden probabilities, scores and similarities come from the pinned
	// upstream static/fingerprint-core.js, including 1/2/3-output calibration.
	raw, err := os.ReadFile("testdata/modeltrace-golden.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []struct {
		Outputs  []TraceOutput
		Expected TraceAttribution
	}
	if err = json.Unmarshal(raw, &cases); err != nil {
		t.Fatal(err)
	}
	for _, tt := range cases {
		got, err := AnalyzeModelTrace(tt.Outputs)
		if err != nil {
			t.Fatal(err)
		}
		want := tt.Expected
		if got.Prediction != want.Prediction || got.Used != want.Used || !reflect.DeepEqual(got.Diagnostics, want.Diagnostics) || len(got.Results) != len(want.Results) {
			t.Fatalf("attribution mismatch: %+v", got)
		}
		close := func(a, b float64) {
			t.Helper()
			if math.IsNaN(a) || math.Abs(a-b) > 1e-10 {
				t.Fatalf("got %.15g, want %.15g", a, b)
			}
		}
		close(got.Probability, want.Probability)
		close(got.FamilyProbability, want.FamilyProbability)
		for i, r := range got.Results {
			w := want.Results[i]
			if r.Model != w.Model || r.Family != w.Family {
				t.Fatalf("rank %d: %s, want %s", i, r.Model, w.Model)
			}
			close(r.Probability, w.Probability)
			close(r.Score, w.Score)
			close(r.Similarity, w.Similarity)
		}
		for i, f := range got.Families {
			close(f.Probability, want.Families[i].Probability)
		}
	}
}

func TestModelTraceParseAndThreshold(t *testing.T) {
	for text, want := range map[string][]int{
		"生成 300 个：1, 22, 355, 0, 356, 99。完成 4": {1, 22, 355, 99},
		"1,2 words 3,4": {1, 2},
		"1,2 中文 3,4,5":  {3, 4, 5},
		"1,2 😀 3,4":     {1, 2, 3, 4},
	} {
		if got := TraceParseNumbers(text); !reflect.DeepEqual(got, want) {
			t.Fatalf("parse %q = %v, want %v", text, got, want)
		}
	}
	if _, err := AnalyzeModelTrace([]TraceOutput{{strings.Repeat("42,", 164), 300}}); err == nil {
		t.Fatal("short output was accepted")
	}
	got, err := AnalyzeModelTrace([]TraceOutput{{strings.Repeat("42,", 165), 300}})
	if err != nil || got.Used != 1 {
		t.Fatalf("threshold = %+v, %v", got, err)
	}
	seen := map[int]bool{}
	for _, c := range TraceChallenges() {
		if c.ExpectedCount < 292 || c.ExpectedCount > 332 || seen[c.ExpectedCount] || !strings.Contains(c.Prompt, fmt.Sprintf(" %d 个", c.ExpectedCount)) {
			t.Fatalf("bad challenge: %+v", c)
		}
		seen[c.ExpectedCount] = true
	}
}

// The summary check of candy-eval's ModelTrace run test, on a record with three samples.
func TestModelTraceSummary(t *testing.T) {
	text := strings.Repeat("42,", 320)
	attribution, err := AnalyzeModelTrace([]TraceOutput{{text, 300}, {text, 300}, {text, 300}})
	if err != nil {
		t.Fatal(err)
	}
	r := TraceResult{Samples: make([]TraceSample, TraceTarget), Attribution: &attribution}
	if summary := r.Summary(); summary.Samples != nil || summary.Attribution.Results != nil || summary.Attribution.Prediction != r.Attribution.Prediction ||
		len(r.Samples) != 3 || r.Attribution.Results == nil {
		t.Fatal("summaries must drop details without touching stored records")
	}
}
