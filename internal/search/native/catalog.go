package native

import (
	"bytes"
	"encoding/json"
	"slices"
	"strings"

	"cpa-key-policy/internal/jsonscan"
)

// catalogEntry is the part of a Codex catalog model the decision reads.
type catalogEntry struct {
	Slug       string `json:"slug"`
	Lite       *bool  `json:"use_responses_lite"`
	Visibility string `json:"visibility"`
}

// candidate reports whether a catalog model searches through /alpha/search
// today and so could search natively: Codex sends Responses Lite models no
// hosted tool. Hidden models (codex-auto-review and the like) are left to
// Codex.
func (e catalogEntry) candidate() bool {
	return e.Slug != "" && e.Lite != nil && *e.Lite && !strings.EqualFold(e.Visibility, "hide")
}

// rewriteCatalog takes the body of a Codex model catalog (an object with a
// models array; OpenAI model lists have data instead) and returns its
// candidates and, when any of them is in native, the body with
// use_responses_lite turned off for those. Codex then sends the hosted
// web_search tool with the conversation and stops calling /alpha/search.
// Everything else in the body is kept. A body that is not a catalog returns
// ok false.
func rewriteCatalog(body []byte, native map[string]bool) (candidates []string, rewritten []byte, ok bool) {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 || trimmed[0] != '{' || !bytes.Contains(trimmed, []byte(`"models"`)) {
		return nil, nil, false
	}
	var top map[string]json.RawMessage
	if json.Unmarshal(trimmed, &top) != nil {
		return nil, nil, false
	}
	var models []json.RawMessage
	if raw, found := top["models"]; !found || json.Unmarshal(raw, &models) != nil {
		return nil, nil, false
	}
	changed := false
	for i, raw := range models {
		var entry catalogEntry
		if json.Unmarshal(raw, &entry) != nil || !entry.candidate() {
			continue
		}
		candidates = append(candidates, entry.Slug)
		if !native[entry.Slug] {
			continue
		}
		var fields map[string]json.RawMessage
		if json.Unmarshal(raw, &fields) != nil {
			continue
		}
		fields["use_responses_lite"] = json.RawMessage("false")
		if out, err := marshal(fields); err == nil {
			models[i], changed = out, true
		}
	}
	if !changed {
		return candidates, nil, true
	}
	encoded, err := marshal(models)
	if err != nil {
		return candidates, nil, true
	}
	top["models"] = encoded
	if rewritten, err = marshal(top); err != nil {
		return candidates, nil, true
	}
	return candidates, rewritten, true
}

// stripWebSearch takes out of a Responses request body the hosted web_search
// tools, a tool_choice that forces one, which would be refused without the
// tool, and the web_search_call items the input replays: CPA declares the
// tool again for a request that replays one, since the upstream refuses such
// a replay without it, as it drops them from a Responses Lite request. Only
// the top-level tools, tool_choice and input values are rebuilt; the rest of
// the body, and every input item kept, is copied as it is. ok is false when
// nothing was removed.
func stripWebSearch(body []byte) ([]byte, bool) {
	tools, choice, input := [2]int{-1, -1}, [2]int{-1, -1}, [2]int{-1, -1}
	err := jsonscan.Members(body, func(key []byte, start, end int) bool {
		switch string(key) {
		case "tools":
			tools = [2]int{start, end}
		case "tool_choice":
			choice = [2]int{start, end}
		case "input":
			input = [2]int{start, end}
		}
		return true
	})
	if err != nil {
		return nil, false
	}
	type replacement struct {
		start, end int
		value      []byte
	}
	var replacements []replacement
	if tools[0] >= 0 {
		var list []json.RawMessage
		if json.Unmarshal(body[tools[0]:tools[1]], &list) != nil {
			return nil, false
		}
		kept := list[:0:0]
		for _, raw := range list {
			if !isWebSearchTool(raw) {
				kept = append(kept, raw)
			}
		}
		if len(kept) < len(list) {
			encoded, err := marshal(kept)
			if err != nil {
				return nil, false
			}
			replacements = append(replacements, replacement{tools[0], tools[1], encoded})
		}
	}
	if choice[0] >= 0 && isWebSearchTool(body[choice[0]:choice[1]]) {
		replacements = append(replacements, replacement{choice[0], choice[1], []byte(`"auto"`)})
	}
	if input[0] >= 0 {
		if kept, ok := withoutSearchCalls(body[input[0]:input[1]]); ok {
			replacements = append(replacements, replacement{input[0], input[1], kept})
		}
	}
	if len(replacements) == 0 {
		return nil, false
	}
	slices.SortFunc(replacements, func(a, b replacement) int { return a.start - b.start })
	out := make([]byte, 0, len(body))
	at := 0
	for _, r := range replacements {
		out = append(append(out, body[at:r.start]...), r.value...)
		at = r.end
	}
	return append(out, body[at:]...), true
}

// withoutSearchCalls returns an input array without its web_search_call
// items, the others copied as they are; ok is false when it has none or
// cannot be read.
func withoutSearchCalls(input []byte) ([]byte, bool) {
	if !bytes.Contains(input, []byte(`"web_search_call"`)) {
		return nil, false
	}
	out := make([]byte, 0, len(input))
	out = append(out, '[')
	removed, readable := false, true
	err := jsonscan.Elements(input, func(start, end int) bool {
		item := input[start:end]
		search, ok := isSearchCall(item)
		if !ok {
			readable = false
			return false
		}
		if search {
			removed = true
			return true
		}
		if len(out) > 1 {
			out = append(out, ',')
		}
		out = append(out, item...)
		return true
	})
	if err != nil || !readable || !removed {
		return nil, false
	}
	return append(out, ']'), true
}

// isSearchCall reports an input item of type web_search_call; ok is false
// when the item is an object that cannot be read.
func isSearchCall(item []byte) (search, ok bool) {
	if len(item) == 0 || item[0] != '{' {
		return false, true
	}
	err := jsonscan.Members(item, func(key []byte, start, end int) bool {
		if string(key) != "type" {
			return true
		}
		search = string(item[start:end]) == `"web_search_call"`
		return false
	})
	return search, err == nil
}

// marshal encodes v without HTML escaping, so <, > and & stay literal as
// clients send them.
func marshal(v any) ([]byte, error) {
	var buf bytes.Buffer
	encoder := json.NewEncoder(&buf)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}

// isWebSearchTool reports a hosted web search tool or tool choice:
// web_search, web_search_preview and their dated variants.
func isWebSearchTool(raw json.RawMessage) bool {
	var tool struct {
		Type string `json:"type"`
	}
	return json.Unmarshal(raw, &tool) == nil && strings.HasPrefix(tool.Type, "web_search")
}
