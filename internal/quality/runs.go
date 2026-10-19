package quality

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"sync"
	"sync/atomic"
	"time"
)

// cancelledMessage marks a record a cancel or a hot replacement cut short.
const cancelledMessage = "已取消"

// finish ends a run goroutine: a panic becomes a log line instead of ending
// CPA, the progress entry is removed and the run's context released.
func (s *Service) finish(kind string, running func(), cancel context.CancelFunc) {
	if recovered := recover(); recovered != nil {
		s.logf("error", fmt.Sprintf("cpa-key-policy: %s test panicked: %v", kind, recovered))
	}
	s.mu.Lock()
	running()
	s.mu.Unlock()
	cancel()
	s.tasks.Done()
}

// ask sends prompt as the first turn of a new Codex thread on c, once one of
// the global slots is free. Waiting for it counts neither against the answer
// time limit nor in the duration.
func (s *Service) ask(ctx context.Context, c Credential, model, effort, prompt string) (ModelResponse, time.Duration, error) {
	if effort == "none" {
		effort = ""
	}
	if ctx.Err() != nil {
		return ModelResponse{}, 0, ctx.Err()
	}
	select {
	case s.askSlots <- struct{}{}:
	case <-ctx.Done():
		return ModelResponse{}, 0, ctx.Err()
	}
	defer func() { <-s.askSlots }()
	body, headers := CodexTurn(c.ID, model, effort, prompt)
	ctx, cancel := withModelTimeout(ctx, AnswerTimeout)
	defer cancel()
	started := time.Now()
	out, _, err := executeModel(ctx, s.host, c, model, body, headers)
	return out, time.Since(started), err
}

func errorText(err error) string {
	if errors.Is(err, context.Canceled) {
		return cancelledMessage
	}
	return truncate(err.Error(), 500)
}

// runCandy asks the candy question runs times, one after another.
func (s *Service) runCandy(ctx context.Context, c Credential, model, effort string, runs int, p *candyProgress) {
	defer s.finish(TestCandy, func() { delete(s.candy, c.ID) }, p.cancel)
	for range runs {
		if ctx.Err() != nil {
			return
		}
		r := CandyResult{Time: s.now().UTC(), Model: model, Effort: effort}
		out, elapsed, err := s.ask(ctx, c, model, effort, CandyPrompt)
		r.DurationMS = elapsed.Milliseconds()
		r.InputTokens, r.OutputTokens, r.ReasoningTokens = out.InputTokens, out.OutputTokens, out.ReasoningTokens
		if err != nil {
			r.Error = errorText(err)
		} else {
			r.Answer = truncate(out.Answer, 4000)
			r.OK = CandyCorrect(out.Answer)
		}
		s.mu.Lock()
		s.hist.Results[c.ID] = appendRecent(s.hist.Results[c.ID], r)
		p.Done++
		s.markDirtyLocked()
		s.mu.Unlock()
	}
}

// runPelican asks for the pelican animation once.
func (s *Service) runPelican(ctx context.Context, c Credential, model, effort string, p *pelicanProgress) {
	defer s.finish(TestPelican, func() { delete(s.pelican, c.ID) }, p.cancel)
	now := s.now()
	r := PelicanResult{ID: fmt.Sprint(now.UnixNano()), Time: now.UTC(), Model: model, Effort: effort}
	out, elapsed, err := s.ask(ctx, c, model, effort, PelicanPrompt)
	r.DurationMS = elapsed.Milliseconds()
	r.InputTokens, r.OutputTokens, r.ReasoningTokens = out.InputTokens, out.OutputTokens, out.ReasoningTokens
	switch html := ExtractHTML(out.Answer); {
	case err != nil:
		r.Error = errorText(err)
	case html == "":
		r.Error = "回答中没有 HTML 代码"
	case len(html) > PelicanMaxHTML:
		r.Error = fmt.Sprintf("HTML 超过 %d KB，未保存", PelicanMaxHTML>>10)
	default:
		r.HTML = html
	}
	s.mu.Lock()
	s.hist.Pelicans[c.ID] = appendRecent(s.hist.Pelicans[c.ID], r)
	s.markDirtyLocked()
	s.mu.Unlock()
}

type fingerprintSample struct {
	output     ModelResponse
	cell       string
	normalized string
	category   string
	err        string
}

