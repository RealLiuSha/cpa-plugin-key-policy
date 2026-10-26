package native

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

// Automatic exclusion. Every turn of a native model carries the web_search
// tool, so a channel that serves the model without running the tool leaves
// its share of those sessions without search, and the request path cannot
// move a turn to another channel. With AutoExclude, a config API key
// confirmed not to search for a native model (pairRecord.confirmed) is taken
// off that model with an excluded-models entry in CPA's config, so CPA routes
// the model to its other channels; it keeps serving every other model.
//
// Guards: only native models, and only while another channel searches for
// the model (decide makes a model native only then, so taking a channel that
// does not search off it never leaves it without one); nothing is added in
// the startupGrace after Start or from an incomplete credential list; keys
// CPA disabled ("*") are left alone, and so are keys that take their v8
// group's excluded-models (a list of their own would stop following it).
// Every change is one write of CPA's config, which CPA reloads as a whole,
// reconfiguring every plugin; credentials keep their IDs and session
// bindings, those to the model taken off rebinding. So the edits of one tick
// go in one write.
//
// An excluded model gets no probes, CPA having no route to it there. So it is
// given back on trial after retryAfter: a probe of it then decides, a search
// ending the exclusion and anything else taking the model off again, with
// the next trial twice as late. TRADEOFF: a trial is two writes of CPA's
// config, at most about two a week for each pair after the first month;
// probing the upstream directly instead would bypass CPA's codex executor and
// could disagree with it. Revisit if those writes disturb sessions.
//
// The operator's word wins: an entry taken out of CPA's config by anyone but
// this plugin, or released on the search page, keeps the channel serving the
// model until the operator resumes it; an entry this plugin did not write is
// never removed.

// Edit takes a model off a config API key in CPA's config (Exclude) or gives
// it back; Reason says why, for the audit log.
type Edit struct {
	AuthIndex string
	Model     string
	Exclude   bool
	Reason    string
}

// EditResult is what became of one Edit.
type EditResult string

const (
	// EditDone: CPA's config now says what the edit asked.
	EditDone EditResult = "done"
	// EditKeyDisabled: the key excludes every model ("*"), so CPA routes
	// nothing to it; it was left as the operator set it.
	EditKeyDisabled EditResult = "key_disabled"
	// EditKeyInherits: the key takes its v8 group's excluded-models, so it
	// was left as it is.
	EditKeyInherits EditResult = "key_inherits"
	// EditKeyGone: CPA's config has no such key.
	EditKeyGone EditResult = "key_gone"
)

// ExclusionState is where an automatic exclusion stands.
type ExclusionState string

const (
	// ExclusionActive: CPA's config takes the model off the channel.
	ExclusionActive ExclusionState = "active"
	// ExclusionPending: the next write takes the model off the channel (or
	// the last one failed; Error says why).
	ExclusionPending ExclusionState = "pending"
	// ExclusionTrial: the model is back on the channel until a probe tells
	// whether it searches there now.
	ExclusionTrial ExclusionState = "trial"
	// ExclusionBlocked: the channel does not search for the model, but no
	// other channel searches for it either, so it keeps serving it.
	ExclusionBlocked ExclusionState = "blocked"
	// ExclusionKept: the operator keeps the channel serving the model.
	ExclusionKept ExclusionState = "kept"
)

// How an exclusion came to be kept.
const (
	// KeptByOperator: released on the search page.
	KeptByOperator = "operator"
	// KeptByConfig: the entry was taken out of CPA's config by someone else.
	KeptByConfig = "config"
)

const (
	// trialLimit is how long a trial waits for a probe to answer before the
	// model is taken off again.
	trialLimit = time.Hour
	// maxRetry caps the time between trials.
	maxRetry = 7 * 24 * time.Hour
)

// ErrUnknownExclusion reports a release or resume of a pair with no
// automatic exclusion.
var ErrUnknownExclusion = errors.New("no automatic exclusion of this model on this channel")

