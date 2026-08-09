package policy

import (
	"regexp"
	"strings"
)

// ClassifyGroupPrefix is applied to user-defined classify rule group names when
// they are written into model targets and catalog entries, so
// they never collide with built-in plan_type/tier values like "free" or "team".
//
// Rule storage keeps the bare name (e.g. "vip"); runtime matching and the
// picker catalog always use the prefixed form ("classify:vip").
const ClassifyGroupPrefix = "classify:"

// FormatClassifyGroup returns the storage/runtime group string for a custom
// classify rule group name. Empty input yields empty output. Already-prefixed
// names are returned lowercased as-is (idempotent).
func FormatClassifyGroup(name string) string {
	name = strings.ToLower(strings.TrimSpace(name))
	if name == "" {
		return ""
	}
	if strings.HasPrefix(name, ClassifyGroupPrefix) {
		return name
	}
	return ClassifyGroupPrefix + name
}

// StripClassifyGroupPrefix returns the bare group name for display. Non-prefixed
// values are returned unchanged (built-in tiers).
func StripClassifyGroupPrefix(group string) string {
	group = strings.TrimSpace(group)
	if strings.HasPrefix(strings.ToLower(group), ClassifyGroupPrefix) {
		return group[len(ClassifyGroupPrefix):]
	}
	return group
}

// IsClassifyGroup reports whether group carries the custom-classify prefix.
func IsClassifyGroup(group string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(group)), ClassifyGroupPrefix)
}

// BuiltinTierProviders are auth-file providers that participate in built-in
// plan_type/tier splitting when no custom classify rule matches. Other
// auth-file providers stay flat (no group) unless a custom rule hits them.
var BuiltinTierProviders = map[string]bool{
	"codex":       true,
	"antigravity": true,
}

// CatalogCredential is one auth-file (or equivalent) descriptor plus the models
// that file can serve. Supplied by the management UI when building the picker
// catalog — the plugin has no direct host access to auth-files.
type CatalogCredential struct {
	ID         string            `json:"id"`
	Provider   string            `json:"provider"`
	Attributes map[string]string `json:"attributes,omitempty"`
	Models     []string          `json:"models,omitempty"`
}

// CatalogEntry is one picker row bucket: provider + optional group + models.
type CatalogEntry struct {
	Provider string   `json:"provider"`
	Group    string   `json:"group,omitempty"`
	Models   []string `json:"models"`
}

// GroupsForCredential returns every group a credential belongs to, using the
// same semantics as the runtime scheduler:
//  1. All matching enabled classify rules → prefixed groups (multi-group).
//  2. If none match → built-in plan_type/tier (or "supported" for tiered
//     providers with no claim). Non-tiered providers with no custom match get
//     a single empty group (flat picker row).
//
// rules should already be normalized; disabled rules and rules whose pattern
// does not compile are skipped (Compile is best-effort via Compiled()).
func GroupsForCredential(provider string, attrs map[string]string, id string, rules []ClassifyRule) []string {
	provider = strings.ToLower(strings.TrimSpace(provider))
	var custom []string
	seen := map[string]bool{}
	for i := range rules {
		rule := &rules[i]
		if !rule.Enabled {
			continue
		}
		re := rule.Compiled()
		if re == nil {
			// Best-effort compile for rules that weren't Normalize()'d yet.
			compiled, err := regexp.Compile(rule.Pattern)
			if err != nil {
				continue
			}
			re = compiled
		}
		val := credentialFieldValue(id, provider, attrs, rule.Field)
		if val == "" || !re.MatchString(val) {
			continue
		}
		g := FormatClassifyGroup(rule.Group)
		if g == "" || seen[g] {
			continue
		}
		seen[g] = true
		custom = append(custom, g)
	}
	if len(custom) > 0 {
		return custom
	}
	return []string{builtinGroupFor(provider, attrs)}
}

func credentialFieldValue(id, provider string, attrs map[string]string, field string) string {
	field = strings.ToLower(strings.TrimSpace(field))
	switch field {
	case "filename", "id":
		return id
	case "provider":
		return provider
	default:
		if attrs != nil {
			return attrs[field]
		}
	}
	return ""
}

// builtinGroupFor mirrors plugin builtInGroup, with the addition that non-tiered
// providers without a plan/tier claim return "" (flat) rather than "supported".
func builtinGroupFor(provider string, attrs map[string]string) string {
	if attrs != nil {
		plan := strings.ToLower(strings.TrimSpace(attrs["plan_type"]))
		if plan != "" {
			return plan
		}
		tier := strings.ToLower(strings.TrimSpace(attrs["tier"]))
		if tier != "" {
			return tier
		}
	}
	if BuiltinTierProviders[provider] {
		return "supported"
	}
	return ""
}

// BuildCatalogEntries aggregates credentials into de-duplicated
// (provider, group) → models entries for the model picker.
func BuildCatalogEntries(creds []CatalogCredential, rules []ClassifyRule) []CatalogEntry {
	// key: provider\0group → ordered unique models
	type bucket struct {
		provider string
		group    string
		models   []string
		seen     map[string]bool
	}
	order := make([]string, 0)
	buckets := map[string]*bucket{}

	for _, c := range creds {
		provider := strings.ToLower(strings.TrimSpace(c.Provider))
		if provider == "" {
			continue
		}
		id := strings.TrimSpace(c.ID)
		groups := GroupsForCredential(provider, c.Attributes, id, rules)
		models := normalizeModelList(c.Models)
		if len(models) == 0 {
			continue
		}
		for _, g := range groups {
			key := provider + "\x00" + g
			b := buckets[key]
			if b == nil {
				b = &bucket{provider: provider, group: g, seen: map[string]bool{}}
				buckets[key] = b
				order = append(order, key)
			}
			for _, m := range models {
				lm := strings.ToLower(m)
				if b.seen[lm] {
					continue
				}
				b.seen[lm] = true
				b.models = append(b.models, m)
			}
		}
	}

	out := make([]CatalogEntry, 0, len(order))
	for _, key := range order {
		b := buckets[key]
		if len(b.models) == 0 {
			continue
		}
		e := CatalogEntry{Provider: b.provider, Models: b.models}
		if b.group != "" {
			e.Group = b.group
		}
		out = append(out, e)
	}
	return out
}

func normalizeModelList(models []string) []string {
	if len(models) == 0 {
		return nil
	}
	out := make([]string, 0, len(models))
	for _, m := range models {
		m = strings.TrimSpace(m)
		if m != "" {
			out = append(out, m)
		}
	}
	return out
}
