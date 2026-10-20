package plugin

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"cpa-key-policy/internal/adapt"
	"cpa-key-policy/internal/channel"
	"cpa-key-policy/internal/policy/audit"
	"cpa-key-policy/internal/search"
)

// The management console: the routes behind the 萝卜 pages other than keys,
// prices, statistics and audit. Each handler reads one module and answers
// in one round trip, so a page needs a single request.

const (
	failureRateAttention = 0.2
	failureRateMinimum   = 5
)

// moduleUnavailable answers a route whose module could not be opened.
func moduleUnavailable(name string) ManagementResponse {
	return jsonError(http.StatusServiceUnavailable, "module_unavailable", name+" is unavailable; see the settings page")
}

// moduleWriteError maps a module write failure to its HTTP answer. After
// quiesce every module refuses to write, because a newer instance owns the
// files.
func (a *App) moduleWriteError(err error) ManagementResponse {
	switch {
	case a.quiesced.Load(), errors.Is(err, adapt.ErrReadOnly), errors.Is(err, search.ErrReadOnly), errors.Is(err, channel.ErrReadOnly):
		return a.writeRefused()
	case errors.Is(err, adapt.ErrNotFound):
		return jsonError(http.StatusNotFound, "not_found", err.Error())
	case errors.Is(err, adapt.ErrInvalidRule), errors.Is(err, search.ErrInvalidSettings), errors.Is(err, channel.ErrInvalid):
		return jsonError(http.StatusBadRequest, "invalid_request", err.Error())
	default:
		return jsonError(http.StatusInternalServerError, "persist_failed", err.Error())
	}
}

// channelView is GET /channels' row: the registry's view plus the number of
// adapter rules attached to the channel.
type channelView struct {
	channel.View
	Rules int `json:"rules"`
}

func (a *App) listChannels() ManagementResponse {
	set := a.modules.Load()
	if set == nil || set.channels == nil {
		return moduleUnavailable("channels")
	}
	rules := map[string]int{}
	if set.adapters != nil {
		for _, rule := range set.adapters.Rules() {
			rules[rule.Channel]++
		}
	}
	views := set.channels.List()
	out := make([]channelView, 0, len(views))
	for _, view := range views {
		out = append(out, channelView{View: view, Rules: rules[view.AuthIndex]})
	}
	groups, ids := a.cpaCredentials.groups()
	return jsonResponse(http.StatusOK, map[string]any{"channels": out, "groups": groups, "ids": ids})
}

func (a *App) channelDetail(authIndex string) ManagementResponse {
	set := a.modules.Load()
	if set == nil || set.channels == nil {
		return moduleUnavailable("channels")
	}
	authIndex = strings.ToLower(strings.TrimSpace(authIndex))
	detail, ok := set.channels.Detail(authIndex)
	if !ok {
		return jsonError(http.StatusNotFound, "not_found", "unknown channel")
	}
	rules := []ruleView{}
	if set.adapters != nil {
		hits := set.adapters.Hits()
		for _, rule := range set.adapters.Rules() {
			if rule.Channel == authIndex {
				rules = append(rules, ruleView{Rule: rule, Hits: hits[rule.ID]})
			}
		}
	}
	return jsonResponse(http.StatusOK, map[string]any{"channel": struct {
		channel.Detail
		Rules       int        `json:"rules"`
		RulesDetail []ruleView `json:"rules_detail"`
	}{Detail: detail, Rules: len(rules), RulesDetail: rules}})
}

func (a *App) patchChannel(body []byte) ManagementResponse {
	set := a.modules.Load()
	if set == nil || set.channels == nil {
		return moduleUnavailable("channels")
	}
	var patch channel.MetaPatch
	if err := decodeStrictBody(body, &patch); err != nil {
		return jsonError(http.StatusBadRequest, "invalid_json", err.Error())
	}
	a.channelPatchMu.Lock()
	defer a.channelPatchMu.Unlock()
	before, _ := set.channels.Detail(strings.ToLower(strings.TrimSpace(patch.AuthIndex)))
	meta, err := set.channels.UpdateMeta(patch)
	if err != nil {
		return a.moduleWriteError(err)
	}
	changes := map[string]audit.Change{}
	for name, values := range map[string][2]any{
		"display_name": {before.Meta.DisplayName, meta.DisplayName},
		"vendor":       {before.Meta.Vendor, meta.Vendor},
		"note":         {before.Meta.Note, meta.Note},
	} {
		if values[0] != values[1] {
			changes[name] = audit.Change{From: values[0], To: values[1]}
		}
	}
	if len(changes) > 0 {
		a.store.RecordChange("update_channel", meta.AuthIndex, changes)
	}
	return jsonResponse(http.StatusOK, map[string]any{"meta": meta})
}

