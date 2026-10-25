package native

import (
	"slices"
	"strings"
	"time"
)

// State is what the evidence says about one channel's native search.
type State string

const (
	// StateUnknown: never probed.
	StateUnknown State = "unknown"
	// StateSupported: every conclusive probe in the history searched.
	StateSupported State = "supported"
	// StateUnstable: the history holds probes that searched and probes that
	// did not; the upstream ignores the tool part of the time.
	StateUnstable State = "unstable"
	// StateNoSearch: the upstream answers but never runs the tool.
	StateNoSearch State = "no_search"
	// StateFailing: the latest probes got no usable response.
	StateFailing State = "failing"
	// StateStale: the latest search is too old to vouch for the channel.
	StateStale State = "stale"
	// StateRejected: a client request failed because the upstream refused
	// the web_search tool; cleared by a probe that searches again.
	StateRejected State = "rejected"
	// StateDisabled: the operator took the channel out of native search.
	StateDisabled State = "disabled"
)

// Mode is how Codex searches with one model.
type Mode string

const (
	// ModeNative: the catalog tells Codex to send the web_search tool with
	// the conversation, and the channel serving the turn runs it.
	ModeNative Mode = "native"
	// ModeFallback: Codex keeps calling /v1/alpha/search.
	ModeFallback Mode = "fallback"
	// ModeExcluded: the operator keeps the model on alpha search.
	ModeExcluded Mode = "excluded"
	// ModeOff: native search is switched off.
	ModeOff Mode = "off"
)

// Result is the outcome of one probe.
type Result string

const (
	ResultSearch   Result = "search"
	ResultNoSearch Result = "no_search"
	ResultError    Result = "error"
)

// historyLimit is how many probes a channel keeps; its state is read from
// them, so an upstream that searches only part of the time shows as
// unstable for this many rounds.
const historyLimit = 6

// staleAfter is how long the latest search vouches for a channel: two
// rounds and an hour, so one missed round does not switch a model back.
func staleAfter(intervalHours int) time.Duration {
	return time.Duration(2*intervalHours+1) * time.Hour
}

// classify reads a channel's state from its record. Disabled and rejected
// win over the probes; then the probes decide: no probe is unknown, two
// failures in a row (or only failures) are failing, and the conclusive
// probes are supported, no_search or, mixed, unstable. A supported channel
// whose latest search is older than staleAfter is stale.
func classify(rec *channelRecord, disabled bool, intervalHours int, now time.Time) State {
	switch {
	case disabled:
		return StateDisabled
	case rec == nil || len(rec.Probes) == 0:
		if rec != nil && rec.Rejection != nil {
			return StateRejected
		}
		return StateUnknown
	case rec.Rejection != nil:
		return StateRejected
	}
	probes := rec.Probes
	last := probes[len(probes)-1]
	if last.Result == ResultError && (len(probes) == 1 || probes[len(probes)-2].Result == ResultError) {
		return StateFailing
	}
	var searched, ignored bool
	var lastSearch time.Time
	for _, p := range probes {
		switch p.Result {
		case ResultSearch:
			searched, lastSearch = true, p.At
		case ResultNoSearch:
			ignored = true
		}
	}
	switch {
	case searched && ignored:
		return StateUnstable
	case ignored:
		return StateNoSearch
	case !searched:
		return StateFailing
	case now.Sub(lastSearch) > staleAfter(intervalHours):
		return StateStale
	}
	return StateSupported
}

// Gap is a channel serving a native model on which sessions do not search,
// and why.
type Gap struct {
	AuthIndex string `json:"auth_index"`
	State     State  `json:"state"`
}

// ModelDecision is how one catalog model searches and the evidence for it.
// Usable are the serving channels whose native search is supported; Gaps the
// others.
type ModelDecision struct {
	Model  string   `json:"model"`
	Mode   Mode     `json:"mode"`
	Usable []string `json:"usable"`
	Gaps   []Gap    `json:"gaps"`
	// Reason explains a fallback: no_channel (no credential serves the
	// model), no_usable (none is supported yet) or incomplete (RequireAll and
	// a serving channel is not supported).
	Reason string `json:"reason,omitempty"`
}

