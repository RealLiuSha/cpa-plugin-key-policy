package adapt

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	channelA  = "0123456789abcdef"
	channelB  = "fedcba9876543210"
	editsPath = "/v1/images/edits"
)

var testNow = time.Date(2026, 10, 19, 1, 2, 3, 4, time.UTC)

func newTestManager(t *testing.T, canWrite func() bool) (*Manager, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "cpa-key-policy-adapters.json")
	m, err := NewManager(path, canWrite, func() time.Time { return testNow })
	if err != nil {
		t.Fatal(err)
	}
	return m, path
}

func mustUpsert(t *testing.T, m *Manager, rule Rule) Rule {
	t.Helper()
	saved, err := m.Upsert(rule)
	if err != nil {
		t.Fatalf("upsert %+v: %v", rule, err)
	}
	return saved
}

func TestTypes(t *testing.T) {
	types := Types()
	if len(types) != 3 || types[0].Type != TypeEditsImageField || !reflect.DeepEqual(types[0].DefaultPaths, []string{editsPath}) || types[0].NeedsField || types[0].NeedsValue {
		t.Fatalf("types = %+v", types)
	}
	if types[1].Type != TypeSetField || !types[1].NeedsField || !types[1].NeedsValue || types[2].Type != TypeDeleteField || !types[2].NeedsField || types[2].NeedsValue || types[2].DefaultPaths == nil {
		t.Fatalf("types = %+v", types)
	}
	types[0].DefaultPaths[0] = "/changed"
	if Types()[0].DefaultPaths[0] != editsPath {
		t.Fatal("Types shares its default paths with callers")
	}
}

func TestPreviewRewrites(t *testing.T) {
	images := Rule{Type: TypeEditsImageField}
	quality := func(value string, overwrite bool) Rule {
		return Rule{Type: TypeSetField, Field: "quality", Value: json.RawMessage(value), Overwrite: overwrite}
	}
	for name, tc := range map[string]struct {
		rule Rule
		body string
		want string // empty: unchanged
	}{
		"edits single string element":       {images, `{"model":"gpt-image-1","images":["data:image/png;base64,AAAA"],"prompt":"<b> & c"}`, `{"image":"data:image/png;base64,AAAA","model":"gpt-image-1","prompt":"<b> & c"}`},
		"edits object elements, nested url": {images, `{"images":[{"image_url":{"url":"data:a"}},{"image_url":"data:b"},{"url":"data:c"},{"image_url":""},{"file_id":"f"}],"n":1}`, `{"image":["data:a","data:b","data:c"],"n":1}`},
		"edits escaped keys and spacing":    {images, "{ \"im\\u0061ges\" : [ { \"image_url\" : { \"url\" : \"data:a\\\"b\" } } ] ,\n \"size\" : [1, 2] }", `{"image":"data:a\"b","size":[1, 2]}`},
		"edits image already present":       {images, `{"image":"data:x","images":[{"image_url":"data:a"}]}`, ""},
		"edits images not an array":         {images, `{"images":{"image_url":"data:a"}}`, ""},
		"edits no usable url":               {images, `{"images":[{"file_id":"f"},"",{"image_url":{"url":""}},null]}`, ""},
		"edits non-object body":             {images, `["data:a"]`, ""},
		"edits invalid JSON body":           {images, `{"images":["data:a"]`, ""},
		"set missing field":                 {quality(`"high"`, false), `{"model":"m"}`, `{"model":"m","quality":"high"}`},
		"set keeps present field":           {quality(`"high"`, false), `{"quality":"low"}`, ""},
		"set overwrites present field":      {quality(`"high"`, true), `{"quality":"low","n":1}`, `{"n":1,"quality":"high"}`},
		"set same value":                    {quality(`"high"`, true), `{"quality":"high"}`, ""},
		"set dotted field is one key":       {Rule{Type: TypeSetField, Field: "a.b", Value: json.RawMessage(`1`)}, `{"a":{"b":2}}`, `{"a":{"b":2},"a.b":1}`},
		"delete present field":              {Rule{Type: TypeDeleteField, Field: "quality"}, `{"model":"m","quality":"low"}`, `{"model":"m"}`},
		"delete missing field":              {Rule{Type: TypeDeleteField, Field: "quality"}, `{"model":"m"}`, ""},
	} {
		t.Run(name, func(t *testing.T) {
			out, changed, err := Preview(tc.rule, []byte(tc.body))
			if err != nil {
				t.Fatal(err)
			}
			if tc.want == "" {
				if changed || string(out) != tc.body {
					t.Fatalf("changed=%v out=%s, want the body unchanged", changed, out)
				}
				return
			}
			if !changed || string(out) != tc.want {
				t.Fatalf("changed=%v out=%s, want %s", changed, out, tc.want)
			}
		})
	}
	if _, _, err := Preview(Rule{Type: TypeSetField, Field: "quality"}, []byte(`{}`)); !errors.Is(err, ErrInvalidRule) {
		t.Fatalf("preview of a rule without value = %v", err)
	}
}