// runFingerprint collects the mode's samples with p.Concurrency workers and
// compares them with the baselines.
func (s *Service) runFingerprint(ctx context.Context, c Credential, model string, mode FingerprintMode, p *fingerprintProgress) {
	started := s.now()
	var inputTokens, outputTokens, reasoningTokens int64
	r := FingerprintResult{ID: fmt.Sprint(started.UnixNano()), Time: started.UTC(), Model: model, Mode: mode.ID, Concurrency: p.Concurrency,
		Effort: "low", Status: "completed", Total: mode.Cells * mode.Samples}
	r.InputTokens, r.OutputTokens, r.ReasoningTokens = &inputTokens, &outputTokens, &reasoningTokens
	defer s.finish(TestFingerprint, func() { delete(s.fingerprint, c.ID) }, p.cancel)
	defer func() {
		if recovered := recover(); recovered != nil {
			r.Status, r.Error = "failed", fmt.Sprint(recovered)
		}
		r.DurationMS = time.Since(started).Milliseconds()
		if ctx.Err() != nil && r.Done < r.Total && r.Status != "failed" {
			r.Status = "cancelled"
		}
		s.mu.Lock()
		s.hist.Fingerprints[c.ID] = appendRecent(s.hist.Fingerprints[c.ID], r)
		s.markDirtyLocked()
		s.mu.Unlock()
	}()
	jobs := make([]FingerprintProbe, 0, r.Total)
	for _, probe := range FingerprintProbes[:mode.Cells] {
		for range mode.Samples {
			jobs = append(jobs, probe)
		}
	}
	rand.Shuffle(len(jobs), func(i, j int) { jobs[i], jobs[j] = jobs[j], jobs[i] })
	events := make(chan fingerprintSample)
	var cursor atomic.Int64
	var workers sync.WaitGroup
	for range p.Concurrency {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for ctx.Err() == nil {
				i := int(cursor.Add(1) - 1)
				if i >= len(jobs) {
					return
				}
				events <- s.fingerprintSample(ctx, c, model, jobs[i])
			}
		}()
	}
	go func() { workers.Wait(); close(events) }()
	valid := map[string][]string{}
	consecutiveErrors := 0
	for sample := range events {
		inputTokens += sample.output.InputTokens
		outputTokens += sample.output.OutputTokens
		reasoningTokens += sample.output.ReasoningTokens
		if sample.category == "cancelled" {
			continue
		}
		r.Done++
		if sample.category == "error" {
			r.Errors++
			consecutiveErrors++
			if consecutiveErrors >= 8 && r.Status != "failed" {
				r.Status, r.Error = "failed", "连续 8 次请求失败，已停止采集："+sample.err
				p.cancel()
			}
		} else {
			consecutiveErrors = 0
		}
		if sample.category == "valid" {
			r.Valid++
			valid[sample.cell] = append(valid[sample.cell], sample.normalized)
		}
		s.mu.Lock()
		p.Done, p.Valid, p.Errors = r.Done, r.Valid, r.Errors
		s.mu.Unlock()
	}
	s.mu.Lock()
	if p.Phase != "cancelling" {
		p.Phase = "comparing"
	}
	s.mu.Unlock()
	r.Attribution = AttributeFingerprint(model, valid)
}

func (s *Service) fingerprintSample(ctx context.Context, c Credential, model string, probe FingerprintProbe) (sample fingerprintSample) {
	sample.cell = probe.ID
	// The worker goroutine has no recover of its own, and a panic there would
	// end CPA: it becomes one failed sample.
	defer func() {
		if recovered := recover(); recovered != nil {
			s.logf("error", fmt.Sprintf("cpa-key-policy: %s sample panicked: %v", TestFingerprint, recovered))
			sample.category, sample.err = "error", truncate(fmt.Sprint(recovered), 500)
		}
	}()
	out, _, err := executeProbe(ctx, s.host, c, model, FingerprintRequest(model, probe), nil, s.fingerprintSlots)
	sample.output = out
	if err != nil {
		if errors.Is(err, context.Canceled) {
			sample.category = "cancelled"
			return
		}
		sample.category, sample.err = "error", truncate(err.Error(), 500)
		return
	}
	sample.normalized, sample.category = NormalizeFingerprintAnswer(out.Answer, probe)
	return
}