// decisions is the immutable snapshot the request path reads.
type decisions struct {
	enabled bool
	native  map[string]bool
	// candidates are the catalog models native search manages; a client
	// with a catalog from before a switch back still sends their tool.
	candidates map[string]bool
	// strip holds the channels a candidate's web_search tool is taken out
	// for: disabled by the operator, or refusing the tool.
	strip  map[string]bool
	models []ModelDecision
	states map[string]State
}

// serves reports whether credential c may serve model. A credential whose
// config lists no models serves the provider's own, unless CPA answered that
// it lacks this one.
func serves(c Credential, rec *channelRecord, model string) bool {
	if c.Models != nil {
		return slices.Contains(c.Models, model)
	}
	return rec == nil || !slices.Contains(rec.NotServing, model)
}

// servesKnown reports whether c is known to serve model: its config lists
// it, or a probe of it got through. Only such a channel can make a model
// native.
func servesKnown(c Credential, rec *channelRecord, model string) bool {
	if c.Models != nil {
		return slices.Contains(c.Models, model)
	}
	return rec != nil && slices.Contains(rec.Served, model)
}

// decide builds the snapshot. A model searches natively when native search
// is on, the operator did not exclude it and at least one channel known to
// serve it is supported: no model switches on unknown or stale evidence.
// With RequireAll every channel that may serve it must be supported or
// disabled. Credentials CPA disabled serve nothing. Nothing is stripped while
// native search is off.
func decide(settings Settings, candidates []string, credentials []Credential, records map[string]*channelRecord, now time.Time) *decisions {
	d := &decisions{enabled: settings.Enabled, native: map[string]bool{}, candidates: map[string]bool{}, strip: map[string]bool{}, states: map[string]State{}}
	disabled := make(map[string]bool, len(settings.DisabledChannels))
	for _, index := range settings.DisabledChannels {
		disabled[index] = true
	}
	for _, c := range credentials {
		state := classify(records[c.AuthIndex], disabled[c.AuthIndex], settings.IntervalHours, now)
		d.states[c.AuthIndex] = state
		if settings.Enabled && (state == StateDisabled || state == StateRejected) {
			d.strip[c.AuthIndex] = true
		}
	}
	for _, model := range candidates {
		d.candidates[model] = true
		decision := ModelDecision{Model: model, Usable: []string{}, Gaps: []Gap{}}
		allSupported := true
		for _, c := range credentials {
			rec := records[c.AuthIndex]
			if c.Disabled || !serves(c, rec, model) {
				continue
			}
			state := d.states[c.AuthIndex]
			if state == StateSupported && servesKnown(c, rec, model) {
				decision.Usable = append(decision.Usable, c.AuthIndex)
				continue
			}
			if state == StateSupported {
				// Supported, but never seen serving this model.
				state = StateUnknown
			}
			decision.Gaps = append(decision.Gaps, Gap{AuthIndex: c.AuthIndex, State: state})
			if state != StateDisabled {
				allSupported = false
			}
		}
		switch {
		case !settings.Enabled:
			decision.Mode = ModeOff
		case slices.ContainsFunc(settings.ExcludedModels, func(m string) bool { return strings.EqualFold(m, model) }):
			decision.Mode = ModeExcluded
		case len(decision.Usable) == 0 && len(decision.Gaps) == 0:
			decision.Mode, decision.Reason = ModeFallback, "no_channel"
		case len(decision.Usable) == 0:
			decision.Mode, decision.Reason = ModeFallback, "no_usable"
		case settings.RequireAll && !allSupported:
			decision.Mode, decision.Reason = ModeFallback, "incomplete"
		default:
			decision.Mode = ModeNative
			d.native[model] = true
		}
		d.models = append(d.models, decision)
	}
	return d
}