func TestUpsertValidation(t *testing.T) {
	m, path := newTestManager(t, nil)
	set := func(change func(*Rule)) Rule {
		rule := Rule{Channel: channelA, Type: TypeSetField, Paths: []string{editsPath}, Field: "quality", Value: json.RawMessage(`"high"`)}
		change(&rule)
		return rule
	}
	for name, rule := range map[string]Rule{
		"short channel":           set(func(r *Rule) { r.Channel = "0123" }),
		"non-hex channel":         set(func(r *Rule) { r.Channel = "0123456789abcdeg" }),
		"unknown type":            set(func(r *Rule) { r.Type = "rename_field" }),
		"set_field without paths": set(func(r *Rule) { r.Paths = nil }),
		"relative path":           set(func(r *Rule) { r.Paths = []string{"v1/images/edits"} }),
		"path with a space":       set(func(r *Rule) { r.Paths = []string{"/v1/images /edits"} }),
		"path too long":           set(func(r *Rule) { r.Paths = []string{"/" + strings.Repeat("a", 128)} }),
		"nine paths":              set(func(r *Rule) { r.Paths = strings.Fields("/1 /2 /3 /4 /5 /6 /7 /8 /9") }),
		"nested field":            set(func(r *Rule) { r.Field = "messages[0]" }),
		"field too long":          set(func(r *Rule) { r.Field = strings.Repeat("f", 65) }),
		"set_field without value": set(func(r *Rule) { r.Value = nil }),
		"set_field null value":    set(func(r *Rule) { r.Value = json.RawMessage(" null ") }),
		"invalid value":           set(func(r *Rule) { r.Value = json.RawMessage(`{"a":`) }),
		"value too large":         set(func(r *Rule) { r.Value = json.RawMessage(`"` + strings.Repeat("v", 4095) + `"`) }),
		"delete_field with value": set(func(r *Rule) { r.Type = TypeDeleteField }),
		"edits with a field":      set(func(r *Rule) { r.Type, r.Value = TypeEditsImageField, nil }),
		"note too long":           set(func(r *Rule) { r.Note = strings.Repeat("萝", 201) }),
	} {
		if _, err := m.Upsert(rule); !errors.Is(err, ErrInvalidRule) {
			t.Errorf("%s: err = %v, want ErrInvalidRule", name, err)
		}
	}
	if len(m.Rules()) != 0 {
		t.Fatalf("invalid rules were stored: %+v", m.Rules())
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("invalid rules wrote the file: %v", err)
	}
	edge := mustUpsert(t, m, set(func(r *Rule) {
		r.Paths = strings.Fields("/1 /2 /3 /4 /5 /6 /7 /8 /8")
		r.Field = strings.Repeat("f", 64)
		r.Value = json.RawMessage(`"` + strings.Repeat("v", 4094) + `"`)
		r.Note = strings.Repeat("萝", 200)
	}))
	if len(edge.Paths) != 8 || len(edge.Value) != 4096 {
		t.Fatalf("limits are inclusive: %+v", edge)
	}
}

