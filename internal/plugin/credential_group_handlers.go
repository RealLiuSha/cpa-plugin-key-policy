package plugin

import (
	"net/http"
	"regexp"
	"strings"

	"cpa-key-policy/internal/policy"
)

type classifyRuleUpsertRequest struct {
	Name    string `json:"name"`
	Field   string `json:"field"`
	Pattern string `json:"pattern"`
	Group   string `json:"group"`
	Enabled bool   `json:"enabled"`
}

func (a *App) upsertClassifyRule(raw []byte) ManagementResponse {
	var req classifyRuleUpsertRequest
	if err := decodeStrictBody(raw, &req); err != nil {
		return jsonError(http.StatusBadRequest, "bad_request", err.Error())
	}
	rule := policy.ClassifyRule{Name: req.Name, Field: req.Field, Pattern: req.Pattern, Group: req.Group, Enabled: req.Enabled}
	if err := a.store.UpsertClassifyRule(rule); err != nil {
		return jsonError(http.StatusBadRequest, "validation_error", err.Error())
	}
	return jsonResponse(http.StatusOK, map[string]any{"rule": rule})
}

type classifyRuleDeleteRequest struct {
	Name string `json:"name"`
}

func (a *App) deleteClassifyRule(raw []byte) ManagementResponse {
	var req classifyRuleDeleteRequest
	if err := decodeStrictBody(raw, &req); err != nil {
		return jsonError(http.StatusBadRequest, "bad_request", err.Error())
	}
	if err := a.store.DeleteClassifyRule(req.Name); err != nil {
		return jsonError(http.StatusBadRequest, "delete_failed", err.Error())
	}
	return jsonResponse(http.StatusOK, map[string]any{"deleted": true})
}

type classifyReorderRequest struct {
	Names []string `json:"names"`
}

func (a *App) reorderClassifyRules(raw []byte) ManagementResponse {
	var req classifyReorderRequest
	if err := decodeStrictBody(raw, &req); err != nil {
		return jsonError(http.StatusBadRequest, "bad_request", err.Error())
	}
	if err := a.store.ReorderClassifyRules(req.Names); err != nil {
		return jsonError(http.StatusBadRequest, "reorder_failed", err.Error())
	}
	return jsonResponse(http.StatusOK, map[string]any{"reordered": true})
}

type classifyPreviewRequest struct {
	Descriptors []credentialDescriptor `json:"descriptors"`
	Rules       []policy.ClassifyRule  `json:"rules,omitempty"`
}

type credentialDescriptor struct {
	ID         string            `json:"id"`
	Provider   string            `json:"provider"`
	Attributes map[string]string `json:"attributes,omitempty"`
}

type classifyPreviewResponse struct {
	Groups      map[string][]string `json:"groups"`
	GroupCounts map[string]int      `json:"group_counts"`
}

func (a *App) classifyPreview(raw []byte) ManagementResponse {
	var req classifyPreviewRequest
	if err := decodeStrictBody(raw, &req); err != nil {
		return jsonError(http.StatusBadRequest, "bad_request", err.Error())
	}
	rules := req.Rules
	if len(rules) == 0 {
		rules = a.store.ClassifyRulesSnapshot()
	}
	type compiledRule struct {
		rule    policy.ClassifyRule
		pattern *regexp.Regexp
	}
	compiled := make([]compiledRule, 0, len(rules))
	for _, rule := range rules {
		if !rule.Enabled {
			continue
		}
		pattern, err := regexp.Compile(rule.Pattern)
		if err != nil {
			continue
		}
		compiled = append(compiled, compiledRule{rule: rule, pattern: pattern})
	}

	groups := make(map[string][]string)
	groupCounts := make(map[string]int)
	for _, descriptor := range req.Descriptors {
		matched := false
		for _, rule := range compiled {
			value := descriptorFieldValue(descriptor, rule.rule.Field)
			if value == "" || !rule.pattern.MatchString(value) {
				continue
			}
			group := strings.ToLower(rule.rule.Group)
			groups[group] = append(groups[group], descriptor.ID)
			groupCounts[group]++
			matched = true
		}
		if !matched {
			group := descriptorBuiltInGroup(descriptor)
			groups[group] = append(groups[group], descriptor.ID)
			groupCounts[group]++
		}
	}
	return jsonResponse(http.StatusOK, classifyPreviewResponse{Groups: groups, GroupCounts: groupCounts})
}

func descriptorFieldValue(descriptor credentialDescriptor, field string) string {
	field = strings.ToLower(strings.TrimSpace(field))
	switch field {
	case "filename", "id":
		return descriptor.ID
	case "provider":
		return descriptor.Provider
	default:
		return descriptor.Attributes[field]
	}
}

func descriptorBuiltInGroup(descriptor credentialDescriptor) string {
	plan := strings.ToLower(strings.TrimSpace(descriptor.Attributes["plan_type"]))
	tier := strings.ToLower(strings.TrimSpace(descriptor.Attributes["tier"]))
	if plan != "" {
		return plan
	}
	if tier != "" {
		return tier
	}
	return "supported"
}
