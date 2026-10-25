package channel

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"os"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"
)

// metaDefaultBaseURL is where CPA sends a meta-api-key entry without a
// base-url; the entry's auth_index is derived with it.
const metaDefaultBaseURL = "https://api.meta.ai/v1"

// cpaDocument holds the credential lists of CPA's config.yaml in both
// layouts, the global proxy-url (requests.proxy-url in v8, top-level before)
// and force-model-prefix. Each list is a pointer so that a present but empty
// list differs from an absent one.
type cpaDocument struct {
	ProxyURL         string `yaml:"proxy-url"`
	ForceModelPrefix bool   `yaml:"force-model-prefix"`
	Requests         struct {
		ProxyURL *string `yaml:"proxy-url"`
	} `yaml:"requests"`
	Grouped      cpaGroupedKeys `yaml:"api-keys"`
	Gemini       *[]cpaEntry    `yaml:"gemini-api-key"`
	Interactions *[]cpaEntry    `yaml:"interactions-api-key"`
	Codex        *[]cpaEntry    `yaml:"codex-api-key"`
	Claude       *[]cpaEntry    `yaml:"claude-api-key"`
	XAI          *[]cpaEntry    `yaml:"xai-api-key"`
	Meta         *[]cpaEntry    `yaml:"meta-api-key"`
	Compat       *[]cpaEntry    `yaml:"openai-compatibility"`
}

// cpaGroupedKeys is the v8 api-keys mapping of families to groups. In the
// older layout api-keys is the list of client keys, which decodes to
// nothing here. vertex is absent: CPA derives its indexes from internal
// credential IDs, not from base URL and key.
type cpaGroupedKeys struct {
	Gemini       *[]cpaEntry `yaml:"gemini"`
	Interactions *[]cpaEntry `yaml:"interactions"`
	Codex        *[]cpaEntry `yaml:"codex"`
	Claude       *[]cpaEntry `yaml:"claude"`
	XAI          *[]cpaEntry `yaml:"xai"`
	Meta         *[]cpaEntry `yaml:"meta"`
	Compat       *[]cpaEntry `yaml:"openai-compatibility"`
}

func (g *cpaGroupedKeys) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind != yaml.MappingNode {
		return nil
	}
	type plain cpaGroupedKeys
	return node.Decode((*plain)(g))
}

// cpaEntry decodes the fields that matter in every credential layout: an
// older entry (api-key, base-url, proxy-url, prefix, headers and
// excluded-models), a v8 group (name, base-url, keys and the proxy-url,
// prefix, headers and excluded-models its keys share) and an
// openai-compatibility provider (name, disabled, plus api-key-entries in the
// older layout or keys in v8).
type cpaEntry struct {
	Name           string            `yaml:"name"`
	APIKey         string            `yaml:"api-key"`
	BaseURL        string            `yaml:"base-url"`
	ProxyURL       string            `yaml:"proxy-url"`
	Prefix         string            `yaml:"prefix"`
	Headers        map[string]string `yaml:"headers"`
	ExcludedModels []string          `yaml:"excluded-models"`
	Models         []cpaModel        `yaml:"models"`
	Disabled       bool              `yaml:"disabled"`
	Keys           []cpaKey          `yaml:"keys"`
	APIKeyEntries  []cpaKey          `yaml:"api-key-entries"`
}

// cpaModel is one models entry: the upstream name and the name clients use
// for it, which CPA's config keeps under the host's own field name.
type cpaModel struct {
	Name       string `yaml:"name"`
	ClientName string `yaml:"alias"`
}

// cpaKey is one key of a v8 group or an openai-compatibility provider. In a
// v8 group a field the key sets, other than null, replaces the group's.
type cpaKey struct {
	APIKey         string            `yaml:"api-key"`
	ProxyURL       *string           `yaml:"proxy-url"`
	Prefix         *string           `yaml:"prefix"`
	Headers        map[string]string `yaml:"headers"`
	ExcludedModels *[]string         `yaml:"excluded-models"`
	Models         *[]cpaModel       `yaml:"models"`
}

// configKey is one credential CPA builds from an entry, with the proxy-url,
// prefix, headers, excluded-models and models in effect for it.
type configKey struct {
	apiKey, proxyURL, prefix string
	headers                  map[string]string
	excludedModels           []string
	models                   []cpaModel
}

// configKeyDisabled is the excluded-models entry with which CPA's panel
// disables a config API key: CPA keeps the credential but registers no
// model for it, so nothing is routed to it, not even a request pinned to
// its ID.
const configKeyDisabled = "*"