// deleteChannel forgets a channel CPA's config.yaml no longer has: its
// statistics, metadata and the adapter rules bound to it. A channel still in
// the config is refused, since its next attempt would only bring it back
// without its history. When the config cannot be read nothing is refused.
func (a *App) deleteChannel(authIndex string) ManagementResponse {
	set := a.modules.Load()
	if set == nil || set.channels == nil {
		return moduleUnavailable("channels")
	}
	index := strings.ToLower(strings.TrimSpace(authIndex))
	if _, configured := a.cpaCredentials.lookup(index); configured {
		return jsonError(http.StatusConflict, "channel_in_cpa", "the channel is still in CPA's config.yaml; remove it there first")
	}
	a.channelPatchMu.Lock()
	defer a.channelPatchMu.Unlock()
	before, _ := set.channels.Detail(index)
	found, err := set.channels.Delete(index)
	if err != nil {
		return a.moduleWriteError(err)
	}
	removedRules := 0
	if set.adapters != nil {
		for _, rule := range set.adapters.Rules() {
			if rule.Channel != index {
				continue
			}
			if err := set.adapters.Delete(rule.ID); err != nil {
				return a.moduleWriteError(err)
			}
			a.store.RecordChange("delete_adapter", rule.ID, map[string]audit.Change{"rule": {From: rule, To: nil}})
			removedRules++
		}
	}
	if !found && removedRules == 0 {
		return jsonError(http.StatusNotFound, "not_found", "unknown channel")
	}
	a.store.RecordChange("delete_channel", index, map[string]audit.Change{"channel": {From: before.View, To: nil}})
	return jsonResponse(http.StatusOK, map[string]any{"deleted": index, "adapter_rules_deleted": removedRules})
}

// ruleView is a rule with its hit counter.
type ruleView struct {
	adapt.Rule
	Hits adapt.Hit `json:"hits"`
}

func (a *App) listAdapters() ManagementResponse {
	set := a.modules.Load()
	if set == nil || set.adapters == nil {
		return moduleUnavailable("adapters")
	}
	hits := set.adapters.Hits()
	rules := set.adapters.Rules()
	out := make([]ruleView, 0, len(rules))
	for _, rule := range rules {
		out = append(out, ruleView{Rule: rule, Hits: hits[rule.ID]})
	}
	return jsonResponse(http.StatusOK, map[string]any{"rules": out, "types": adapt.Types()})
}

func (a *App) upsertAdapter(body []byte) ManagementResponse {
	set := a.modules.Load()
	if set == nil || set.adapters == nil {
		return moduleUnavailable("adapters")
	}
	var rule adapt.Rule
	if err := decodeStrictBody(body, &rule); err != nil {
		return jsonError(http.StatusBadRequest, "invalid_json", err.Error())
	}
	var before *adapt.Rule
	for _, existing := range set.adapters.Rules() {
		if existing.ID == rule.ID && rule.ID != "" {
			copied := existing
			before = &copied
		}
	}
	stored, err := set.adapters.Upsert(rule)
	if err != nil {
		return a.moduleWriteError(err)
	}
	a.store.RecordChange("upsert_adapter", stored.ID, map[string]audit.Change{"rule": {From: before, To: stored}})
	return jsonResponse(http.StatusOK, map[string]any{"rule": stored})
}

func (a *App) deleteAdapter(id string) ManagementResponse {
	set := a.modules.Load()
	if set == nil || set.adapters == nil {
		return moduleUnavailable("adapters")
	}
	id = strings.TrimSpace(id)
	var before *adapt.Rule
	for _, existing := range set.adapters.Rules() {
		if existing.ID == id {
			copied := existing
			before = &copied
		}
	}
	if err := set.adapters.Delete(id); err != nil {
		return a.moduleWriteError(err)
	}
	a.store.RecordChange("delete_adapter", id, map[string]audit.Change{"rule": {From: before, To: nil}})
	return jsonResponse(http.StatusOK, map[string]any{"deleted": id})
}