func TestBookMatchAndApply(t *testing.T) {
	m, _ := newTestManager(t, nil)
	edits := mustUpsert(t, m, Rule{Channel: channelA, Type: TypeEditsImageField, Enabled: true})
	setN := mustUpsert(t, m, Rule{Channel: channelA, Type: TypeSetField, Paths: []string{editsPath}, Field: "n", Value: json.RawMessage(`1`), Enabled: true})
	mustUpsert(t, m, Rule{Channel: channelA, Type: TypeDeleteField, Paths: []string{"/v1/chat/completions"}, Field: "user"})
	dropUser := mustUpsert(t, m, Rule{Channel: channelB, Type: TypeDeleteField, Paths: []string{"/v1/chat/completions", "/v1/responses"}, Field: "user", Enabled: true})
	book := m.Book()
	for _, tc := range []struct {
		channel, path string
		want          bool
	}{
		{channelA, editsPath, true},
		{channelA, "/v1/images/generations", false},
		{channelA, "/v1/chat/completions", false}, // only a disabled rule
		{channelB, "/v1/responses", true},
		{channelB, editsPath, false},
		{"0000000000000000", editsPath, false},
	} {
		if got := book.Match(tc.channel, tc.path); got != tc.want {
			t.Errorf("Match(%s, %s) = %v, want %v", tc.channel, tc.path, got, tc.want)
		}
	}
	if allocs := testing.AllocsPerRun(100, func() {
		book.Match(channelA, editsPath)
		book.Match(channelB, editsPath)
	}); allocs != 0 {
		t.Fatalf("Match allocates %.0f times per call pair", allocs)
	}
	var none *Book
	if none.Match(channelA, editsPath) {
		t.Fatal("a nil Book matched")
	}
	for name, tc := range map[string]struct {
		channel, path, body, want string
		applied                   []string
	}{
		"rules run in order":          {channelA, editsPath, `{"images":["data:a"]}`, `{"image":"data:a","n":1}`, []string{edits.ID, setN.ID}},
		"only changing rules applied": {channelA, editsPath, `{"image":"data:a","images":["data:b"],"n":2}`, "", nil},
		"other path":                  {channelA, "/v1/images/generations", `{"images":["data:a"]}`, "", nil},
		"non-object body":             {channelA, editsPath, `"data:a"`, "", nil},
		"other channel":               {channelB, "/v1/responses", `{"input":"hi","user":"u"}`, `{"input":"hi"}`, []string{dropUser.ID}},
	} {
		out, applied, err := book.Apply(tc.channel, tc.path, []byte(tc.body))
		if err != nil || string(out) != tc.want || !reflect.DeepEqual(applied, tc.applied) || (tc.want == "" && out != nil) {
			t.Errorf("%s: out=%s applied=%v err=%v, want %s %v", name, out, applied, err, tc.want, tc.applied)
		}
	}
}

