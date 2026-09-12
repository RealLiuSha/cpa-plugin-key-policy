package pricingmetadata

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"
	"unicode"
)

const (
	SourceName     = "Models.dev"
	SourceURL      = "https://models.dev/api.json"
	requestTimeout = 12 * time.Second
	maxCatalogSize = 64 << 20
)

type Client struct {
	httpClient *http.Client
	sourceURL  string
}

func NewClient() *Client {
	return &Client{
		httpClient: &http.Client{Timeout: requestTimeout},
		sourceURL:  SourceURL,
	}
}

func NewClientForTest(httpClient *http.Client, sourceURL string) *Client {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: requestTimeout}
	}
	if strings.TrimSpace(sourceURL) == "" {
		sourceURL = SourceURL
	}
	return &Client{httpClient: httpClient, sourceURL: sourceURL}
}

type PriceMatch struct {
	Model                string   `json:"model"`
	MatchedModel         string   `json:"matched_model"`
	MatchType            string   `json:"match_type"`
	Source               string   `json:"source"`
	SourceURL            string   `json:"source_url"`
	SourceProviderID     string   `json:"source_provider_id"`
	SourceProviderName   string   `json:"source_provider_name"`
	PromptPricePer1M     float64  `json:"prompt_price_per_1m"`
	CompletionPricePer1M float64  `json:"completion_price_per_1m"`
	CacheReadPricePer1M  float64  `json:"cache_read_price_per_1m"`
	CacheWritePricePer1M *float64 `json:"cache_write_price_per_1m,omitempty"`
}

type Preview struct {
	Source          string       `json:"source"`
	SourceURL       string       `json:"source_url"`
	MetadataModels  int          `json:"metadata_models"`
	Matches         []PriceMatch `json:"matches"`
	UnmatchedModels []string     `json:"unmatched_models"`
}

type modelsDevProvider struct {
	ID     string                    `json:"id"`
	Name   string                    `json:"name"`
	Models map[string]modelsDevModel `json:"models"`
}

type modelsDevModel struct {
	ID          string        `json:"id"`
	Name        string        `json:"name"`
	Family      string        `json:"family"`
	LastUpdated string        `json:"last_updated"`
	Status      string        `json:"status"`
	Cost        modelsDevCost `json:"cost"`
}

type modelsDevCost struct {
	Input      *float64 `json:"input"`
	Output     *float64 `json:"output"`
	CacheRead  *float64 `json:"cache_read"`
	CacheWrite *float64 `json:"cache_write"`
}

type catalogEntry struct {
	providerID   string
	providerName string
	model        modelsDevModel
}

type catalogIndex struct {
	exact      map[string][]catalogEntry
	normalized map[string][]catalogEntry
}

type matchCandidate struct {
	entry     catalogEntry
	matchType string
	score     int
}

func (c *Client) Preview(ctx context.Context, models []string) (Preview, error) {
	if c == nil {
		c = NewClient()
	}
	catalog, err := c.fetchCatalog(ctx)
	if err != nil {
		return Preview{}, err
	}
	return buildPreview(models, catalog, c.sourceURL), nil
}

func (c *Client) fetchCatalog(ctx context.Context) (map[string]modelsDevProvider, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, c.sourceURL, nil)
	if err != nil {
		return nil, fmt.Errorf("build pricing catalog request: %w", err)
	}
	request.Header.Set("Accept", "application/json")
	response, err := c.httpClient.Do(request)
	if err != nil {
		return nil, fmt.Errorf("fetch pricing catalog: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return nil, fmt.Errorf("fetch pricing catalog: unexpected status %d", response.StatusCode)
	}
	var catalog map[string]modelsDevProvider
	decoder := json.NewDecoder(io.LimitReader(response.Body, maxCatalogSize))
	if err := decoder.Decode(&catalog); err != nil {
		return nil, fmt.Errorf("decode pricing catalog: %w", err)
	}
	if catalog == nil {
		catalog = map[string]modelsDevProvider{}
	}
	return catalog, nil
}

