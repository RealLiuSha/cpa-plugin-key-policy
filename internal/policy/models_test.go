package policy

import (
	"net/url"
	"testing"
)

func TestExtractRequestedModel(t *testing.T) {
	cases := []struct {
		name  string
		path  string
		query url.Values
		body  []byte
		want  string
	}{
		{
			name: "openai body",
			body: []byte(`{"model":"fast","messages":[]}`),
			want: "fast",
		},
		{
			name: "nested body",
			body: []byte(`{"request":{"model":"nested"}}`),
			want: "nested",
		},
		{
			name: "query model",
			query: url.Values{
				"model": []string{"query-model"},
			},
			want: "query-model",
		},
		{
			name: "gemini path",
			path: "/v1beta/models/gemini-2.5-pro:generateContent",
			want: "gemini-2.5-pro",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ExtractRequestedModel(tc.path, tc.query, tc.body); got != tc.want {
				t.Fatalf("ExtractRequestedModel() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestIsModelsEndpoint(t *testing.T) {
	for _, path := range []string{"/v1/models", "/openai/v1/models", "/proxy/v1/models/"} {
		if !IsModelsEndpoint(path) {
			t.Fatalf("IsModelsEndpoint(%q) = false", path)
		}
	}
}

func TestRewriteTopLevelModel(t *testing.T) {
	got, ok := RewriteTopLevelModel([]byte(`{"id":"1","model":"upstream"}`), "public-model")
	if !ok {
		t.Fatal("RewriteTopLevelModel() changed = false")
	}
	if string(got) != `{"id":"1","model":"public-model"}` {
		t.Fatalf("RewriteTopLevelModel() = %s", got)
	}
}