// ReadCPAConfig parses CPA's config.yaml (the v8 layout api-keys.<kind>[] and
// the older top-level <kind>-api-key and openai-compatibility lists) and
// returns the credentials keyed by auth_index; of several with one
// auth_index the first is kept.
//
// It mirrors how CPA builds credentials: a family present under the v8
// api-keys mapping replaces the older list of the same family; a v8 group's
// base-url, proxy-url, prefix and headers apply to each of its keys; entries
// without a key are skipped, as are codex and xai entries without a
// base-url, meta keys starting with "dca:" and disabled or base-url-less
// openai-compatibility providers; a meta entry without a base-url uses
// https://api.meta.ai/v1. A key without its own proxy-url gets the global
// one; as in CPA, a requests.proxy-url replaces the older top-level key. A
// key outside openai-compatibility whose excluded-models holds "*" is kept
// and marked Disabled.
// Values of an unexpected type are skipped. No error returned here quotes a
// key.
func ReadCPAConfig(path string) (map[string]Credential, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read CPA config: %w", err)
	}
	var doc cpaDocument
	if err := yaml.Unmarshal(raw, &doc); err != nil && !isTypeError(err) {
		return nil, fmt.Errorf("parse CPA config %s: %w", path, err)
	}
	globalProxy := strings.TrimSpace(doc.ProxyURL)
	if doc.Requests.ProxyURL != nil {
		globalProxy = strings.TrimSpace(*doc.Requests.ProxyURL)
	}
	grouped := &doc.Grouped
	families := []struct {
		// kind names the family under api-keys; prefix is its older
		// top-level key and its auth_index seed prefix.
		kind, prefix    string
		grouped, listed *[]cpaEntry
	}{
		{"gemini", "gemini-api-key", grouped.Gemini, doc.Gemini},
		{"interactions", "interactions-api-key", grouped.Interactions, doc.Interactions},
		{"codex", "codex-api-key", grouped.Codex, doc.Codex},
		{"claude", "claude-api-key", grouped.Claude, doc.Claude},
		{"xai", "xai-api-key", grouped.XAI, doc.XAI},
		{"meta", "meta-api-key", grouped.Meta, doc.Meta},
		{"openai-compatibility", "openai-compatibility", grouped.Compat, doc.Compat},
	}
	credentials := make(map[string]Credential)
	for _, family := range families {
		entries, v8 := family.listed, false
		if family.grouped != nil {
			entries, v8 = family.grouped, true
		}
		if entries == nil {
			continue
		}
		for _, entry := range *entries {
			for _, key := range familyKeys(family.kind, v8, entry) {
				addCredential(credentials, family.kind, family.prefix, entry, key, globalProxy, doc.ForceModelPrefix)
			}
		}
	}
	return credentials, nil
}

// familyKeys lists the credentials an entry contributes under the entry's
// base-url.
func familyKeys(kind string, v8 bool, entry cpaEntry) []configKey {
	if kind == "openai-compatibility" {
		if entry.Disabled || strings.TrimSpace(entry.BaseURL) == "" {
			return nil
		}
		keys := entry.APIKeyEntries
		if v8 {
			keys = entry.Keys
		}
		// Only a key entry's own proxy-url is part of its ID.
		out := make([]configKey, 0, len(keys))
		for _, key := range keys {
			c := configKey{apiKey: key.APIKey}
			if key.ProxyURL != nil {
				c.proxyURL = *key.ProxyURL
			}
			out = append(out, c)
		}
		return out
	}
	shared := configKey{apiKey: entry.APIKey, proxyURL: entry.ProxyURL, prefix: entry.Prefix, headers: entry.Headers, excludedModels: entry.ExcludedModels, models: entry.Models}
	if !v8 {
		return []configKey{shared}
	}
	out := make([]configKey, 0, len(entry.Keys))
	for _, key := range entry.Keys {
		c := shared
		c.apiKey = key.APIKey
		if key.ProxyURL != nil {
			c.proxyURL = *key.ProxyURL
		}
		if key.Prefix != nil {
			c.prefix = *key.Prefix
		}
		if key.Headers != nil {
			c.headers = key.Headers
		}
		if key.ExcludedModels != nil {
			c.excludedModels = *key.ExcludedModels
		}
		if key.Models != nil {
			c.models = *key.Models
		}
		out = append(out, c)
	}
	return out
}

func addCredential(credentials map[string]Credential, kind, prefix string, entry cpaEntry, c configKey, globalProxy string, forceModelPrefix bool) {
	key, base := strings.TrimSpace(c.apiKey), strings.TrimSpace(entry.BaseURL)
	switch kind {
	case "codex", "xai":
		if base == "" {
			return
		}
	case "meta":
		if strings.HasPrefix(key, "dca:") {
			return
		}
		if base == "" {
			base = metaDefaultBaseURL
		}
	}
	if key == "" {
		return
	}
	index := authIndexFor(prefix, base, key)
	if _, exists := credentials[index]; exists {
		return
	}
	credential := Credential{AuthIndex: index, Kind: kind, Group: strings.TrimSpace(entry.Name), BaseURL: base, APIKey: key, ProxyURL: strings.TrimSpace(c.proxyURL)}
	if credential.ProxyURL == "" {
		credential.ProxyURL = globalProxy
	}
	if kind == "openai-compatibility" {
		name := strings.ToLower(credential.Group)
		if name == "" {
			name = "openai-compatibility"
		}
		credential.Provider = compatProviderKey(name)
		credential.ID = authIDFor("openai-compatibility:"+name, key, base, c.proxyURL)
	} else {
		credential.Provider = kind
		if kind == "interactions" {
			credential.Provider = "gemini-interactions"
		}
		credential.ID = authIDFor(credential.Provider+":apikey", key, base, c.proxyURL, modelPrefix(c.prefix), sortedHeaders(c.headers))
		credential.Disabled = slices.ContainsFunc(c.excludedModels, func(model string) bool {
			return strings.TrimSpace(model) == configKeyDisabled
		})
		credential.Models = servedModels(c.models, c.excludedModels, modelPrefix(c.prefix), forceModelPrefix)
	}
	credentials[index] = credential
}