func (a *App) previewAdapter(body []byte) ManagementResponse {
	set := a.modules.Load()
	if set == nil || set.adapters == nil {
		return moduleUnavailable("adapters")
	}
	var req struct {
		Rule   *adapt.Rule     `json:"rule,omitempty"`
		RuleID string          `json:"rule_id,omitempty"`
		Body   json.RawMessage `json:"body"`
	}
	if err := decodeStrictBody(body, &req); err != nil {
		return jsonError(http.StatusBadRequest, "invalid_json", err.Error())
	}
	rule := req.Rule
	if rule == nil {
		for _, existing := range set.adapters.Rules() {
			if existing.ID == strings.TrimSpace(req.RuleID) {
				copied := existing
				rule = &copied
			}
		}
	}
	if rule == nil {
		return jsonError(http.StatusNotFound, "not_found", "unknown adapter rule")
	}
	out, changed, err := adapt.Preview(*rule, req.Body)
	if err != nil {
		return jsonError(http.StatusBadRequest, "invalid_request", err.Error())
	}
	if !changed {
		out = req.Body
	}
	return jsonResponse(http.StatusOK, map[string]any{"changed": changed, "body": json.RawMessage(out)})
}

// tryProtocols maps a rule path to the entry protocol CPA executes it with.
var tryProtocols = map[string]string{
	"/v1/images/edits":       "openai-image",
	"/v1/images/generations": "openai-image",
	"/v1/responses":          "openai-response",
	// Codex CLI's direct route runs the same Responses handler in CPA.
	"/backend-api/codex/responses": "openai-response",
	"/v1/chat/completions":         "openai",
	"/v1/messages":                 "claude",
}

const tryResponseLimit = 4096

// tryAdapter applies a rule to a sample body and sends it once through CPA,
// pinned to the rule's channel. CPA skips this plugin's own hooks for a call
// the plugin makes, so the rule is applied here first. The call is real: it
// may cost money and counts toward the channel's cooldown, but it carries no
// downstream key and is not billed. callbackID is the management request's
// host callback id.
func (a *App) tryAdapter(body []byte, callbackID string) ManagementResponse {
	set := a.modules.Load()
	if set == nil || set.adapters == nil || set.channels == nil {
		return moduleUnavailable("adapters")
	}
	if a.host == nil {
		return jsonError(http.StatusServiceUnavailable, "host_unavailable", "host callbacks are unavailable")
	}
	var req struct {
		RuleID string          `json:"rule_id"`
		Body   json.RawMessage `json:"body"`
	}
	if err := decodeStrictBody(body, &req); err != nil {
		return jsonError(http.StatusBadRequest, "invalid_json", err.Error())
	}
	var rule *adapt.Rule
	for _, existing := range set.adapters.Rules() {
		if existing.ID == strings.TrimSpace(req.RuleID) {
			copied := existing
			rule = &copied
		}
	}
	if rule == nil {
		return jsonError(http.StatusNotFound, "not_found", "unknown adapter rule")
	}
	// The credential CPA reported for the channel, or for an API key of
	// CPA's config.yaml the ID CPA derives for it: a rule can be tried before
	// the channel took traffic or after its idle statistics were released.
	authID := ""
	if view, ok := set.channels.Detail(rule.Channel); ok {
		authID = view.AuthID
	}
	if authID == "" {
		if credential, found := a.cpaCredentials.lookup(rule.Channel); found {
			authID = credential.ID
		}
	}
	if authID == "" {
		return jsonError(http.StatusConflict, "channel_not_seen", "no request has used this channel yet, so its credential cannot be pinned; send one real request through CPA first")
	}
	path := ""
	for _, candidate := range rule.Paths {
		if _, known := tryProtocols[candidate]; known {
			path = candidate
			break
		}
	}
	if path == "" {
		return jsonError(http.StatusBadRequest, "unsupported_path", "a test request can only be sent for "+strings.Join(sortedKeys(tryProtocols), ", "))
	}
	sample, changed, err := adapt.Preview(*rule, req.Body)
	if err != nil {
		return jsonError(http.StatusBadRequest, "invalid_request", err.Error())
	}
	if !changed {
		sample = req.Body
	}
	var model struct {
		Model string `json:"model"`
	}
	_ = json.Unmarshal(sample, &model)
	call, err := json.Marshal(map[string]any{
		"entry_protocol": tryProtocols[path], "exit_protocol": "", "model": model.Model, "stream": false,
		"body": []byte(sample), "headers": http.Header{"Content-Type": {"application/json"}},
		"path": path, "auth_id": authID,
		// CPA runs the execution on the management HTTP request's context
		// only with this id; without it nothing cancels the upstream call
		// when the browser, a proxy or CPA's own shutdown drops the request.
		"host_callback_id": callbackID,
	})
	if err != nil {
		return jsonError(http.StatusInternalServerError, "json_error", err.Error())
	}
	// The audit line is the only write. It goes in before the call, under
	// writeMu like every other write, so the call does not hold quiesce up
	// and the line cannot land after the files were handed over.
	if !a.recordUnlessQuiesced("try_adapter", rule.ID, map[string]audit.Change{"channel": {To: rule.Channel}}) {
		return a.writeRefused()
	}
	started := time.Now()
	raw, callErr := a.host.Call("host.model.execute", call)
	latency := time.Since(started).Milliseconds()

	var env struct {
		OK     bool `json:"ok"`
		Result struct {
			StatusCode int    `json:"status_code"`
			Body       []byte `json:"body"`
		} `json:"result"`
		Error *EnvelopeError `json:"error"`
	}
	if err := json.Unmarshal(raw, &env); err != nil && callErr != nil {
		return jsonError(http.StatusBadGateway, "host_call_failed", callErr.Error())
	}
	status, text := env.Result.StatusCode, string(env.Result.Body)
	if !env.OK && env.Error != nil {
		status, text = env.Error.HTTPStatus, env.Error.Message
		if status == 0 {
			status = http.StatusBadGateway
		}
	}
	truncated := len(text) > tryResponseLimit
	if truncated {
		text = text[:tryResponseLimit]
	}
	return jsonResponse(http.StatusOK, map[string]any{"status_code": status, "latency_ms": latency, "auth_id": authID, "body": text, "truncated": truncated})
}

