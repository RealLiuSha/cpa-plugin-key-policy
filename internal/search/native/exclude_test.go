package native

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"cpa-key-policy/internal/channel"
)

// configCPA is CPA over a config.yaml in its v8 layout: the file's API keys
// are the credentials, a model a key does not list (or excludes) has no
// route there, and edits go through channel.EditExcludedModels.
type configCPA struct {
	path     string
	mu       sync.Mutex
	searches map[string]map[string]bool
	writes   int
}

const exclusionConfig = `config-version: 8
api-keys:
    codex:
        - base-url: "https://zeu.example"
          keys:
            - api-key: "sk-zeu"
              models:
                - name: "gpt-6-astra"
                - name: "gpt-6-sol"
              weight: 8
          name: "codex-15"
# Kimera: its images are served elsewhere.
        - base-url: "https://kimera.example"
          keys:
            - api-key: "sk-kimera"
              excluded-models:
                - "gpt-image-*"
              models:
                - name: "gpt-6-astra"
                - name: "gpt-6-sol"
              weight: 1
          name: "codex-11"
`

func (f *configCPA) read() map[string]channel.Credential {
	credentials, err := channel.ReadCPAConfig(f.path)
	if err != nil {
		panic(err)
	}
	return credentials
}

func (f *configCPA) index(group string) string {
	for index, c := range f.read() {
		if c.Group == group {
			return index
		}
	}
	panic("no group " + group)
}

func (f *configCPA) Credentials() ([]Credential, error) {
	out := []Credential{}
	for _, c := range f.read() {
		out = append(out, Credential{AuthIndex: c.AuthIndex, ID: c.ID, Provider: c.Provider, Name: c.Group, Group: c.Group,
			Models: c.Models, Disabled: c.Disabled, InConfig: true, Excluded: c.ExcludedModels})
	}
	return out, nil
}