func buildPreview(models []string, catalog map[string]modelsDevProvider, sourceURL string) Preview {
	entries := flattenCatalog(catalog)
	index := buildIndex(entries)
	matches := make([]PriceMatch, 0, len(models))
	unmatched := make([]string, 0)
	seen := make(map[string]struct{}, len(models))
	for _, rawModel := range models {
		model := strings.TrimSpace(rawModel)
		if model == "" {
			continue
		}
		if _, ok := seen[model]; ok {
			continue
		}
		seen[model] = struct{}{}
		match, ok := MatchModel(model, index)
		if !ok {
			unmatched = append(unmatched, model)
			continue
		}
		match.Source = SourceName
		match.SourceURL = sourceURL
		matches = append(matches, match)
	}
	sort.Slice(matches, func(left, right int) bool { return matches[left].Model < matches[right].Model })
	sort.Strings(unmatched)
	return Preview{
		Source:          SourceName,
		SourceURL:       sourceURL,
		MetadataModels:  len(entries),
		Matches:         matches,
		UnmatchedModels: unmatched,
	}
}

func flattenCatalog(catalog map[string]modelsDevProvider) []catalogEntry {
	providerIDs := make([]string, 0, len(catalog))
	for providerID := range catalog {
		providerIDs = append(providerIDs, providerID)
	}
	sort.Strings(providerIDs)
	entries := make([]catalogEntry, 0)
	for _, providerKey := range providerIDs {
		provider := catalog[providerKey]
		providerID := strings.TrimSpace(provider.ID)
		if providerID == "" {
			providerID = strings.TrimSpace(providerKey)
		}
		if providerID == "" {
			continue
		}
		providerName := strings.TrimSpace(provider.Name)
		if providerName == "" {
			providerName = providerID
		}
		modelIDs := make([]string, 0, len(provider.Models))
		for modelID := range provider.Models {
			modelIDs = append(modelIDs, modelID)
		}
		sort.Strings(modelIDs)
		for _, modelKey := range modelIDs {
			model := provider.Models[modelKey]
			if strings.TrimSpace(model.ID) == "" {
				model.ID = strings.TrimSpace(modelKey)
			}
			if strings.TrimSpace(model.ID) == "" && strings.TrimSpace(model.Name) == "" {
				continue
			}
			entries = append(entries, catalogEntry{providerID: providerID, providerName: providerName, model: model})
		}
	}
	return entries
}

func buildIndex(entries []catalogEntry) catalogIndex {
	index := catalogIndex{
		exact:      make(map[string][]catalogEntry, len(entries)*3),
		normalized: make(map[string][]catalogEntry, len(entries)*3),
	}
	for _, entry := range entries {
		model := entry.model
		registerIndexValue(index.exact, model.ID, entry)
		registerIndexValue(index.exact, model.Name, entry)
		registerIndexValue(index.exact, stripPrefix(model.ID), entry)
		registerIndexValue(index.exact, stripPrefix(model.Name), entry)
		registerIndexValue(index.normalized, normalizeKey(model.ID), entry)
		registerIndexValue(index.normalized, normalizeKey(model.Name), entry)
		registerIndexValue(index.normalized, normalizeKey(stripPrefix(model.ID)), entry)
		registerIndexValue(index.normalized, normalizeKey(stripPrefix(model.Name)), entry)
	}
	return index
}

func registerIndexValue(target map[string][]catalogEntry, value string, entry catalogEntry) {
	key := strings.ToLower(strings.TrimSpace(value))
	if key == "" {
		return
	}
	target[key] = append(target[key], entry)
}

func MatchModel(model string, index catalogIndex) (PriceMatch, bool) {
	candidates := matchCandidates(model, index)
	for _, candidate := range candidates {
		match, ok := priceFromCandidate(model, candidate)
		if ok {
			return match, true
		}
	}
	return PriceMatch{}, false
}

func matchCandidates(model string, index catalogIndex) []matchCandidate {
	model = strings.TrimSpace(model)
	if model == "" {
		return nil
	}
	var candidates []matchCandidate
	add := func(entries []catalogEntry, matchType string, score int) {
		for _, entry := range entries {
			candidates = append(candidates, matchCandidate{entry: entry, matchType: matchType, score: score})
		}
	}
	suffix := stripPrefix(model)
	if suffix != model {
		add(index.exact[strings.ToLower(suffix)], "index_suffix", 100)
		add(index.normalized[normalizeKey(suffix)], "index_normalized_suffix", 96)
		add(index.exact[strings.ToLower(model)], "index_exact", 92)
		add(index.normalized[normalizeKey(model)], "index_normalized", 90)
	} else {
		add(index.exact[strings.ToLower(model)], "index_exact", 100)
		add(index.normalized[normalizeKey(model)], "index_normalized", 92)
	}
	return sortedUniqueCandidates(model, candidates)
}

