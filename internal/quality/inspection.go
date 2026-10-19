package quality

import (
	"fmt"
	"path"
	"slices"
	"time"
)

// inspection is the latest scheduled check of one test: when it started and
// which credentials it covered. It lives until the plugin stops.
type inspection struct {
	started time.Time
	authIDs []string
}

// InspectionRequest starts a test, as RunRequest does, on the enabled
// credentials whose email, name or group matches an include pattern (any
// credential without one) and no exclude pattern. The field names are
// candy-eval's, so its inspection script works with only the path prefix
// changed.
type InspectionRequest struct {
	RunRequest
	Include []string `json:"include"`
	Exclude []string `json:"exclude"`
}

// InspectionItem is one credential of an inspection with the records it got
// from it. Degraded is null when none of them reached a verdict.
type InspectionItem struct {
	Credential
	Results  any   `json:"results"`
	Degraded *bool `json:"degraded"`
}

// InspectionView reports whether the latest inspection of a test still runs.
type InspectionView struct {
	Running     bool             `json:"running"`
	Credentials []InspectionItem `json:"credentials"`
}

var inspectedTests = []string{TestCandy, TestFingerprint, TestModelTrace}

// Inspect starts req's test on the credentials its patterns select.
func (s *Service) Inspect(req InspectionRequest) (RunSummary, error) {
	if !slices.Contains(inspectedTests, req.Test) {
		return RunSummary{}, fmt.Errorf("%w: unknown test %q", ErrInvalid, req.Test)
	}
	credentials, err := s.selected(nil, true)
	if err != nil {
		return RunSummary{}, err
	}
	ids := []string{}
	for _, c := range credentials {
		if (len(req.Include) == 0 || matchesAny(c, req.Include)) && !matchesAny(c, req.Exclude) {
			ids = append(ids, c.ID)
		}
	}
	run := req.RunRequest
	run.AuthIDs, run.All = ids, false
	started := s.now()
	summary, err := s.Run(run)
	if err != nil {
		return RunSummary{}, err
	}
	s.mu.Lock()
	s.inspections[req.Test] = inspection{started, ids}
	s.mu.Unlock()
	return summary, nil
}

func matchesAny(c Credential, patterns []string) bool {
	for _, pattern := range patterns {
		for _, value := range []string{c.Email, c.Name, c.Group} {
			if matched, _ := path.Match(pattern, value); matched {
				return true
			}
		}
	}
	return false
}

// Inspection reports the latest inspection of test: whether it still runs,
// and each covered credential's records since it started. A credential is
// degraded when any of them shows so: a wrong candy answer, or a fingerprint
// or ModelTrace attribution to another model.
func (s *Service) Inspection(test string) (InspectionView, error) {
	if !slices.Contains(inspectedTests, test) {
		return InspectionView{}, fmt.Errorf("%w: unknown test %q", ErrInvalid, test)
	}
	credentials, _, err := s.credentials()
	if err != nil {
		return InspectionView{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	latest, ok := s.inspections[test]
	if !ok {
		return InspectionView{}, fmt.Errorf("%w: no inspection of this test has started", ErrNotFound)
	}
	view := InspectionView{Credentials: []InspectionItem{}}
	for _, c := range credentials {
		if !slices.Contains(latest.authIDs, c.ID) {
			continue
		}
		item := InspectionItem{Credential: c}
		switch test {
		case TestCandy:
			view.Running = view.Running || s.candy[c.ID] != nil
			item.Results, item.Degraded = inspected(s.hist.Results[c.ID], latest.started, func(r CandyResult) (time.Time, bool, bool) {
				return r.Time, !r.Skipped && r.Error == "", !r.OK
			})
		case TestFingerprint:
			view.Running = view.Running || s.fingerprint[c.ID] != nil
			records, degraded := inspected(s.hist.Fingerprints[c.ID], latest.started, func(r FingerprintResult) (time.Time, bool, bool) {
				status := r.Attribution.Status
				return r.Time, r.Status == "completed" && (status == "consistent" || status == "substitution" || status == "different"), status != "consistent"
			})
			item.Results, item.Degraded = summaries(records), degraded
		case TestModelTrace:
			view.Running = view.Running || s.trace[c.ID] != nil
			records, degraded := inspected(s.hist.ModelTraces[c.ID], latest.started, func(r TraceResult) (time.Time, bool, bool) {
				if r.Attribution == nil || (r.Status != "completed" && r.Status != "partial") {
					return r.Time, false, false
				}
				return r.Time, true, TraceModelID(r.Model) != TraceModelID(r.Attribution.Prediction)
			})
			item.Results, item.Degraded = summaries(records), degraded
		}
		view.Credentials = append(view.Credentials, item)
	}
	return view, nil
}

// inspected returns the records made since started and whether any shows a
// degraded model, or nil when none reaches a verdict. judge returns when a
// record was made and its verdict.
func inspected[T any](records []T, started time.Time, judge func(T) (at time.Time, concluded, degraded bool)) ([]T, *bool) {
	out, anyConcluded, anyDegraded := []T{}, false, false
	for _, r := range records {
		at, concluded, degraded := judge(r)
		if at.Before(started) {
			continue
		}
		out = append(out, r)
		anyConcluded = anyConcluded || concluded
		anyDegraded = anyDegraded || concluded && degraded
	}
	if !anyConcluded {
		return out, nil
	}
	return out, &anyDegraded
}
