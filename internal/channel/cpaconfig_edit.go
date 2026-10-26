package channel

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"reflect"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"
)

// ModelEdit takes one model off a config API key in CPA's config.yaml
// (Exclude) or gives it back, by adding an excluded-models entry naming the
// model to the key or removing that entry from it.
type ModelEdit struct {
	AuthIndex string
	Model     string
	Exclude   bool
}

// EditOutcome says what became of one ModelEdit.
type EditOutcome string

const (
	// EditApplied: the entry was added or removed.
	EditApplied EditOutcome = "applied"
	// EditUnchanged: the key excluded the model already (by an entry of its
	// own or of its group), or holds no entry of its own to remove.
	EditUnchanged EditOutcome = "unchanged"
	// EditDisabled: the key excludes every model ("*"), which is how CPA's
	// panel disables it; it is left as the operator set it.
	EditDisabled EditOutcome = "disabled"
	// EditInherited: the key has no excluded-models of its own and takes its
	// v8 group's, which a list of its own would replace; it is left as it
	// is, since such a list would stop following the group's.
	EditInherited EditOutcome = "inherited"
	// EditNoKey: no config API key outside openai-compatibility has the
	// auth_index.
	EditNoKey EditOutcome = "no_key"
)

var (
	// ErrConfigChanged reports that config.yaml changed between reading and
	// writing it, as when CPA's panel saved meanwhile; nothing was written.
	ErrConfigChanged = errors.New("CPA config.yaml changed while it was being edited; nothing was written")
	// ErrConfigLayout reports a config.yaml that is not laid out the way CPA
	// writes it, which an edit would reformat as a whole.
	ErrConfigLayout = errors.New("CPA config.yaml is not laid out the way CPA writes it, so it is not edited; saving it once from CPA's panel lays it out")
)

// editableFamilies are the credential families whose keys carry
// excluded-models: every family ReadCPAConfig reads but openai-compatibility.
var editableFamilies = []struct{ kind, prefix string }{
	{"gemini", "gemini-api-key"},
	{"interactions", "interactions-api-key"},
	{"codex", "codex-api-key"},
	{"claude", "claude-api-key"},
	{"xai", "xai-api-key"},
	{"meta", "meta-api-key"},
}

// keyNode is one config API key in the parsed document: the mapping holding
// its fields, the excluded-models of its v8 group (nil in the older layout or
// when the group lists none) and its path in the decoded document.
type keyNode struct {
	fields    *yaml.Node
	inherited *yaml.Node
	path      []any
}

// EditExcludedModels applies edits to the excluded-models of config API keys
// in CPA's config.yaml and returns what became of each, in order. CPA
// reloads the file when it changes, which rebuilds every credential.
//
// The file is rewritten only when an edit applies, and only if it is laid out
// the way CPA writes it (ErrConfigLayout otherwise), so nothing else in it
// changes; that is checked on the decoded documents before writing. Only a
// key's own excluded-models is edited: a key that takes its v8 group's is left
// alone (EditInherited), and a list the edits leave empty is removed, so
// giving back every model restores the file byte for byte. It is written in
// place, keeping the inode: CPA's container mounts the single file. It is read
// again just before writing, and a change meanwhile returns ErrConfigChanged
// without writing.
func EditExcludedModels(path string, edits []ModelEdit) ([]EditOutcome, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read CPA config: %w", err)
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("parse CPA config %s: %w", path, err)
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) == 0 || doc.Content[0].Kind != yaml.MappingNode {
		return nil, fmt.Errorf("CPA config %s holds no mapping", path)
	}
	layout, ok := detectLayout(&doc, raw)
	if !ok {
		return nil, ErrConfigLayout
	}
	keys := configKeyNodes(doc.Content[0])
	outcomes := make([]EditOutcome, len(edits))
	var edited [][]any
	for i, edit := range edits {
		key, found := keys[strings.ToLower(strings.TrimSpace(edit.AuthIndex))]
		if !found {
			outcomes[i] = EditNoKey
			continue
		}
		if outcomes[i] = key.edit(strings.TrimSpace(edit.Model), edit.Exclude); outcomes[i] == EditApplied {
			edited = append(edited, key.path)
		}
	}
	if len(edited) == 0 {
		return outcomes, nil
	}
	out, err := layout.encode(&doc)
	if err != nil {
		return nil, fmt.Errorf("encode CPA config: %w", err)
	}
	if err := onlyExclusionsDiffer(raw, out, edited); err != nil {
		return nil, err
	}
	current, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read CPA config: %w", err)
	}
	if !bytes.Equal(current, raw) {
		return nil, ErrConfigChanged
	}
	if err := os.WriteFile(path, out, 0o600); err != nil {
		return nil, fmt.Errorf("write CPA config: %w", err)
	}
	return outcomes, nil
}