func sortedUniqueCandidates(model string, candidates []matchCandidate) []matchCandidate {
	bestByKey := make(map[string]matchCandidate, len(candidates))
	for _, candidate := range candidates {
		key := strings.TrimSpace(candidate.entry.providerID) + "\x00" + strings.TrimSpace(candidate.entry.model.ID)
		if key == "\x00" {
			continue
		}
		if existing, ok := bestByKey[key]; !ok || candidateLess(model, candidate, existing) {
			bestByKey[key] = candidate
		}
	}
	unique := make([]matchCandidate, 0, len(bestByKey))
	for _, candidate := range bestByKey {
		unique = append(unique, candidate)
	}
	sort.Slice(unique, func(left, right int) bool {
		return candidateLess(model, unique[left], unique[right])
	})
	return unique
}

func candidateLess(model string, left, right matchCandidate) bool {
	if left.score != right.score {
		return left.score > right.score
	}
	leftPlanZero := isPlanZero(left)
	rightPlanZero := isPlanZero(right)
	if leftPlanZero != rightPlanZero {
		return !leftPlanZero
	}
	leftRank := providerRankForModel(model, left.entry.providerID)
	rightRank := providerRankForModel(model, right.entry.providerID)
	if leftRank != rightRank {
		return leftRank < rightRank
	}
	leftDeprecated := isDeprecated(left.entry.model)
	rightDeprecated := isDeprecated(right.entry.model)
	if leftDeprecated != rightDeprecated {
		return !leftDeprecated
	}
	if left.entry.model.LastUpdated != right.entry.model.LastUpdated {
		return left.entry.model.LastUpdated > right.entry.model.LastUpdated
	}
	if left.entry.providerID != right.entry.providerID {
		return left.entry.providerID < right.entry.providerID
	}
	return left.entry.model.ID < right.entry.model.ID
}

func isDeprecated(model modelsDevModel) bool {
	return strings.EqualFold(strings.TrimSpace(model.Status), "deprecated")
}

func isPlanZero(candidate matchCandidate) bool {
	if !isPlanProvider(candidate.entry.providerID) {
		return false
	}
	input := candidate.entry.model.Cost.Input
	output := candidate.entry.model.Cost.Output
	return input != nil && output != nil && *input == 0 && *output == 0
}

func isPlanProvider(providerID string) bool {
	provider := strings.ToLower(strings.TrimSpace(providerID))
	return strings.Contains(provider, "coding-plan") || strings.Contains(provider, "token-plan")
}

func providerRankForModel(model, providerID string) int {
	family := modelFamily(model)
	provider := strings.ToLower(strings.TrimSpace(providerID))
	if family != "" {
		for index, official := range officialProvidersByFamily(family) {
			if provider == official {
				return index
			}
		}
	}
	return 100 + providerRank(provider)
}

func modelFamily(model string) string {
	normalized := normalizeKey(stripPrefix(model))
	switch {
	case strings.HasPrefix(normalized, "gpt") || strings.HasPrefix(normalized, "chatgpt") || strings.HasPrefix(normalized, "o1") || strings.HasPrefix(normalized, "o3") || strings.HasPrefix(normalized, "o4"):
		return "openai"
	case strings.HasPrefix(normalized, "claude"):
		return "anthropic"
	case strings.HasPrefix(normalized, "deepseek"):
		return "deepseek"
	case strings.HasPrefix(normalized, "glm"):
		return "glm"
	case strings.HasPrefix(normalized, "qwen"):
		return "qwen"
	case strings.HasPrefix(normalized, "gemini"):
		return "google"
	case strings.HasPrefix(normalized, "grok"):
		return "xai"
	case strings.HasPrefix(normalized, "minimax"):
		return "minimax"
	case strings.HasPrefix(normalized, "moonshot") || strings.HasPrefix(normalized, "kimi"):
		return "moonshot"
	case strings.HasPrefix(normalized, "doubao"):
		return "doubao"
	case strings.HasPrefix(normalized, "mistral"):
		return "mistral"
	case strings.HasPrefix(normalized, "llama"):
		return "llama"
	case strings.HasPrefix(normalized, "xiaomi"):
		return "xiaomi"
	default:
		return ""
	}
}