func TestManagerPersistsAndGuardsWrites(t *testing.T) {
	writable := true
	m, path := newTestManager(t, func() bool { return writable })
	if len(m.Rules()) != 0 || m.Book().Match(channelA, editsPath) {
		t.Fatal("a missing file must mean no rules")
	}
	edits := mustUpsert(t, m, Rule{Channel: " " + strings.ToUpper(channelA) + " ", Type: TypeEditsImageField, Enabled: true, Note: " Nbility 只认 image "})
	if !isRuleID(edits.ID) || edits.Channel != channelA || !reflect.DeepEqual(edits.Paths, []string{editsPath}) || !edits.UpdatedAt.Equal(testNow) || edits.Note != "Nbility 只认 image" {
		t.Fatalf("created rule = %+v", edits)
	}
	quality := mustUpsert(t, m, Rule{Channel: channelB, Type: TypeSetField, Paths: []string{editsPath, " " + editsPath}, Field: "quality", Value: json.RawMessage(`{ "level" : "<high>" }`), Overwrite: true})
	if !reflect.DeepEqual(quality.Paths, []string{editsPath}) || string(quality.Value) != `{"level":"<high>"}` || !quality.Overwrite || quality.Enabled {
		t.Fatalf("set_field rule = %+v", quality)
	}
	drop := mustUpsert(t, m, Rule{Channel: channelB, Type: TypeDeleteField, Paths: []string{"/v1/responses"}, Field: "user", Value: json.RawMessage("null"), Overwrite: true, Enabled: true})
	if drop.Value != nil || drop.Overwrite {
		t.Fatalf("delete_field rule = %+v", drop)
	}
	edits.Enabled, edits.Note = false, ""
	if updated := mustUpsert(t, m, edits); updated.ID != edits.ID || updated.Enabled {
		t.Fatalf("updated rule = %+v", updated)
	}
	if _, err := m.Upsert(Rule{ID: "r_0000000000000000", Channel: channelA, Type: TypeEditsImageField}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("upsert of an unknown id = %v", err)
	}
	want := m.Rules()
	if len(want) != 3 || want[0].ID != edits.ID || want[0].Enabled || want[1].ID != quality.ID || want[2].ID != drop.ID {
		t.Fatalf("rules = %+v", want)
	}

	reopened, err := NewManager(path, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := reopened.Rules(); !reflect.DeepEqual(got, want) {
		t.Fatalf("reopened rules = %+v\nwant %+v", got, want)
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("rule file mode = %v err=%v", info.Mode(), err)
	}
	if !reopened.Book().Match(channelB, "/v1/responses") || reopened.Book().Match(channelA, editsPath) || reopened.Book().Match(channelB, editsPath) {
		t.Fatal("reopened book does not index exactly the enabled rules")
	}

	writable = false
	before, _ := os.ReadFile(path)
	book := m.Book()
	if _, err := m.Upsert(Rule{Channel: channelA, Type: TypeEditsImageField}); !errors.Is(err, ErrReadOnly) {
		t.Fatalf("read-only upsert = %v", err)
	}
	if err := m.Delete(drop.ID); !errors.Is(err, ErrReadOnly) {
		t.Fatalf("read-only delete = %v", err)
	}
	if after, _ := os.ReadFile(path); !bytes.Equal(before, after) || !reflect.DeepEqual(m.Rules(), want) || m.Book() != book {
		t.Fatal("a refused write changed the file or the memory state")
	}
	writable = true

	m.RecordHits([]string{drop.ID, drop.ID, "r_ffffffffffffffff"})
	if hits := m.Hits(); len(hits) != 1 || hits[drop.ID].Count != 2 || !hits[drop.ID].LastAt.Equal(testNow) {
		t.Fatalf("hits = %+v", hits)
	}
	if err := m.Delete(drop.ID); err != nil {
		t.Fatal(err)
	}
	m.RecordHits([]string{drop.ID}) // an attempt that still held the previous Book
	if len(m.Hits()) != 0 || len(m.Rules()) != 2 || m.Book().Match(channelB, "/v1/responses") {
		t.Fatalf("after delete: hits=%+v rules=%+v", m.Hits(), m.Rules())
	}
	if err := m.Delete(drop.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second delete = %v", err)
	}

	// A failed save leaves memory alone: the file's path is now a directory.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	kept, book := m.Rules(), m.Book()
	if _, err := m.Upsert(Rule{Channel: channelA, Type: TypeEditsImageField}); err == nil || errors.Is(err, ErrInvalidRule) || errors.Is(err, ErrReadOnly) {
		t.Fatalf("upsert onto an unwritable path = %v", err)
	}
	if err := m.Delete(kept[0].ID); err == nil || errors.Is(err, ErrNotFound) || errors.Is(err, ErrReadOnly) {
		t.Fatalf("delete onto an unwritable path = %v", err)
	}
	if !reflect.DeepEqual(m.Rules(), kept) || m.Book() != book {
		t.Fatal("a failed save changed the memory state")
	}
}

func TestLoadRejectsMalformedFiles(t *testing.T) {
	rule := func(id, channel string) string {
		return fmt.Sprintf(`{"id":%q,"channel":%q,"type":"edits_image_field","enabled":true,"updated_at":"2026-10-19T00:00:00Z"}`, id, channel)
	}
	valid := rule("r_0123456789abcdef", channelA)
	for name, raw := range map[string]string{
		"truncated":       `{"version":1,"rules":[`,
		"unknown version": `{"version":2,"rules":[]}`,
		"unknown field":   `{"version":1,"rules":[],"extra":true}`,
		"invalid rule":    `{"version":1,"rules":[` + rule("r_0123456789abcdef", "xyz") + `]}`,
		"custom id":       `{"version":1,"rules":[` + rule("custom", channelA) + `]}`,
		"duplicate id":    `{"version":1,"rules":[` + valid + `,` + valid + `]}`,
	} {
		path := filepath.Join(t.TempDir(), "rules.json")
		if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := NewManager(path, nil, nil); err == nil {
			t.Errorf("%s: loaded without error", name)
		}
	}
}

func TestConcurrentApplyAndChanges(t *testing.T) {
	m, _ := newTestManager(t, nil)
	edits := mustUpsert(t, m, Rule{Channel: channelA, Type: TypeEditsImageField, Enabled: true})
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			for range 200 {
				out, applied, err := m.Book().Apply(channelA, editsPath, []byte(`{"images":["data:a"]}`))
				if err != nil || string(out) != `{"image":"data:a"}` {
					t.Errorf("apply: out=%s err=%v", out, err)
					return
				}
				m.RecordHits(applied)
				_ = m.Hits()
			}
		})
	}
	for i := range 3 {
		mustUpsert(t, m, Rule{Channel: channelB, Type: TypeDeleteField, Paths: []string{"/v1/responses"}, Field: fmt.Sprintf("f%d", i), Enabled: true})
		_ = m.Rules()
	}
	wg.Wait()
	if got := m.Hits()[edits.ID].Count; got != 800 {
		t.Fatalf("hits = %d, want 800", got)
	}
}