// exclusion is one model taken, or to be taken, off one channel.
type exclusion struct {
	AuthIndex string    `json:"auth_index"`
	Model     string    `json:"model"`
	Since     time.Time `json:"since"`
	// Written says CPA's config holds the entry this plugin wrote.
	Written bool `json:"written,omitempty"`
	// Trials counts the trials after which the channel still did not
	// search; RetryAt is when the next begins (set when the entry is
	// written), TrialAt when the current one began.
	Trials  int       `json:"trials,omitempty"`
	RetryAt time.Time `json:"retry_at,omitzero"`
	TrialAt time.Time `json:"trial_at,omitzero"`
	// KeptAt and KeptBy say when and how the operator kept the channel
	// serving the model.
	KeptAt time.Time `json:"kept_at,omitzero"`
	KeptBy string    `json:"kept_by,omitempty"`
	// Error is why the last write of the entry failed.
	Error string `json:"error,omitempty"`

	// want is whether the entry belongs in CPA's config, as last planned.
	want bool
}

func (e *exclusion) state() ExclusionState {
	switch {
	case !e.KeptAt.IsZero():
		return ExclusionKept
	case !e.TrialAt.IsZero():
		return ExclusionTrial
	case e.Written:
		return ExclusionActive
	case e.want:
		return ExclusionPending
	}
	return ExclusionBlocked
}

// retryAfter is how long after the trials so far the next one begins: the
// probe interval, doubled with each trial, up to maxRetry.
func (m *Manager) retryAfter(trials int) time.Duration {
	wait := time.Duration(m.settings.IntervalHours) * time.Hour
	for ; trials > 0 && wait < maxRetry; trials-- {
		wait *= 2
	}
	return min(wait, maxRetry)
}

// excludableLocked reports whether a channel confirmed not to search for the
// model would be taken off it: automatic exclusion is on, CPA's config holds
// the key and routes to it, and the operator does not keep it serving the
// model. decide counts such a channel as no gap under RequireAll.
func (m *Manager) excludableLocked(authIndex, model string) bool {
	if !m.settings.AutoExclude {
		return false
	}
	if e := m.exclusions[pair{authIndex, model}]; e != nil && !e.KeptAt.IsZero() {
		return false
	}
	i := slices.IndexFunc(m.credentials, func(c Credential) bool { return c.AuthIndex == authIndex })
	return i >= 0 && m.credentials[i].InConfig && !m.credentials[i].Disabled
}

// planned is one edit of a reconcile and the exclusion it is for; drop
// removes the exclusion once its entry is out of CPA's config.
type planned struct {
	edit Edit
	e    *exclusion
	drop bool
}

