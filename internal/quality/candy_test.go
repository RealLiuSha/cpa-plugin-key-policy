// Ported from github.com/haowang02/cpa-plugin-codex-candy-eval (MIT, see LICENSE in this directory).

package quality

import (
	"math/rand"
	"strconv"
	"strings"
	"testing"
)

func TestCandyCorrect(t *testing.T) {
	for text, want := range map[string]bool{
		"21": true, "最少取出**21**个": true, "答案：21。": true,
		"121": false, "210": false, "2 1": false,
	} {
		if got := CandyCorrect(text); got != want {
			t.Errorf("CandyCorrect(%q) = %v, want %v", text, got, want)
		}
	}
}

// candyAnswer agrees with drawing every way the bag allows: on small bags,
// the fewest candies for which no draw of r first-shape and s second-shape
// candies misses both pairs. The published question's answer is 21.
func TestCandyAnswerMatchesExhaustiveDraws(t *testing.T) {
	if got := candyAnswer([2][3]int{{7, 9, 8}, {7, 6, 4}}); got != 21 {
		t.Fatalf("published question = %d, want 21", got)
	}
	draws := func(caps [3]int, total int, visit func([3]int)) {
		for a := 0; a <= caps[0]; a++ {
			for b := 0; b <= caps[1]; b++ {
				if c := total - a - b; c >= 0 && c <= caps[2] {
					visit([3]int{a, b, c})
				}
			}
		}
	}
	guaranteed := func(counts [2][3]int, r, s int) bool {
		ok := true
		draws(counts[0], r, func(x [3]int) {
			draws(counts[1], s, func(y [3]int) {
				if !(x[0] > 0 && y[1] > 0 || x[1] > 0 && y[0] > 0) {
					ok = false
				}
			})
		})
		return ok
	}
	rng := rand.New(rand.NewSource(1))
	for range 60 {
		var counts [2][3]int
		for shape := range counts {
			for kind := range counts[shape] {
				counts[shape][kind] = 1 + rng.Intn(4)
			}
		}
		want := 1 << 30
		for r := 0; r <= counts[0][0]+counts[0][1]+counts[0][2]; r++ {
			for s := 0; s <= counts[1][0]+counts[1][1]+counts[1][2]; s++ {
				if r+s < want && guaranteed(counts, r, s) {
					want = r + s
				}
			}
		}
		if got := candyAnswer(counts); got != want {
			t.Fatalf("candyAnswer(%v) = %d, exhaustive %d", counts, got, want)
		}
	}
}

// Variants keep their answer clear of the usual mistakes and are judged by
// the number after the last 答案, else the last number.
func TestCandyVariantsAndJudging(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	for range 200 {
		v := NewCandyVariant(rng)
		if v.Answer != candyAnswer(v.Counts) || v.Kinds[0] == v.Kinds[1] {
			t.Fatalf("variant %+v", v)
		}
		for _, trap := range candyTraps(v.Counts) {
			if v.Answer+4 > trap {
				t.Fatalf("variant %+v answer %d is near trap %d", v, v.Answer, trap)
			}
		}
		prompt := v.Prompt()
		if !strings.Contains(prompt, v.Kinds[0]+"味和"+v.Kinds[1]+"味") || !strings.Contains(prompt, strconv.Itoa(v.Counts[1][v.Order[2]])) {
			t.Fatalf("prompt of %+v:\n%s", v, prompt)
		}
	}
	for text, want := range map[string]bool{
		"答案：21": true, "所以……\n答案是 \\(21\\)。": true, "最少取出 21 个": true,
		"答案：21\n（9 个圆形加 12 个五角星形）": true,
		"答案：\n21": true, "**答案**：２１": true, "答案：21\n（答案见第3步的推导）": true,
		"先取 21 个不够。\n答案：19": false, "答案：121": false, "需要 29 个": false,
	} {
		if got := candyVariantCorrect(text, 21); got != want {
			t.Errorf("candyVariantCorrect(%q, 21) = %v, want %v", text, got, want)
		}
	}
}