// configLayout is how a config.yaml is encoded: CPA writes it through
// yaml.v3 and then moves every comment to the start of its line.
type configLayout struct {
	indent        int
	flushComments bool
}

func (l configLayout) encode(doc *yaml.Node) ([]byte, error) {
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(l.indent)
	if err := enc.Encode(doc); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	if !l.flushComments {
		return buf.Bytes(), nil
	}
	lines := bytes.Split(buf.Bytes(), []byte("\n"))
	for i, line := range lines {
		if trimmed := bytes.TrimLeft(line, " \t"); len(trimmed) > 0 && trimmed[0] == '#' {
			lines[i] = trimmed
		}
	}
	return bytes.Join(lines, []byte("\n")), nil
}

// detectLayout finds the encoding that reproduces raw byte for byte.
func detectLayout(doc *yaml.Node, raw []byte) (configLayout, bool) {
	for _, layout := range []configLayout{{4, true}, {4, false}, {2, true}, {2, false}} {
		if out, err := layout.encode(doc); err == nil && bytes.Equal(out, raw) {
			return layout, true
		}
	}
	return configLayout{}, false
}

// configKeyNodes indexes the API keys of a parsed config.yaml by auth_index
// as ReadCPAConfig does: a family under the v8 api-keys mapping replaces the
// older list, and of several keys with one auth_index the first counts.
func configKeyNodes(root *yaml.Node) map[string]keyNode {
	out := map[string]keyNode{}
	add := func(prefix, base, apiKey string, key keyNode) {
		base, apiKey = strings.TrimSpace(base), strings.TrimSpace(apiKey)
		if prefix == "meta-api-key" && base == "" {
			base = metaDefaultBaseURL
		}
		if apiKey == "" {
			return
		}
		if index := authIndexFor(prefix, base, apiKey); out[index].fields == nil {
			out[index] = key
		}
	}
	grouped := mappingValue(root, "api-keys")
	for _, family := range editableFamilies {
		if groups := mappingValue(grouped, family.kind); !isNull(groups) {
			if groups.Kind != yaml.SequenceNode {
				continue
			}
			for gi, group := range groups.Content {
				keys := mappingValue(group, "keys")
				if group.Kind != yaml.MappingNode || keys == nil || keys.Kind != yaml.SequenceNode {
					continue
				}
				inherited := mappingValue(group, "excluded-models")
				if isNull(inherited) || inherited.Kind != yaml.SequenceNode || len(inherited.Content) == 0 {
					inherited = nil
				}
				for ki, key := range keys.Content {
					if key.Kind == yaml.MappingNode {
						add(family.prefix, scalarValue(group, "base-url"), scalarValue(key, "api-key"),
							keyNode{fields: key, inherited: inherited, path: []any{"api-keys", family.kind, gi, "keys", ki}})
					}
				}
			}
			continue
		}
		entries := mappingValue(root, family.prefix)
		if entries == nil || entries.Kind != yaml.SequenceNode {
			continue
		}
		for ei, entry := range entries.Content {
			if entry.Kind == yaml.MappingNode {
				add(family.prefix, scalarValue(entry, "base-url"), scalarValue(entry, "api-key"), keyNode{fields: entry, path: []any{family.prefix, ei}})
			}
		}
	}
	return out
}