// planLocked moves every exclusion along and returns the edits that bring
// CPA's config in line. It adds exclusions for the confirmed gaps of native
// models, settles trials, notices entries someone else took out of CPA's
// config (or put in), and drops exclusions that no longer hold.
func (m *Manager) planLocked(now time.Time) []planned {
	active := m.settings.Enabled && m.settings.AutoExclude && m.started
	// Adds wait for a settled, complete view of CPA's credentials; giving a
	// model back is always safe.
	canAdd := active && m.credErr == "" && now.Sub(m.startedAt) >= startupGrace
	d := m.snapshot.Load()
	byIndex := make(map[string]Credential, len(m.credentials))
	for _, c := range m.credentials {
		byIndex[c.AuthIndex] = c
	}
	if canAdd {
		for _, decision := range d.models {
			if decision.Mode != ModeNative {
				continue
			}
			for _, gap := range decision.Gaps {
				key := pair{gap.AuthIndex, decision.Model}
				if gap.State == StateDisabled || m.exclusions[key] != nil || !m.excludableLocked(gap.AuthIndex, decision.Model) ||
					!m.records[gap.AuthIndex].pair(decision.Model).confirmed() {
					continue
				}
				m.exclusions[key] = &exclusion{AuthIndex: key.authIndex, Model: key.model, Since: now.UTC()}
				m.dirty = true
			}
		}
	}
	// A list read before the last edit may not show it.
	fresh := m.listedAt.After(m.editedAt)
	var edits []planned
	for key, e := range m.exclusions {
		c, listed := byIndex[key.authIndex]
		p := m.records[key.authIndex].pair(key.model)
		if fresh && listed && c.InConfig {
			present := slices.ContainsFunc(c.Excluded, func(entry string) bool { return strings.EqualFold(entry, key.model) })
			switch {
			case e.Written && !present:
				e.Written, e.want, m.dirty = false, false, true
				if e.KeptAt.IsZero() {
					e.KeptAt, e.KeptBy = now.UTC(), KeptByConfig
					m.log("info", fmt.Sprintf("native search: the entry taking %s off channel %s left CPA's config; the channel keeps serving it until resumed on the search page", key.model, key.authIndex))
				}
			case !e.Written && present:
				// An entry this plugin did not write is the operator's.
				delete(m.exclusions, key)
				m.dirty = true
				continue
			}
		}
		native := d.native[key.model]
		reason := ""
		switch {
		case listed && c.Disabled:
			// CPA routes nothing to the key; its entries wait until the
			// operator enables it again.
			e.want = e.Written
		case !e.KeptAt.IsZero():
			e.want, reason = false, "kept by the operator"
		case !active:
			e.want, reason = false, "automatic exclusion is off"
		case !e.TrialAt.IsZero():
			// A round is started after the reconcile of its tick, so a probe
			// at the very instant the trial began is part of it.
			answered := p != nil && !p.LastProbe.Before(e.TrialAt)
			searched := answered && p.LastResult == ResultSearch
			ignored := answered && p.LastResult == ResultNoSearch || p != nil && p.Rejection != nil && !p.Rejection.At.Before(e.TrialAt)
			switch {
			case searched:
				e.TrialAt, e.want, reason = time.Time{}, false, "it searches natively again"
				m.log("info", fmt.Sprintf("native search: channel %s searches natively for %s again; the exclusion ends", key.authIndex, key.model))
			case ignored || now.Sub(e.TrialAt) >= trialLimit:
				if ignored {
					e.Trials++
				}
				e.TrialAt, m.dirty = time.Time{}, true
				e.want, reason = native, "it still does not search natively"
			default:
				e.want = false
			}
		case !d.candidates[key.model]:
			e.want, reason = false, "the model left the Codex catalog"
		case !p.confirmed():
			e.want, reason = false, "it searches natively again"
		case e.Written && native && !now.Before(e.RetryAt):
			e.TrialAt, e.want, m.dirty = now.UTC(), false, true
			reason = "trial: probe whether it searches natively now"
		case !native:
			e.want, reason = false, "no other channel searches natively for the model"
		default:
			e.want, reason = true, "it does not search natively"
		}
		// An exclusion that no longer holds goes once its entry is out.
		drop := e.KeptAt.IsZero() && e.TrialAt.IsZero() && (!active || !p.confirmed() || !d.candidates[key.model])
		if drop && !e.Written {
			delete(m.exclusions, key)
			m.dirty = true
			continue
		}
		// An entry to add waits for canAdd, pending meanwhile.
		if e.want != e.Written && (!e.want || canAdd) {
			edits = append(edits, planned{edit: Edit{AuthIndex: key.authIndex, Model: key.model, Exclude: e.want, Reason: reason}, e: e, drop: drop})
		}
	}
	slices.SortFunc(edits, func(a, b planned) int {
		return strings.Compare(a.edit.AuthIndex+"\x00"+a.edit.Model, b.edit.AuthIndex+"\x00"+b.edit.Model)
	})
	return edits
}

