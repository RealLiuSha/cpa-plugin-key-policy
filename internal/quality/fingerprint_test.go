// Ported from github.com/haowang02/cpa-plugin-codex-candy-eval (MIT, see LICENSE in this directory).

package quality

import (
	"encoding/json"
	"math"
	"slices"
	"testing"
)

func TestFingerprintNormalization(t *testing.T) {
	for _, tc := range []struct{ kind, raw, answer, category string }{
		{"int", "**４７**", "47", "valid"}, {"int", "٤٧", "47", "valid"}, {"int", "۴۷", "47", "valid"},
		{"int", "四十七。", "47", "valid"}, {"int", "一百", "100", "valid"}, {"int", "十", "10", "valid"},
		{"int", "forty-seven", "47", "valid"}, {"int", "one hundred", "1", "valid"},
		{"int", "101", "101", "invalid"}, {"int", "0", "0", "invalid"}, {"int", "A number: 47", "a", "invalid"},
		{"letter", "Zed!", "z", "valid"}, {"letter", "QUEUE", "q", "valid"}, {"letter", "abc", "abc", "invalid"},
		{"color", "Grey", "gray", "valid"}, {"color", "蓝色", "蓝", "valid"}, {"color", "Aqua", "cyan", "valid"},
		{"coin", "正面！", "heads", "valid"}, {"coin", "花", "tails", "valid"}, {"coin", "HEAD", "heads", "valid"},
		{"word", "Snow leopard", "snow", "valid"}, {"word", "“上海”", "上海", "valid"},
		{"word", "\u00e9", "é", "invalid"}, {"word", "🦊", "", "empty"}, {"int", "", "", "empty"},
		// candy-eval composes "e\u0301" to "é" with norm.NFC; without that step the combining mark is dropped.
		{"word", "e\u0301", "e", "valid"},
		{"int", "As an AI, I cannot pick", "", "refusal"}, {"word", "抱歉，不能回答", "", "refusal"},
	} {
		t.Run(tc.kind+"/"+tc.raw, func(t *testing.T) {
			a, c := NormalizeFingerprintAnswer(tc.raw, FingerprintProbe{Kind: tc.kind, Lo: 1, Hi: 100})
			if a != tc.answer || c != tc.category {
				t.Fatalf("got (%q, %q), want (%q, %q)", a, c, tc.answer, tc.category)
			}
		})
	}
}

func repeatedFingerprint(answer string, n int) map[string][]string {
	cells := map[string][]string{}
	for _, probe := range FingerprintProbes[:4] {
		for range n {
			cells[probe.ID] = append(cells[probe.ID], answer)
		}
	}
	return cells
}

func TestFingerprintStatistics(t *testing.T) {
	if got := fingerprintJSD([]string{"a", "a"}, []string{"b"}); got != 1 {
		t.Fatalf("disjoint JSD = %v", got)
	}
	if got := fingerprintJSD([]string{"a", "b"}, []string{"a", "a", "b", "b"}); got != 0 {
		t.Fatalf("equal distributions JSD = %v", got)
	}
	if got := fingerprintJSD([]string{"a"}, []string{"a", "b"}); math.Abs(got-.31127812445913283) > 1e-12 {
		t.Fatalf("JSD = %v", got)
	}
	a, b := repeatedFingerprint("a", 25), repeatedFingerprint("b", 25)
	entries, mean := compareFingerprintCells(a, b)
	if len(entries) != 4 || mean == nil || *mean != 1 {
		t.Fatalf("comparison = %v %v", entries, mean)
	}
	if p := fingerprintPermutation(a, b, "different"); p == nil || *p > .004 {
		t.Fatalf("disjoint p = %v", p)
	}
	if p := fingerprintPermutation(a, a, "equal"); p == nil || *p != 1 {
		t.Fatalf("identical p = %v", p)
	}
	if d := fingerprintSplitHalf(a); d == nil || *d != 0 {
		t.Fatalf("self JSD = %v", d)
	}
	delete(a, FingerprintProbes[0].ID)
	if _, mean := compareFingerprintCells(a, b); mean != nil {
		t.Fatal("three probes must not produce attribution")
	}
	if _, mean := compareFingerprintCells(repeatedFingerprint("a", 9), b); mean != nil {
		t.Fatal("nine samples must not qualify")
	}
}

func TestFingerprintAttribution(t *testing.T) {
	a, b := repeatedFingerprint("a", 25), repeatedFingerprint("b", 25)
	baselines := []fingerprintBaseline{{Model: "original", Cells: a}, {Model: "substitute", Cells: b}}
	for _, tc := range []struct {
		model   string
		samples map[string][]string
		status  string
	}{
		{"original", a, "consistent"}, {"original", b, "substitution"},
		{"original", repeatedFingerprint("c", 25), "different"}, {"unknown", a, "no_baseline"},
		{"original", repeatedFingerprint("a", 9), "insufficient"},
	} {
		if r := attributeFingerprintWith(tc.model, tc.samples, baselines); r.Status != tc.status {
			t.Fatalf("%s: got %+v, want %s", tc.model, r, tc.status)
		}
	}
	baselines = []fingerprintBaseline{{Model: "other", Cells: a}, {Model: "original", Cells: a}}
	if r := attributeFingerprintWith("original", a, baselines); r.Status != "ambiguous" {
		t.Fatalf("indistinguishable references: %+v", r)
	}
}

func TestFingerprintBaselineCoverage(t *testing.T) {
	if len(FingerprintProbes) != 16 {
		t.Fatal("invalid embedded data")
	}
	seen := map[string]bool{}
	for _, b := range fingerprintBaselines {
		if b.Model == "" || seen[b.Model] || len(b.Cells) != len(FingerprintProbes) {
			t.Fatalf("invalid/duplicate baseline %s", b.Model)
		}
		seen[b.Model] = true
		for _, probe := range FingerprintProbes {
			if len(b.Cells[probe.ID]) < fingerprintMinValid {
				t.Errorf("%s %s: insufficient baseline answers: %d", b.Model, probe.ID, len(b.Cells[probe.ID]))
			}
		}
	}
	if len(seen) == 0 {
		t.Fatal("missing baselines")
	}
}

// The probe body candy-eval's execution contract test checked, and the answer it collected.
func TestFingerprintRequest(t *testing.T) {
	probe := FingerprintProbes[0]
	raw, err := json.Marshal(FingerprintRequest("gpt-5.5", probe))
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	_ = json.Unmarshal(raw, &body)
	input, _ := body["input"].(string)
	if body["reasoning"].(map[string]any)["effort"] != "low" || body["temperature"] != 1.0 || body["store"] != false || body["instructions"] != probe.Instructions ||
		!slices.Contains(probe.Prompts, input) {
		t.Fatalf("wrong probe body: %v", body)
	}
	if answer, category := NormalizeFingerprintAnswer("47", probe); category != "valid" || answer != "47" {
		t.Fatalf("sample = %q, %q", answer, category)
	}
}

func TestFingerprintMissingUsage(t *testing.T) {
	var r FingerprintResult
	if err := json.Unmarshal([]byte(`{"status":"completed","done":60}`), &r); err != nil {
		t.Fatal(err)
	}
	if r.InputTokens != nil || r.OutputTokens != nil || r.ReasoningTokens != nil {
		t.Fatal("missing usage must stay unknown")
	}
}