// recordUnlessQuiesced writes one audit line for a call that does not hold
// writeMu, unless the instance has been quiesced since the call came in.
func (a *App) recordUnlessQuiesced(action, target string, changes map[string]audit.Change) bool {
	a.writeMu.RLock()
	defer a.writeMu.RUnlock()
	if a.quiesced.Load() {
		return false
	}
	a.store.RecordChange(action, target, changes)
	return true
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func (a *App) searchState() ManagementResponse {
	set := a.modules.Load()
	if set == nil || set.search == nil {
		return moduleUnavailable("search")
	}
	return jsonResponse(http.StatusOK, map[string]any{
		"enabled":  a.pluginSettings().SearchEnabled,
		"declared": a.declaredRouter.Load(),
		"settings": set.search.Settings(),
		"status":   set.search.Status(),
	})
}

func (a *App) updateSearch(body []byte) ManagementResponse {
	set := a.modules.Load()
	if set == nil || set.search == nil {
		return moduleUnavailable("search")
	}
	var req struct {
		Settings      search.Settings `json:"settings"`
		ClearProbeKey bool            `json:"clear_probe_key,omitempty"`
	}
	if err := decodeStrictBody(body, &req); err != nil {
		return jsonError(http.StatusBadRequest, "invalid_json", err.Error())
	}
	before := set.search.Settings()
	stored, err := set.search.Update(req.Settings, req.ClearProbeKey)
	if err != nil {
		return a.moduleWriteError(err)
	}
	a.syncSearch(set, a.pluginSettings())
	a.store.RecordChange("update_search", "search", map[string]audit.Change{"settings": {From: before, To: stored}})
	return jsonResponse(http.StatusOK, map[string]any{"settings": stored, "status": set.search.Status()})
}

func (a *App) importSearch(body []byte) ManagementResponse {
	set := a.modules.Load()
	if set == nil || set.search == nil {
		return moduleUnavailable("search")
	}
	var req struct {
		Config map[string]any `json:"config"`
	}
	if err := decodeStrictBody(body, &req); err != nil {
		return jsonError(http.StatusBadRequest, "invalid_json", err.Error())
	}
	imported, err := search.FromPluginConfig(req.Config)
	if errors.Is(err, search.ErrNothingToImport) {
		return jsonError(http.StatusConflict, "nothing_to_import", err.Error()+"; the current settings are unchanged")
	}
	if err != nil {
		return jsonError(http.StatusBadRequest, "invalid_config", err.Error())
	}
	before := set.search.Settings()
	stored, err := set.search.Update(imported, false)
	if err != nil {
		return a.moduleWriteError(err)
	}
	a.syncSearch(set, a.pluginSettings())
	a.store.RecordChange("import_search", "search", map[string]audit.Change{"settings": {From: before, To: stored}})
	return jsonResponse(http.StatusOK, map[string]any{"settings": stored})
}

// moduleStates is the modules object shared by /overview and /settings.
func (a *App) moduleStates(set *moduleSet) map[string]any {
	states := map[string]any{
		"search": map[string]any{"enabled": a.pluginSettings().SearchEnabled, "declared": a.declaredRouter.Load()},
	}
	if set == nil {
		return states
	}
	if set.quality != nil {
		states["quality"] = map[string]any{"running": set.quality.Running()}
	}
	if set.adapters != nil {
		rules := set.adapters.Rules()
		enabled := 0
		for _, rule := range rules {
			if rule.Enabled {
				enabled++
			}
		}
		states["adapters"] = map[string]any{"rules": len(rules), "enabled_rules": enabled}
	}
	if set.channels != nil {
		states["channels"] = map[string]any{"tracked": len(set.channels.List())}
	}
	return states
}

// channelAttention is a channel the overview points out: a high failure
// rate, or an inspection verdict (degraded or suspect, or group_low for a
// whole group, with no auth_index) with the model and the answers.
type channelAttention struct {
	AuthIndex   string  `json:"auth_index"`
	DisplayName string  `json:"display_name"`
	Reason      string  `json:"reason"`
	Value       float64 `json:"value"`
	Model       string  `json:"model,omitempty"`
	Effort      string  `json:"effort,omitempty"`
	Answered    int     `json:"answered,omitempty"`
	Correct     int     `json:"correct,omitempty"`
}

type keyAttention struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	Status        string `json:"status"`
	BlockedReason string `json:"blocked_reason,omitempty"`
}