// servedModels lists the names clients ask a key for: each entry's client
// name, else its name, less those an excluded-models pattern matches. A key
// with a prefix also serves prefix/name, and under force-model-prefix only
// that, unless the prefix is the name itself. nil means the key lists no
// models.
func servedModels(models []cpaModel, excluded []string, prefix string, forceModelPrefix bool) []string {
	if len(models) == 0 {
		return nil
	}
	out := []string{}
	add := func(name string) {
		if !slices.Contains(out, name) {
			out = append(out, name)
		}
	}
	for _, m := range models {
		name := strings.TrimSpace(m.ClientName)
		if name == "" {
			name = strings.TrimSpace(m.Name)
		}
		if name == "" || slices.ContainsFunc(excluded, func(pattern string) bool { return modelExcluded(pattern, name) }) {
			continue
		}
		if prefix == "" || !forceModelPrefix || prefix == name {
			add(name)
		}
		if prefix != "" {
			add(prefix + "/" + name)
		}
	}
	return out
}

// modelExcluded matches an excluded-models pattern as CPA does: without case,
// "*" standing for any run of characters (slashes included) and every other
// character for itself.
func modelExcluded(pattern, model string) bool {
	pattern = strings.ToLower(strings.TrimSpace(pattern))
	if pattern == "" {
		return false
	}
	value := strings.ToLower(strings.TrimSpace(model))
	parts := strings.Split(pattern, "*")
	if len(parts) == 1 {
		return pattern == value
	}
	head, tail := parts[0], parts[len(parts)-1]
	if !strings.HasPrefix(value, head) {
		return false
	}
	value = value[len(head):]
	if !strings.HasSuffix(value, tail) {
		return false
	}
	value = value[:len(value)-len(tail)]
	for _, segment := range parts[1 : len(parts)-1] {
		i := strings.Index(value, segment)
		if i < 0 {
			return false
		}
		value = value[i+len(segment):]
	}
	return true
}

// authIDFor is the ID CPA's config synthesizer gives a credential: kind, ":"
// and the first 12 hex characters of sha256 over kind and each trimmed part
// behind a NUL byte. CPA appends "-1", "-2"… to an ID it has given already,
// but such a repeat has the auth_index of an earlier entry, of which the
// first is the one kept.
func authIDFor(kind string, parts ...string) string {
	hash := sha256.New()
	hash.Write([]byte(kind))
	for _, part := range parts {
		hash.Write([]byte{0})
		hash.Write([]byte(strings.TrimSpace(part)))
	}
	return kind + ":" + hex.EncodeToString(hash.Sum(nil))[:12]
}

// modelPrefix normalizes a prefix as CPA does before building the ID: outer
// slashes go, and one with a slash left inside counts as none.
func modelPrefix(prefix string) string {
	prefix = strings.Trim(strings.TrimSpace(prefix), "/")
	if strings.Contains(prefix, "/") {
		return ""
	}
	return prefix
}

// sortedHeaders is the headers part of an ID: the pairs with a non-empty
// trimmed name and value, sorted by name, each as name NUL value NUL.
func sortedHeaders(headers map[string]string) string {
	clean := make(map[string]string, len(headers))
	for name, value := range headers {
		if name, value = strings.TrimSpace(name), strings.TrimSpace(value); name != "" && value != "" {
			clean[name] = value
		}
	}
	names := slices.Sorted(maps.Keys(clean))
	var b strings.Builder
	for _, name := range names {
		b.WriteString(name)
		b.WriteByte(0)
		b.WriteString(clean[name])
		b.WriteByte(0)
	}
	return b.String()
}

// compatProviderKey is the provider CPA files an openai-compatibility
// provider under, from its lowercased name.
func compatProviderKey(name string) string {
	if name == "openai-compatibility" || strings.HasPrefix(name, "openai-compatible-") {
		return name
	}
	return "openai-compatible-" + name
}

// authIndexFor is CPA's auth_index for a config credential: the first 8
// bytes of sha256 over "<prefix>:" + base_url + "+" + api_key, in hex.
func authIndexFor(prefix, baseURL, apiKey string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(prefix + ":" + baseURL + "+" + apiKey)))
	return hex.EncodeToString(sum[:8])
}

// isTypeError reports a value-type mismatch. Decoding then continues with
// the other values; the error text quotes the start of the offending value,
// which may be a key, so it is never passed on.
func isTypeError(err error) bool {
	var typeErr *yaml.TypeError
	return errors.As(err, &typeErr)
}
