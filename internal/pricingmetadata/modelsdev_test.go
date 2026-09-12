package pricingmetadata

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestPreviewMatchesPrefixExactOfficialProvider(t *testing.T) {
	preview := previewFixture(t, map[string]any{
		"openai": map[string]any{
			"id": "openai", "name": "OpenAI",
			"models": map[string]any{
				"gpt-4o": map[string]any{"id": "gpt-4o", "name": "GPT-4o", "cost": map[string]any{"input": 2.5, "output": 10, "cache_read": 1.25, "cache_write": 3.75}},
			},
		},
		"openrouter": map[string]any{
			"id": "openrouter", "name": "OpenRouter",
			"models": map[string]any{
				"gpt-4o": map[string]any{"id": "gpt-4o", "name": "GPT-4o", "cost": map[string]any{"input": 3, "output": 12}},
			},
		},
	}, []string{"cpa/gpt-4o"})
	if len(preview.Matches) != 1 {
		t.Fatalf("matches = %+v", preview.Matches)
	}
	match := preview.Matches[0]
	if match.MatchType != "index_suffix" || match.SourceProviderID != "openai" || match.PromptPricePer1M != 2.5 || match.CacheWritePricePer1M == nil || *match.CacheWritePricePer1M != 3.75 {
		t.Fatalf("match = %+v", match)
	}
}

func TestPreviewNormalizedMatch(t *testing.T) {
	preview := previewFixture(t, map[string]any{
		"anthropic": map[string]any{
			"id": "anthropic", "name": "Anthropic",
			"models": map[string]any{
				"claude-sonnet-4.6": map[string]any{"id": "claude-sonnet-4.6", "cost": map[string]any{"input": 3, "output": 15, "cache_read": 0.3}},
			},
		},
	}, []string{"claude_sonnet_4_6"})
	if len(preview.Matches) != 1 || preview.Matches[0].MatchType != "index_normalized" {
		t.Fatalf("normalized match = %+v", preview.Matches)
	}
}

func TestPreviewAvoidsPlanZeroAndDeprecated(t *testing.T) {
	preview := previewFixture(t, map[string]any{
		"zai-coding-plan": map[string]any{
			"id": "zai-coding-plan", "name": "ZAI Plan",
			"models": map[string]any{
				"glm-4.5": map[string]any{"id": "glm-4.5", "cost": map[string]any{"input": 0, "output": 0}},
			},
		},
		"zai": map[string]any{
			"id": "zai", "name": "ZAI",
			"models": map[string]any{
				"glm-4.5":     map[string]any{"id": "glm-4.5", "status": "deprecated", "last_updated": "2024-01-01", "cost": map[string]any{"input": 1, "output": 2}},
				"glm-4.5-new": map[string]any{"id": "glm-4.5", "last_updated": "2026-01-01", "cost": map[string]any{"input": 0.5, "output": 1.5}},
			},
		},
	}, []string{"glm-4.5"})
	if len(preview.Matches) != 1 {
		t.Fatalf("matches = %+v", preview.Matches)
	}
	match := preview.Matches[0]
	if match.SourceProviderID != "zai" || match.PromptPricePer1M != 0.5 {
		t.Fatalf("expected official non-plan non-deprecated price, got %+v", match)
	}
}

func TestPreviewPreservesMissingAndExplicitZeroCacheWrite(t *testing.T) {
	preview := previewFixture(t, map[string]any{
		"openai": map[string]any{
			"id": "openai", "name": "OpenAI",
			"models": map[string]any{
				"gpt-mini":   map[string]any{"id": "gpt-mini", "cost": map[string]any{"input": 0.15, "output": 0.6}},
				"free-write": map[string]any{"id": "free-write", "cost": map[string]any{"input": 0.15, "output": 0.6, "cache_write": 0}},
				"bad-cache":  map[string]any{"id": "bad-cache", "cost": map[string]any{"input": 1, "output": 2, "cache_read": -1}},
			},
		},
	}, []string{"gpt-mini", "free-write", "bad-cache"})
	if len(preview.Matches) != 2 {
		t.Fatalf("matches = %+v", preview.Matches)
	}
	byModel := map[string]PriceMatch{}
	for _, match := range preview.Matches {
		byModel[match.Model] = match
	}
	if byModel["gpt-mini"].CacheReadPricePer1M != 0 || byModel["gpt-mini"].CacheWritePricePer1M != nil {
		t.Fatalf("missing cache-write must stay unconfigured: %+v", byModel["gpt-mini"])
	}
	if byModel["free-write"].CacheWritePricePer1M == nil || *byModel["free-write"].CacheWritePricePer1M != 0 {
		t.Fatalf("explicit zero cache-write must be preserved: %+v", byModel["free-write"])
	}
	if len(preview.UnmatchedModels) != 1 || preview.UnmatchedModels[0] != "bad-cache" {
		t.Fatalf("negative cache should be unmatched: %+v", preview.UnmatchedModels)
	}
}

func previewFixture(t *testing.T, catalog map[string]any, models []string) Preview {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		raw := mustJSON(t, catalog)
		_, _ = w.Write(raw)
	}))
	t.Cleanup(server.Close)
	client := NewClientForTest(&http.Client{Timeout: 2 * time.Second}, server.URL)
	preview, err := client.Preview(context.Background(), models)
	if err != nil {
		t.Fatal(err)
	}
	return preview
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
