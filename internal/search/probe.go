package search

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

// probeLoop is one run of the probe goroutine. Each Start creates a new one,
// so the router can start again after Stop.
type probeLoop struct {
	cancel context.CancelFunc
	done   chan struct{}
}

func (l *probeLoop) running() bool {
	if l == nil {
		return false
	}
	select {
	case <-l.done:
		return false
	default:
		return true
	}
}

// startLoop starts probing the current snapshot unless a loop is running or
// probing is off. r.mu must be held.
func (r *Router) startLoop() {
	if previous := r.loop.Load(); previous != nil {
		if previous.running() {
			return
		}
		previous.cancel()
		r.loop.Store(nil)
	}
	st := r.state.Load()
	if !st.settings.probing() {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	loop := &probeLoop{cancel: cancel, done: make(chan struct{})}
	r.loop.Store(loop)
	go r.runProbes(ctx, loop, st)
}

// stopLoop cancels the probe loop, which also aborts a probe in flight, and
// waits until the goroutine has exited. r.mu must be held.
func (r *Router) stopLoop() {
	loop := r.loop.Swap(nil)
	if loop == nil {
		return
	}
	loop.cancel()
	<-loop.done
	r.log("info", "alpha search: probes stopped")
}

// runProbes probes once at start and then every interval, skipping rounds
// while searches are idle. It only touches st, the channel atomics and the
// router's search clock, never r.mu.
func (r *Router) runProbes(ctx context.Context, loop *probeLoop, st *routerState) {
	defer close(loop.done)
	defer r.recoverPanic(st, "probe loop stopped")
	s := st.settings
	names := make([]string, len(s.Channels))
	for i, ch := range s.Channels {
		names[i] = ch.Name
	}
	message := fmt.Sprintf("alpha search: probing channels %s every %ds with model %s",
		strings.Join(names, ","), s.Probe.IntervalSeconds, s.probeModel())
	if s.Probe.IdleSeconds > 0 {
		message += fmt.Sprintf(", paused after %ds without searches", s.Probe.IdleSeconds)
	}
	r.log("info", message)

	r.safeProbeRound(ctx, st)
	ticker := time.NewTicker(time.Duration(s.Probe.IntervalSeconds) * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if r.idle(s.Probe.IdleSeconds, time.Now()) {
				continue
			}
			r.safeProbeRound(ctx, st)
		}
	}
}

// safeProbeRound keeps a panic inside one round: an unrecovered panic in a
// plugin goroutine ends the whole CPA process.
func (r *Router) safeProbeRound(ctx context.Context, st *routerState) {
	defer r.recoverPanic(st, "probe round aborted")
	r.probeRound(ctx, st)
}

func (r *Router) recoverPanic(st *routerState, what string) {
	if p := recover(); p != nil {
		r.log("warn", "alpha search: "+what+" by a panic: "+redact(fmt.Sprint(p), st.settings.Probe.APIKey))
	}
}

// idle reports whether probes are paused because no client search arrived
// within idleSeconds. Router creation counts as activity, so the first
// rounds run. Health then keeps its last value until traffic returns.
func (r *Router) idle(idleSeconds int, now time.Time) bool {
	if idleSeconds <= 0 {
		return false
	}
	last := max(r.lastSearch.Load(), r.createdAt.UnixNano())
	return now.UnixNano()-last > int64(idleSeconds)*int64(time.Second)
}

// probeRound probes the channels in order and stops at the first healthy
// one: routing uses only that channel, so the fallbacks after it are probed,
// and paid for, only while the channels before them fail.
func (r *Router) probeRound(ctx context.Context, st *routerState) {
	s := st.settings
	transport := r.transport
	if transport == nil {
		// The CPA container exports HTTP(S)_PROXY; probes target CPA itself.
		transport = &http.Transport{Proxy: nil}
	}
	client := &http.Client{Transport: transport, Timeout: time.Duration(s.Probe.TimeoutSeconds) * time.Second}
	defer client.CloseIdleConnections()
	model := s.probeModel()
	for i, ch := range s.Channels {
		if ch.ForceUnhealthy {
			continue
		}
		selected := strings.ReplaceAll(ch.Model, modelPlaceholder, model)
		err := probeChannel(ctx, client, s.Probe, ch.Name, selected)
		if ctx.Err() != nil {
			return
		}
		live := st.channels[i]
		result := &probeResult{at: time.Now()}
		var errOp *net.OpError
		switch {
		case errors.As(err, &errOp) && errOp.Op == "dial":
			// Failing to reach CPA itself, as during startup before it
			// listens, says nothing about the channel.
			result.err = "CPA unreachable, health unchanged: " + redact(err.Error(), s.Probe.APIKey)
			live.lastProbe.Store(result)
			r.log("warn", "alpha search: probe could not reach CPA, channel health unchanged: "+result.err)
		default:
			if err != nil {
				result.err = redact(err.Error(), s.Probe.APIKey)
			}
			live.lastProbe.Store(result)
			wasUnhealthy := live.unhealthy.Swap(err != nil)
			switch {
			case err != nil && !wasUnhealthy:
				r.log("warn", fmt.Sprintf("alpha search: channel %s unhealthy, probe %s failed: %s", ch.Name, selected, result.err))
			case err == nil && wasUnhealthy:
				r.log("info", fmt.Sprintf("alpha search: channel %s recovered, probe %s succeeded", ch.Name, selected))
			}
		}
		if !live.unhealthy.Load() {
			return
		}
	}
}

// probeChannel sends one short search through CPA. Success means HTTP 200
// with a JSON body carrying a non-empty output, which also rejects the HTML
// pages some upstreams return with status 200.
func probeChannel(ctx context.Context, client *http.Client, probe Probe, channelName, model string) error {
	body, err := json.Marshal(map[string]any{
		"id":    "alpha-search-model-probe-" + channelName,
		"model": model,
		"input": []map[string]any{{
			"type": "message", "role": "user",
			"content": []map[string]any{{"type": "input_text", "text": "What is the current UTC date?"}},
		}},
		"commands": map[string]any{
			"search_query":    []map[string]any{{"q": "current UTC date"}},
			"response_length": "short",
		},
		"max_output_tokens": 256,
	})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, probe.URL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+probe.APIKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", probe.UserAgent)
	req.Header.Set(ProbeHeader, "1")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("status %d: %s", resp.StatusCode, snippet(raw, probe.APIKey))
	}
	var result struct {
		Output string `json:"output"`
	}
	if err := json.Unmarshal(raw, &result); err != nil || strings.TrimSpace(result.Output) == "" {
		return fmt.Errorf("status 200 without search output: %s", snippet(raw, probe.APIKey))
	}
	return nil
}

// snippet is the start of a response body for an error message, with the
// probe key removed before truncation could split it.
func snippet(raw []byte, key string) string {
	text := redact(strings.Join(strings.Fields(string(raw)), " "), key)
	if len(text) > 200 {
		text = strings.ToValidUTF8(text[:200], "")
	}
	return text
}

// redact removes the probe key from text bound for logs or the management
// API.
func redact(text, key string) string {
	if key == "" {
		return text
	}
	return strings.ReplaceAll(text, key, "[probe key]")
}
