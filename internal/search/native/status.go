package native

import (
	"slices"
	"time"
)

// ChannelStatus is one channel serving a catalog model, for the management
// UI. Models are the catalog models it serves and ModelStates its state for
// each; Probes its history, newest first; Rejection its latest refusal.
type ChannelStatus struct {
	AuthIndex   string           `json:"auth_index"`
	Name        string           `json:"name"`
	Group       string           `json:"group,omitempty"`
	Models      []string         `json:"models"`
	ModelStates map[string]State `json:"model_states"`
	State       State            `json:"state"`
	CPADisabled bool             `json:"cpa_disabled,omitempty"`
	Unavailable bool             `json:"unavailable,omitempty"`
	Probes      []Probe          `json:"probes"`
	Rejection   *Rejection       `json:"rejection,omitempty"`
}

// RoundStatus is the probe round under way.
type RoundStatus struct {
	Kind    string    `json:"kind"`
	Started time.Time `json:"started"`
	Done    int       `json:"done"`
	Total   int       `json:"total"`
}

// Status is the module's live state. NextFullRound is zero while rounds do
// not run (switched off, or the search module is off). ExclusionError is why
// CPA's config could not be edited the last time, if it could not.
type Status struct {
	Settings         Settings          `json:"settings"`
	Models           []ModelDecision   `json:"models"`
	Channels         []ChannelStatus   `json:"channels"`
	Exclusions       []ExclusionStatus `json:"exclusions"`
	ExclusionError   string            `json:"exclusion_error,omitempty"`
	Round            *RoundStatus      `json:"round,omitempty"`
	LastFullRound    time.Time         `json:"last_full_round,omitzero"`
	NextFullRound    time.Time         `json:"next_full_round,omitzero"`
	Running          bool              `json:"running"`
	CredentialsError string            `json:"credentials_error,omitempty"`
}

// Status reports the live state.
func (m *Manager) Status() Status {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	d := m.snapshot.Load()
	out := Status{Settings: m.settings.clone(), Models: slices.Clone(d.models), Channels: []ChannelStatus{},
		Exclusions: m.exclusionStatusLocked(), ExclusionError: m.editErr,
		LastFullRound: m.lastFull, Running: m.started, CredentialsError: m.credErr}
	if out.Models == nil {
		out.Models = []ModelDecision{}
	}
	if m.started && m.settings.Enabled {
		out.NextFullRound = m.lastFull.Add(time.Duration(m.settings.IntervalHours) * time.Hour)
		if earliest := m.startedAt.Add(startupGrace); m.lastFull.IsZero() || out.NextFullRound.Before(earliest) {
			out.NextFullRound = earliest
		}
	}
	if r := m.round; r != nil {
		out.Round = &RoundStatus{Kind: r.kind, Started: r.started, Done: r.done, Total: r.total}
	}
	candidates := m.candidatesLocked(now)
	for _, c := range m.credentials {
		rec := m.records[c.AuthIndex]
		served := []string{}
		for _, model := range candidates {
			if serves(c, rec, model) {
				served = append(served, model)
			}
		}
		if len(served) == 0 {
			continue
		}
		status := ChannelStatus{AuthIndex: c.AuthIndex, Name: c.Name, Group: c.Group, Models: served, ModelStates: map[string]State{}, State: d.states[c.AuthIndex],
			CPADisabled: c.Disabled, Unavailable: c.Unavailable, Probes: []Probe{}}
		if status.State == "" {
			status.State = StateUnknown
		}
		for _, model := range served {
			if state, ok := d.modelStates[pair{c.AuthIndex, model}]; ok {
				status.ModelStates[model] = state
			}
		}
		if rec != nil {
			for i := len(rec.Probes) - 1; i >= 0; i-- {
				status.Probes = append(status.Probes, rec.Probes[i])
			}
			if r := rec.latestRejection(); r != nil {
				rejection := *r
				status.Rejection = &rejection
			}
		}
		out.Channels = append(out.Channels, status)
	}
	return out
}