func (a *App) overview() ManagementResponse {
	set := a.modules.Load()
	today := a.store.Stats(1).Totals
	// The channel numbers stay zero while the channels module is unavailable.
	numbers := map[string]any{
		"requests": today.Calls, "billed_usd": today.ChargedUSD,
		"attempts": 0, "failures": 0, "list_cost_usd": 0.0,
	}

	keys := a.store.Keys()
	enabled, limited, warning := 0, 0, 0
	keyList := []keyAttention{}
	for _, key := range keys {
		if key.Enabled {
			enabled++
		}
		summary := a.store.UsageSummaryFor(key)
		switch summary.Status {
		case "limited":
			limited++
		case "warning":
			warning++
		default:
			continue
		}
		keyList = append(keyList, keyAttention{ID: key.ID, Name: key.Name, Status: summary.Status, BlockedReason: summary.BlockedReason})
	}
	sort.Slice(keyList, func(i, j int) bool {
		return keyList[i].Status < keyList[j].Status || keyList[i].Status == keyList[j].Status && keyList[i].ID < keyList[j].ID
	})

	channels := map[string]any{"total": 0, "attention": []channelAttention{}}
	if set != nil && set.channels != nil {
		views := set.channels.List()
		attention := []channelAttention{}
		// A channel without a name of its own is named by its group in CPA's
		// config, as the channel page does, rather than by its auth_index.
		groups, _ := a.cpaCredentials.groups()
		names := make(map[string]string, len(views))
		for _, view := range views {
			names[view.AuthIndex] = cmp.Or(view.Meta.DisplayName, groups[view.AuthIndex])
		}
		var attempts, failures int64
		var listCost float64
		for _, view := range views {
			attempts += view.Today.Attempts
			failures += view.Today.Failures
			listCost += view.Today.ListCostUSD
			if day := view.Last24h; day.Attempts >= failureRateMinimum && 1-day.SuccessRate >= failureRateAttention {
				attention = append(attention, channelAttention{AuthIndex: view.AuthIndex, DisplayName: names[view.AuthIndex], Reason: "failure_rate", Value: 1 - day.SuccessRate})
			}
		}
		numbers["attempts"] = attempts
		numbers["failures"] = failures
		numbers["list_cost_usd"] = listCost
		if set.quality != nil {
			for _, item := range set.quality.Attention() {
				name := names[item.AuthIndex]
				if item.AuthIndex != "" && name == "" {
					name = groups[item.AuthIndex]
				}
				attention = append(attention, channelAttention{AuthIndex: item.AuthIndex, DisplayName: name, Reason: item.Verdict,
					Value: float64(item.Correct) / float64(item.Answered), Model: item.Model, Effort: item.Effort, Answered: item.Answered, Correct: item.Correct})
			}
		}
		channels = map[string]any{"total": len(views), "attention": attention}
	}

	recent, _ := a.store.AuditEvents("", "", 10)
	if recent == nil {
		recent = []audit.Event{}
	}
	// Billing piles up in memory while the ledger cannot be saved, and a crash
	// would lose it: the landing page says so, not only settings and stats.
	warnings := a.configWarnings()
	if health := a.store.Health(); health.LastFlushError != "" {
		since := health.LastFlushAt
		if since == "" {
			since = "none since start"
		}
		warnings = append(warnings, fmt.Sprintf("usage ledger not saved (%d failed writes, last success %s): %s", health.FlushFailures, since, health.LastFlushError))
	}
	return jsonResponse(http.StatusOK, map[string]any{
		"today":          numbers,
		"keys":           map[string]any{"total": len(keys), "enabled": enabled, "limited": limited, "warning": warning, "attention": keyList},
		"channels":       channels,
		"modules":        a.moduleStates(set),
		"warnings":       warnings,
		"recent_changes": recent,
	})
}

