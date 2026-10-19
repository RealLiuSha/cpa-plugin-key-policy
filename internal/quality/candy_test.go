// Ported from github.com/haowang02/cpa-plugin-codex-candy-eval (MIT, see LICENSE in this directory).

package quality

import "testing"

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