func officialProvidersByFamily(family string) []string {
	switch family {
	case "openai":
		return []string{"openai", "azure", "azure-cognitive-services"}
	case "anthropic":
		return []string{"anthropic", "google-vertex-anthropic"}
	case "deepseek":
		return []string{"deepseek", "siliconflow-cn", "siliconflow"}
	case "glm":
		return []string{"zai", "zhipuai", "zai-coding-plan", "zhipuai-coding-plan"}
	case "qwen":
		return []string{"alibaba-cn", "alibaba", "aliyun-bailian"}
	case "google":
		return []string{"google", "google-vertex"}
	case "xai":
		return []string{"xai"}
	case "minimax":
		return []string{"minimax-cn", "minimax", "minimax-cn-coding-plan", "minimax-coding-plan"}
	case "moonshot":
		return []string{"moonshotai-cn", "moonshotai", "kimi-for-coding"}
	case "doubao":
		return []string{"doubao"}
	case "mistral":
		return []string{"mistral"}
	case "llama":
		return []string{"llama"}
	case "xiaomi":
		return []string{"xiaomi-token-plan-cn", "xiaomi", "xiaomi-token-plan-sgp", "xiaomi-token-plan-ams"}
	default:
		return nil
	}
}

func providerRank(providerID string) int {
	switch strings.ToLower(strings.TrimSpace(providerID)) {
	case "openai":
		return 0
	case "anthropic":
		return 1
	case "deepseek":
		return 2
	case "google":
		return 3
	case "alibaba-cn", "alibaba":
		return 4
	case "zai", "zhipuai":
		return 5
	case "xai":
		return 6
	case "minimax-cn", "minimax", "minimax-cn-coding-plan", "minimax-coding-plan":
		return 7
	case "xiaomi-token-plan-cn", "xiaomi-token-plan-sgp", "xiaomi-token-plan-ams", "xiaomi":
		return 8
	case "302ai":
		return 9
	case "openrouter":
		return 10
	case "vercel":
		return 11
	default:
		return 100
	}
}

func stripPrefix(model string) string {
	trimmed := strings.TrimSpace(model)
	if trimmed == "" {
		return ""
	}
	index := strings.LastIndexAny(trimmed, "/:")
	if index < 0 || index == len(trimmed)-1 {
		return trimmed
	}
	return strings.TrimSpace(trimmed[index+1:])
}

func normalizeKey(value string) string {
	var builder strings.Builder
	builder.Grow(len(value))
	for _, r := range value {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			builder.WriteRune(unicode.ToLower(r))
		}
	}
	return builder.String()
}

func priceFromCandidate(model string, candidate matchCandidate) (PriceMatch, bool) {
	metadata := candidate.entry.model
	if metadata.Cost.Input == nil || metadata.Cost.Output == nil {
		return PriceMatch{}, false
	}
	input := *metadata.Cost.Input
	output := *metadata.Cost.Output
	if input < 0 || output < 0 {
		return PriceMatch{}, false
	}
	cacheRead := 0.0
	if metadata.Cost.CacheRead != nil {
		cacheRead = *metadata.Cost.CacheRead
	}
	cacheWrite := metadata.Cost.CacheWrite
	if cacheRead < 0 || cacheWrite != nil && *cacheWrite < 0 {
		return PriceMatch{}, false
	}
	matchedModel := strings.TrimSpace(metadata.ID)
	if matchedModel == "" {
		matchedModel = strings.TrimSpace(metadata.Name)
	}
	providerName := strings.TrimSpace(candidate.entry.providerName)
	if providerName == "" {
		providerName = strings.TrimSpace(candidate.entry.providerID)
	}
	return PriceMatch{
		Model:                model,
		MatchedModel:         matchedModel,
		MatchType:            candidate.matchType,
		SourceProviderID:     strings.TrimSpace(candidate.entry.providerID),
		SourceProviderName:   providerName,
		PromptPricePer1M:     input,
		CompletionPricePer1M: output,
		CacheReadPricePer1M:  cacheRead,
		CacheWritePricePer1M: clonePrice(cacheWrite),
	}, true
}

func clonePrice(price *float64) *float64 {
	if price == nil {
		return nil
	}
	cloned := *price
	return &cloned
}