// runModelTrace sends the three number challenges and attributes the
// answers.
func (s *Service) runModelTrace(ctx context.Context, c Credential, model string, concurrency int, p *traceProgress) {
	started := s.now()
	done := 0
	r := TraceResult{ID: fmt.Sprint(started.UnixNano()), Time: started.UTC(), Model: model, Concurrency: concurrency, Status: "completed"}
	defer s.finish(TestModelTrace, func() { delete(s.trace, c.ID) }, p.cancel)
	defer func() {
		if ctx.Err() != nil && done < TraceTarget {
			r.Status = "cancelled"
		}
		if recovered := recover(); recovered != nil {
			r.Status, r.Error = "failed", fmt.Sprint(recovered)
		}
		r.DurationMS = time.Since(started).Milliseconds()
		r.BankRevision = TraceBankRevision
		s.mu.Lock()
		s.hist.ModelTraces[c.ID] = appendRecent(s.hist.ModelTraces[c.ID], r)
		s.markDirtyLocked()
		s.mu.Unlock()
	}()
	challenges := TraceChallenges()
	type event struct {
		index  int
		sample TraceSample
		output ModelResponse
		err    error
	}
	jobs, events := make(chan int, len(challenges)), make(chan event, len(challenges))
	for i := range challenges {
		jobs <- i
	}
	close(jobs)
	var workers sync.WaitGroup
	for range concurrency {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for i := range jobs {
				if ctx.Err() != nil {
					return
				}
				sample, output, err := s.traceSample(ctx, c, model, challenges[i])
				if sample.Attempts > 0 {
					events <- event{i, sample, output, err}
				}
			}
		}()
	}
	go func() { workers.Wait(); close(events) }()
	samples := make([]*TraceSample, len(challenges))
	valid := 0
	for e := range events {
		samples[e.index] = &e.sample
		r.InputTokens += e.output.InputTokens
		r.OutputTokens += e.output.OutputTokens
		r.ReasoningTokens += e.output.ReasoningTokens
		if !errors.Is(e.err, context.Canceled) {
			done++
		}
		if e.sample.Valid {
			valid++
		}
		s.mu.Lock()
		p.Done = done
		s.mu.Unlock()
	}
	var outputs []TraceOutput
	// Keep the challenge order even when requests complete out of order.
	for _, sample := range samples {
		if sample == nil {
			continue
		}
		r.Samples = append(r.Samples, *sample)
		outputs = append(outputs, TraceOutput{Text: sample.Text, ExpectedCount: sample.ExpectedCount})
	}
	if ctx.Err() != nil && done < TraceTarget {
		return
	}
	attribution, err := AnalyzeModelTrace(outputs)
	if err != nil {
		r.Status, r.Error = "failed", err.Error()
		if len(r.Samples) > 0 && r.Samples[len(r.Samples)-1].Error != "" {
			r.Error = r.Samples[len(r.Samples)-1].Error
		}
		return
	}
	r.Attribution = &attribution
	if valid < TraceTarget {
		r.Status = "partial"
	}
}

func (s *Service) traceSample(ctx context.Context, c Credential, model string, challenge TraceChallenge) (sample TraceSample, out ModelResponse, err error) {
	sample.TraceChallenge = challenge
	// As in fingerprintSample; the worker passes on only samples with an
	// attempt.
	defer func() {
		if recovered := recover(); recovered != nil {
			s.logf("error", fmt.Sprintf("cpa-key-policy: %s sample panicked: %v", TestModelTrace, recovered))
			err = fmt.Errorf("%v", recovered)
			sample.Error, sample.Attempts = errorText(err), max(sample.Attempts, 1)
		}
	}()
	body, headers := CodexTurn(c.ID, model, "", challenge.Prompt)
	out, sample.Attempts, err = executeProbe(ctx, s.host, c, model, body, headers, s.traceSlots)
	if err != nil {
		sample.Error = errorText(err)
		return
	}
	if len(out.Answer) > TraceMaxText {
		sample.Error = "回答过长，本次未计入"
		return
	}
	sample.Text = out.Answer
	sample.Parsed = len(TraceParseNumbers(sample.Text))
	sample.Valid = sample.Parsed >= TraceMinimumNumbers(challenge.ExpectedCount)
	if !sample.Valid {
		sample.Error = "有效数字不足"
	}
	return
}