// edit adds or removes the entry naming model in the key's own
// excluded-models. Entries compare as CPA reads them: trimmed and without
// case.
func (k keyNode) edit(model string, exclude bool) EditOutcome {
	own := mappingValue(k.fields, "excluded-models")
	if isNull(own) {
		own = nil
	} else if own.Kind != yaml.SequenceNode {
		// Not a list CPA would read; the key is not touched.
		return EditUnchanged
	}
	effective := own
	if effective == nil {
		effective = k.inherited
	}
	names := func(list *yaml.Node, match func(string) bool) bool {
		return list != nil && slices.ContainsFunc(list.Content, func(n *yaml.Node) bool { return match(strings.TrimSpace(n.Value)) })
	}
	if names(effective, func(v string) bool { return v == configKeyDisabled }) {
		return EditDisabled
	}
	same := func(v string) bool { return strings.EqualFold(v, model) }
	if exclude {
		switch {
		case names(effective, same):
			return EditUnchanged
		case own == nil && k.inherited != nil:
			return EditInherited
		case own == nil:
			own = &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
			setMappingValue(k.fields, "excluded-models", own)
		}
		own.Content = append(own.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: model, Style: yaml.DoubleQuotedStyle})
		return EditApplied
	}
	if !names(own, same) {
		return EditUnchanged
	}
	own.Content = slices.DeleteFunc(own.Content, func(n *yaml.Node) bool { return same(strings.TrimSpace(n.Value)) })
	if len(own.Content) == 0 {
		deleteMappingKey(k.fields, "excluded-models")
	}
	return EditApplied
}

// onlyExclusionsDiffer checks that the encoded document decodes to the
// original one except for the excluded-models of the edited keys.
func onlyExclusionsDiffer(before, after []byte, edited [][]any) error {
	var old, updated any
	if err := yaml.Unmarshal(before, &old); err != nil {
		return fmt.Errorf("decode CPA config: %w", err)
	}
	if err := yaml.Unmarshal(after, &updated); err != nil {
		return fmt.Errorf("decode the edited CPA config: %w", err)
	}
	for _, path := range edited {
		from, _ := at(old, path).(map[string]any)
		to, _ := at(updated, path).(map[string]any)
		if from == nil || to == nil {
			return errors.New("the edited CPA config lost a key it edits; nothing was written")
		}
		if value, ok := to["excluded-models"]; ok {
			from["excluded-models"] = value
		} else {
			delete(from, "excluded-models")
		}
	}
	if !reflect.DeepEqual(old, updated) {
		return errors.New("editing excluded-models would change other parts of CPA config.yaml; nothing was written")
	}
	return nil
}

// at follows a path of mapping keys and sequence indexes in a decoded
// document.
func at(value any, path []any) any {
	for _, step := range path {
		switch step := step.(type) {
		case string:
			m, _ := value.(map[string]any)
			value = m[step]
		case int:
			list, _ := value.([]any)
			if step >= len(list) {
				return nil
			}
			value = list[step]
		}
	}
	return value
}

func mappingValue(m *yaml.Node, key string) *yaml.Node {
	if m == nil || m.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}
	return nil
}

func scalarValue(m *yaml.Node, key string) string {
	if n := mappingValue(m, key); n != nil && n.Kind == yaml.ScalarNode {
		return n.Value
	}
	return ""
}

func isNull(n *yaml.Node) bool {
	return n == nil || n.Kind == yaml.ScalarNode && n.Tag == "!!null"
}

// setMappingValue replaces the value of key, or adds the pair before the
// first key that sorts after it: CPA writes key fields in sorted order.
func setMappingValue(m *yaml.Node, key string, value *yaml.Node) {
	at := len(m.Content)
	for i := 0; i+1 < len(m.Content); i += 2 {
		switch {
		case m.Content[i].Value == key:
			m.Content[i+1] = value
			return
		case m.Content[i].Value > key && at == len(m.Content):
			at = i
		}
	}
	m.Content = slices.Insert(m.Content, at, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}, value)
}

func deleteMappingKey(m *yaml.Node, key string) {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			m.Content = slices.Delete(m.Content, i, i+2)
			return
		}
	}
}