func (f *configCPA) Stream(_ context.Context, c Credential, model string, _ any, _ http.Header) ([]byte, int, error) {
	if !slices.Contains(f.read()[c.AuthIndex].Models, model) {
		return nil, 500, fmt.Errorf("%w: auth_not_found", ErrNotServing)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.searches[c.AuthIndex][model] {
		return stream("web_search_call", "message"), 200, nil
	}
	return stream("message"), 200, nil
}

func (f *configCPA) EditExcluded(edits []Edit) ([]EditResult, error) {
	changes := make([]channel.ModelEdit, len(edits))
	for i, e := range edits {
		changes[i] = channel.ModelEdit{AuthIndex: e.AuthIndex, Model: e.Model, Exclude: e.Exclude}
	}
	outcomes, err := channel.EditExcludedModels(f.path, changes)
	if err != nil {
		return nil, err
	}
	results := make([]EditResult, len(outcomes))
	for i, outcome := range outcomes {
		results[i] = map[channel.EditOutcome]EditResult{channel.EditApplied: EditDone, channel.EditUnchanged: EditDone,
			channel.EditDisabled: EditKeyDisabled, channel.EditInherited: EditKeyInherits, channel.EditNoKey: EditKeyGone}[outcome]
		if outcome == channel.EditApplied {
			f.writes++
		}
	}
	return results, nil
}

// TestAutoExclusionFollowsTheEvidence runs Kimera's case end to end over a
// config.yaml: a channel that searches with gpt-6-astra and not with
// gpt-6-sol is taken off gpt-6-sol once a refusal and a probe confirm it, and
// only while another channel searches for it; it is given back on trial and
// on the operator's word, and when the evidence or the switch says so.
func TestAutoExclusionFollowsTheEvidence(t *testing.T) {
	dir := t.TempDir()
	cpa := &configCPA{path: filepath.Join(dir, "config.yaml")}
	if err := os.WriteFile(cpa.path, []byte(exclusionConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	zeu, kimera := cpa.index("codex-15"), cpa.index("codex-11")
	cpa.searches = map[string]map[string]bool{zeu: {"gpt-6-astra": true, "gpt-6-sol": true}, kimera: {"gpt-6-astra": true}}
	c := &clock{now: time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)}
	m, err := Open(Options{Path: filepath.Join(dir, "native.json"), Upstream: cpa, Now: c.Now})
	if err != nil {
		t.Fatal(err)
	}
	m.RewriteCatalog([]byte(catalog))
	m.Start()
	c.now = c.now.Add(startupGrace)
	runTick(m)
	excluded := func() []string { return cpa.read()[kimera].ExcludedModels }
	exclusion := func() ExclusionStatus {
		t.Helper()
		list := m.Status().Exclusions
		if len(list) != 1 || list[0].AuthIndex != kimera || list[0].Model != "gpt-6-sol" {
			t.Fatalf("exclusions %+v", list)
		}
		return list[0]
	}

	// Kimera's answer to a required tool_choice it dropped the tool of.
	m.ObserveFailure(kimera, "gpt-6-sol", 400, `{"error":{"code":"E1001","message":"required tool choice has no available tool"}}`)
	if !m.Strips(kimera, "gpt-6-sol") || m.Strips(kimera, "gpt-6-astra") {
		t.Fatal("a refusal for gpt-6-sol strips the tool for that model only")
	}
	if cpa.writes != 0 {
		t.Fatal("a refusal alone took the model off")
	}
	// The recheck confirms it; the next tick writes it.
	c.now = c.now.Add(rejectionRecheck)
	runTick(m)
	runTick(m)
	if cpa.writes != 1 || !slices.Equal(excluded(), []string{"gpt-image-*", "gpt-6-sol"}) || !slices.Equal(cpa.read()[kimera].Models, []string{"gpt-6-astra"}) {
		t.Fatalf("after the confirming probe: %d writes, kimera excludes %v", cpa.writes, excluded())
	}
	if e := exclusion(); e.State != ExclusionActive || e.NoSearch != 1 || e.Rejection == nil || !e.RetryAt.Equal(c.now.Add(12*time.Hour)) {
		t.Fatalf("exclusion %+v", e)
	}
	if sol, astra := modeOf(t, m, "gpt-6-sol"), modeOf(t, m, "gpt-6-astra"); sol.Mode != ModeNative || !slices.Equal(sol.Usable, []string{zeu}) ||
		len(sol.Gaps) != 0 || len(astra.Usable) != 2 {
		t.Fatalf("gpt-6-sol %+v, gpt-6-astra %+v (Kimera searches for astra)", sol, astra)
	}

	// Without Zeu no channel searches for gpt-6-sol: Kimera serves it again.
	settings := m.Settings()
	settings.DisabledChannels = []string{zeu}
	if _, _, err := m.Update(settings); err != nil {
		t.Fatal(err)
	}
	if raw, _ := os.ReadFile(cpa.path); string(raw) != exclusionConfig || exclusion().State != ExclusionBlocked {
		t.Fatalf("with the last searching channel disabled: config\n%s\nexclusion %+v", raw, exclusion())
	}
	settings.DisabledChannels = nil
	if _, _, err := m.Update(settings); err != nil {
		t.Fatal(err)
	}
	if cpa.writes != 3 || exclusion().State != ExclusionActive {
		t.Fatalf("Zeu back: %d writes, %+v", cpa.writes, exclusion())
	}

	// The trial gives the model back; a probe that does not search takes it
	// off again, the next trial twice as late.
	c.now = c.now.Add(12 * time.Hour)
	runTick(m)
	if e := exclusion(); e.State != ExclusionTrial || len(excluded()) != 1 {
		t.Fatalf("trial: %+v, kimera excludes %v", e, excluded())
	}
	runTick(m)
	if e := exclusion(); e.State != ExclusionActive || e.Trials != 1 || !e.RetryAt.Equal(c.now.Add(24*time.Hour)) || cpa.writes != 5 {
		t.Fatalf("after the trial: %+v, %d writes", e, cpa.writes)
	}

	// The operator's word: released, it stays; resumed, it goes again.
	if err := m.Release(kimera, "gpt-6-sol"); err != nil {
		t.Fatal(err)
	}
	c.now = c.now.Add(time.Minute)
	runTick(m)
	if e := exclusion(); e.State != ExclusionKept || e.KeptBy != KeptByOperator || len(excluded()) != 1 {
		t.Fatalf("released: %+v, kimera excludes %v", e, excluded())
	}
	if err := m.Resume(kimera, "gpt-6-sol"); err != nil || exclusion().State != ExclusionActive {
		t.Fatalf("resumed: %v %+v", err, exclusion())
	}
	// An entry taken out of CPA's config by hand is the operator's call too.
	if _, err := channel.EditExcludedModels(cpa.path, []channel.ModelEdit{{AuthIndex: kimera, Model: "gpt-6-sol"}}); err != nil {
		t.Fatal(err)
	}
	c.now = c.now.Add(time.Minute)
	runTick(m)
	runTick(m)
	if e := exclusion(); e.State != ExclusionKept || e.KeptBy != KeptByConfig || len(excluded()) != 1 {
		t.Fatalf("entry removed by hand: %+v, kimera excludes %v", e, excluded())
	}
	if err := m.Resume(kimera, "gpt-6-sol"); err != nil || exclusion().State != ExclusionActive {
		t.Fatalf("resumed again: %v %+v", err, exclusion())
	}

	// Switching automatic exclusion off gives everything back.
	settings.AutoExclude = false
	if _, _, err := m.Update(settings); err != nil {
		t.Fatal(err)
	}
	if raw, _ := os.ReadFile(cpa.path); string(raw) != exclusionConfig || len(m.Status().Exclusions) != 0 {
		t.Fatalf("switched off: config\n%s\nexclusions %+v", raw, m.Status().Exclusions)
	}

	// On again, the evidence still holds; once Kimera searches for
	// gpt-6-sol, its trial ends the exclusion for good.
	settings.AutoExclude = true
	if _, _, err := m.Update(settings); err != nil || exclusion().State != ExclusionActive {
		t.Fatalf("on again: %v %+v", err, m.Status().Exclusions)
	}
	cpa.mu.Lock()
	cpa.searches[kimera]["gpt-6-sol"] = true
	cpa.mu.Unlock()
	c.now = c.now.Add(12 * time.Hour)
	runTick(m)
	runTick(m)
	raw, _ := os.ReadFile(cpa.path)
	if !bytes.Equal(raw, []byte(exclusionConfig)) || len(m.Status().Exclusions) != 0 || modeOf(t, m, "gpt-6-sol").Mode != ModeNative {
		t.Fatalf("recovered: config\n%s\nexclusions %+v", raw, m.Status().Exclusions)
	}
	// Its history still holds the probes that did not search, so it serves
	// gpt-6-sol unvouched for until they rotate out.
	if sol := modeOf(t, m, "gpt-6-sol"); !slices.Equal(sol.Gaps, []Gap{{kimera, StateUnstable}}) {
		t.Fatalf("gpt-6-sol once Kimera searches for it: %+v", sol)
	}

	// Everything above outlives a restart.
	reopened, err := Open(Options{Path: m.path, Upstream: cpa, Now: c.Now})
	if err != nil || !reopened.Settings().AutoExclude || len(reopened.Status().Exclusions) != 0 {
		t.Fatalf("reopened: %v %+v", err, reopened.Status())
	}
}