// dataFile is one file on the settings page.
type dataFile struct {
	Name     string `json:"name"`
	Path     string `json:"path"`
	Size     int64  `json:"size"`
	Modified string `json:"modified,omitempty"`
	Missing  bool   `json:"missing,omitempty"`
}

func (a *App) settingsPage() ManagementResponse {
	set := a.modules.Load()
	statePath := a.store.StatePath()
	dir := filepath.Dir(statePath)
	files := []dataFile{}
	for _, item := range []struct{ name, path string }{
		{"state", statePath},
		{"usage", filepath.Join(dir, "cpa-key-policy-usage.json")},
		{"history", filepath.Join(dir, "cpa-key-policy-history.json")},
		{"audit", filepath.Join(dir, "cpa-key-policy-audit.jsonl")},
		{"adapters", filepath.Join(dir, adaptersFile)},
		{"search", filepath.Join(dir, searchFile)},
		{"channels", filepath.Join(dir, channelsFile)},
		{"channel_stats", filepath.Join(dir, channelStatsFile)},
		{"quality", filepath.Join(dir, qualityFile)},
		{"inspection", filepath.Join(dir, inspectionFile)},
	} {
		file := dataFile{Name: item.name, Path: item.path}
		if info, err := os.Stat(item.path); err == nil {
			file.Size, file.Modified = info.Size(), info.ModTime().UTC().Format(time.RFC3339)
		} else {
			file.Missing = true
		}
		files = append(files, file)
	}
	moduleErrors := map[string]string{}
	if set != nil {
		moduleErrors = set.errors
	}
	handoff := "idle"
	switch {
	case a.readOnly.Load():
		handoff = "read_only"
	case a.quiesced.Load():
		handoff = "quiesced"
	}
	source, digest := a.ui.Describe()
	return jsonResponse(http.StatusOK, map[string]any{
		"version":         Version,
		"plugin_id":       PluginID,
		"display_name":    PluginName,
		"modules":         a.moduleStates(set),
		"module_errors":   moduleErrors,
		"hooks":           a.timings.snapshot(),
		"config_warnings": a.configWarnings(),
		"files":           files,
		"ui":              map[string]any{"source": source, "sha256": digest},
		"ownership":       map[string]any{"owner": a.canWrite(), "handoff": handoff},
		"health":          a.store.Health(),
		"timezone":        a.store.Timezone(),
	})
}

// configWarnings lists the warnings of the configuration in effect, then why
// the latest one was rejected, if it was. It never returns nil, so the JSON
// answer is always a list.
func (a *App) configWarnings() []string {
	warnings := []string{}
	if applied := a.warnings.Load(); applied != nil {
		warnings = append(warnings, *applied...)
	}
	if rejected := a.rejected.Load(); rejected != nil {
		warnings = append(warnings, *rejected)
	}
	return warnings
}
