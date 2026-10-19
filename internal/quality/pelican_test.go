// Ported from github.com/haowang02/cpa-plugin-codex-candy-eval (MIT, see LICENSE in this directory).

package quality

import "testing"

func TestExtractHTML(t *testing.T) {
	page := "<!doctype html><svg></svg>"
	for answer, want := range map[string]string{
		"说明\n```css\nbody {}\n```\n```HTML\n" + page + "\n```": page,
		"```\n" + page + "\n```":                               page,
		"  " + page + "\n":                                     page,
		"```js\nlet svg = 1\n```\n" + page:                     "",
		"下面是页面：" + page:                                        "",
	} {
		if got := ExtractHTML(answer); got != want {
			t.Errorf("ExtractHTML(%q) = %q, want %q", answer, got, want)
		}
	}
}

// The page and summary checks of candy-eval's pelican run test: lists carry summaries, and the page loads
// with its record.
func TestPelicanSummary(t *testing.T) {
	r := PelicanResult{ID: "1", HTML: ExtractHTML("好的：\n```html\n<svg></svg>\n```")}
	if summary := r.Summary(); r.HTML != "<svg></svg>" || summary.HTML != "" || summary.ID != r.ID {
		t.Fatalf("record = %+v, summary = %+v", r, summary)
	}
}