// reconcile brings CPA's config in line with the exclusions, after prepare
// (if any) changed them under the lock, and reports whether it edited the
// config. A failed write is kept on the exclusions it was for and returned;
// the next tick tries again.
func (m *Manager) reconcile(prepare func() error) (bool, error) {
	m.editMu.Lock()
	defer m.editMu.Unlock()
	m.mu.Lock()
	if prepare != nil {
		if err := prepare(); err != nil {
			m.mu.Unlock()
			return false, err
		}
	}
	var edits []planned
	if m.canWrite() {
		edits = m.planLocked(m.now())
	}
	m.mu.Unlock()
	if len(edits) == 0 {
		return false, nil
	}
	list := make([]Edit, len(edits))
	for i, p := range edits {
		list[i] = p.edit
	}
	results, err := m.upstream.EditExcluded(list)
	if err == nil && len(results) != len(list) {
		err = fmt.Errorf("CPA config edit answered %d of %d edits", len(results), len(list))
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.dirty = true
	if err != nil {
		for _, p := range edits {
			p.e.Error = err.Error()
		}
		if err.Error() != m.editErr {
			m.log("warn", "native search: cannot edit CPA's config, retrying every minute: "+err.Error())
		}
		m.editErr = err.Error()
		return false, err
	}
	m.editErr, m.editedAt = "", m.now()
	for i, p := range edits {
		key := pair{p.edit.AuthIndex, p.edit.Model}
		switch results[i] {
		case EditDone:
			p.e.Written, p.e.Error = p.edit.Exclude, ""
			if p.edit.Exclude {
				p.e.RetryAt = m.now().Add(m.retryAfter(p.e.Trials)).UTC()
			}
			verb := "gave %s back to channel %s"
			if p.edit.Exclude {
				verb = "took %s off channel %s"
			}
			m.log("info", fmt.Sprintf("native search: "+verb+" in CPA's config: %s", p.edit.Model, p.edit.AuthIndex, p.edit.Reason))
			if p.drop && m.exclusions[key] == p.e {
				delete(m.exclusions, key)
			}
		case EditKeyDisabled:
			p.e.Error = "the key is disabled in CPA (excluded-models \"*\"), so its entries are left as they are"
		case EditKeyInherits:
			p.e.Error = "the key takes its group's excluded-models in CPA's config, which a list of its own would stop following; give the key its own excluded-models in CPA to let the model be taken off it"
		case EditKeyGone:
			if m.exclusions[key] == p.e {
				delete(m.exclusions, key)
			}
		}
	}
	return true, nil
}

// Release keeps the channel serving the model: its automatic exclusion ends
// now and does not come back until Resume. The entry leaves CPA's config
// before it returns; a write that fails is returned and retried every minute.
func (m *Manager) Release(authIndex, model string) error {
	return m.keep(authIndex, model, true)
}

// Resume lets automatic exclusion take the model off the channel again, at
// once while the evidence holds.
func (m *Manager) Resume(authIndex, model string) error {
	return m.keep(authIndex, model, false)
}

func (m *Manager) keep(authIndex, model string, kept bool) error {
	if !m.canWrite() {
		return ErrReadOnly
	}
	authIndex, model = strings.ToLower(strings.TrimSpace(authIndex)), strings.TrimSpace(model)
	_, err := m.reconcile(func() error {
		e := m.exclusions[pair{authIndex, model}]
		if e == nil {
			return ErrUnknownExclusion
		}
		if e.KeptAt.IsZero() == kept {
			e.KeptAt, e.KeptBy, e.TrialAt = time.Time{}, "", time.Time{}
			if kept {
				e.KeptAt, e.KeptBy = m.now().UTC(), KeptByOperator
			}
			m.dirty = true
			m.rebuildLocked()
		}
		return nil
	})
	return err
}

// ExclusionStatus is one automatic exclusion for the management UI, with the
// evidence behind it: the model's probes in a row on the channel that did not
// search, and its refusal.
type ExclusionStatus struct {
	AuthIndex string         `json:"auth_index"`
	Name      string         `json:"name"`
	Group     string         `json:"group,omitempty"`
	Model     string         `json:"model"`
	State     ExclusionState `json:"state"`
	Since     time.Time      `json:"since"`
	Trials    int            `json:"trials"`
	RetryAt   time.Time      `json:"retry_at,omitzero"`
	TrialAt   time.Time      `json:"trial_at,omitzero"`
	KeptAt    time.Time      `json:"kept_at,omitzero"`
	KeptBy    string         `json:"kept_by,omitempty"`
	Error     string         `json:"error,omitempty"`
	NoSearch  int            `json:"no_search"`
	Rejection *Rejection     `json:"rejection,omitempty"`
}

func (m *Manager) exclusionStatusLocked() []ExclusionStatus {
	out := []ExclusionStatus{}
	for key, e := range m.exclusions {
		status := ExclusionStatus{AuthIndex: key.authIndex, Model: key.model, State: e.state(), Since: e.Since, Trials: e.Trials,
			TrialAt: e.TrialAt, KeptAt: e.KeptAt, KeptBy: e.KeptBy, Error: e.Error}
		if status.State == ExclusionActive {
			status.RetryAt = e.RetryAt
		}
		if i := slices.IndexFunc(m.credentials, func(c Credential) bool { return c.AuthIndex == key.authIndex }); i >= 0 {
			status.Name, status.Group = m.credentials[i].Name, m.credentials[i].Group
		}
		if p := m.records[key.authIndex].pair(key.model); p != nil {
			status.NoSearch = p.NoSearch
			if p.Rejection != nil {
				rejection := *p.Rejection
				status.Rejection = &rejection
			}
		}
		out = append(out, status)
	}
	slices.SortFunc(out, func(a, b ExclusionStatus) int {
		return strings.Compare(a.Model+"\x00"+a.AuthIndex, b.Model+"\x00"+b.AuthIndex)
	})
	return out
}
