package quality

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"cpa-key-policy/internal/policy/persist"
)

// The daily inspection asks chosen channels the candy question at fixed
// times of day, each group of channels with the model it serves. Its
// settings and rounds live in a file of their own: rounds do not push the
// manual test records out, and clearing those keeps the schedule.

// Inspection limits.
const (
	InspectionMaxTimes    = 6
	InspectionMaxGroups   = 50
	InspectionMaxChannels = 500
)

const (
	inspectionFileVersion = 1
	// Rounds are kept as long as the ledger keeps usage; their wrong
	// answers, the bulk of the file, only for two weeks.
	inspectionRetention       = 90 * 24 * time.Hour
	inspectionAnswerRetention = 14 * 24 * time.Hour
	inspectionAnswerLimit     = 1000
	clockLayout               = "15:04"
	// slotLayout names a scheduled round by its local date and time.
	slotLayout = "2006-01-02 15:04"
	dateLayout = "2006-01-02"
)

// Efforts are the reasoning efforts a question can be asked with; none
// leaves it to CPA.
var Efforts = []string{"none", "minimal", "low", "medium", "high", "xhigh"}

// InspectionSettings is the schedule: at each of Times (HH:MM in the
// plugin's timezone) every group's channels are asked Runs candy questions.
type InspectionSettings struct {
	Enabled bool              `json:"enabled"`
	Times   []string          `json:"times"`
	Runs    int               `json:"runs"`
	Groups  []InspectionGroup `json:"groups"`
}

// InspectionGroup is the channels, by auth_index, asked with one model and
// effort. Ignored holds the channels serving the model the operator left
// out, so that the page offers only channels new since.
type InspectionGroup struct {
	Model    string   `json:"model"`
	Effort   string   `json:"effort"`
	Channels []string `json:"channels"`
	Ignored  []string `json:"ignored"`
}

func defaultInspectionSettings() InspectionSettings {
	return InspectionSettings{Times: []string{"08:00", "14:00", "20:00"}, Runs: 3, Groups: []InspectionGroup{}}
}

// inspectionRound is one finished round. Slot names the schedule slot it ran
// for, empty for a round the operator started.
type inspectionRound struct {
	ID       string      `json:"id"`
	Slot     string      `json:"slot,omitempty"`
	Started  time.Time   `json:"started"`
	Finished time.Time   `json:"finished"`
	Items    []roundItem `json:"items"`
}

// roundItem is one channel of a round, asked with its group's model.
// Answered counts the questions that got an answer, Failed those that did
// not; Error is the last failure. Skipped says why the channel was not asked:
// missing from CPA, disabled or cooling down.
type roundItem struct {
	AuthIndex string   `json:"auth_index"`
	Model     string   `json:"model"`
	Effort    string   `json:"effort"`
	Answered  int      `json:"answered"`
	Correct   int      `json:"correct"`
	Failed    int      `json:"failed"`
	Skipped   string   `json:"skipped,omitempty"`
	Error     string   `json:"error,omitempty"`
	Wrong     []string `json:"wrong,omitempty"`
}

type inspectionFile struct {
	Version  int                `json:"version"`
	Settings InspectionSettings `json:"settings"`
	Rounds   []inspectionRound  `json:"rounds"`
}

func emptyInspection() inspectionFile {
	return inspectionFile{Version: inspectionFileVersion, Settings: defaultInspectionSettings(), Rounds: []inspectionRound{}}
}

// roundProgress is the round under way.
type roundProgress struct {
	Slot    string    `json:"slot,omitempty"`
	Started time.Time `json:"started"`
	Done    int       `json:"done"`
	Total   int       `json:"total"`
	cancel  context.CancelFunc
}

// normalizeInspection validates settings from the operator or the file:
// times are distinct HH:MM, sorted; each model and effort has one group; an
// auth_index is 16 hexadecimal characters, in a group or ignored by it.
func normalizeInspection(in InspectionSettings) (InspectionSettings, error) {
	out := InspectionSettings{Enabled: in.Enabled, Runs: in.Runs, Times: []string{}, Groups: []InspectionGroup{}}
	if out.Runs < 1 || out.Runs > CandyMaxRuns {
		return out, fmt.Errorf("%w: questions per channel must be 1-%d", ErrInvalid, CandyMaxRuns)
	}
	for _, value := range in.Times {
		at, err := time.Parse(clockLayout, strings.TrimSpace(value))
		if err != nil {
			return out, fmt.Errorf("%w: time %q is not HH:MM", ErrInvalid, value)
		}
		if text := at.Format(clockLayout); !slices.Contains(out.Times, text) {
			out.Times = append(out.Times, text)
		}
	}
	slices.Sort(out.Times)
	if len(out.Times) == 0 || len(out.Times) > InspectionMaxTimes {
		return out, fmt.Errorf("%w: choose 1-%d times of day", ErrInvalid, InspectionMaxTimes)
	}
	if len(in.Groups) > InspectionMaxGroups {
		return out, fmt.Errorf("%w: at most %d groups", ErrInvalid, InspectionMaxGroups)
	}
	for _, group := range in.Groups {
		group.Model, group.Effort = strings.TrimSpace(group.Model), cmp.Or(strings.TrimSpace(group.Effort), "none")
		if !ValidModelName(group.Model, true) {
			return out, fmt.Errorf("%w: choose a model for every group", ErrInvalid)
		}
		if !slices.Contains(Efforts, group.Effort) {
			return out, fmt.Errorf("%w: unknown reasoning effort %q", ErrInvalid, group.Effort)
		}
		if slices.ContainsFunc(out.Groups, func(g InspectionGroup) bool { return g.Model == group.Model && g.Effort == group.Effort }) {
			return out, fmt.Errorf("%w: %s at effort %s has two groups", ErrInvalid, group.Model, group.Effort)
		}
		var err error
		if group.Channels, err = authIndexes(group.Channels); err != nil {
			return out, err
		}
		if group.Ignored, err = authIndexes(group.Ignored); err != nil {
			return out, err
		}
		group.Ignored = slices.DeleteFunc(group.Ignored, func(index string) bool { return slices.Contains(group.Channels, index) })
		if len(group.Channels)+len(group.Ignored) > InspectionMaxChannels {
			return out, fmt.Errorf("%w: at most %d channels per group", ErrInvalid, InspectionMaxChannels)
		}
		out.Groups = append(out.Groups, group)
	}
	return out, nil
}

// authIndexes lowercases and dedupes channel auth_indexes.
func authIndexes(values []string) ([]string, error) {
	out := []string{}
	for _, value := range values {
		value = strings.ToLower(strings.TrimSpace(value))
		if len(value) != 16 || strings.Trim(value, "0123456789abcdef") != "" {
			return nil, fmt.Errorf("%w: %q is not an auth_index", ErrInvalid, value)
		}
		if !slices.Contains(out, value) {
			out = append(out, value)
		}
	}
	return out, nil
}

// dueSlot returns the latest of today's times that has come by now, unless a
// round already ran for it: a schedule missed while no instance ran catches
// up once, never for an earlier slot or day. times are sorted HH:MM.
func dueSlot(now time.Time, loc *time.Location, times []string, done func(slot string) bool) string {
	local := now.In(loc)
	for i := len(times) - 1; i >= 0; i-- {
		at := clockOn(local, times[i])
		if at.After(local) {
			continue
		}
		if slot := at.Format(slotLayout); !done(slot) {
			return slot
		}
		return ""
	}
	return ""
}

// clockOn is the HH:MM time of day's date, in day's location.
func clockOn(day time.Time, clock string) time.Time {
	at, _ := time.Parse(clockLayout, clock)
	y, m, d := day.Date()
	return time.Date(y, m, d, at.Hour(), at.Minute(), 0, 0, day.Location())
}

func (s *Service) slotDoneLocked(slot string) bool {
	return slices.ContainsFunc(s.inspection.Rounds, func(r inspectionRound) bool { return r.Slot == slot })
}

// scheduledLocked reports whether the schedule has anything to run.
func (s *Service) scheduledLocked() bool {
	settings := s.inspection.Settings
	return settings.Enabled && s.inspectionErr == nil &&
		slices.ContainsFunc(settings.Groups, func(g InspectionGroup) bool { return len(g.Channels) > 0 })
}

// Tick starts the round a schedule slot is due for. The plugin calls it
// every minute; it does nothing while another instance owns the files or a
// round is under way. A round cut short by a hot replacement recorded
// nothing, so the instance taking over runs its slot again.
func (s *Service) Tick() {
	s.mu.Lock()
	due := ""
	if !s.stopped && s.round == nil && s.scheduledLocked() && s.canWrite() {
		due = dueSlot(s.now(), s.location, s.inspection.Settings.Times, s.slotDoneLocked)
	}
	s.mu.Unlock()
	if due == "" {
		return
	}
	err := s.startRound(due)
	s.mu.Lock()
	defer s.mu.Unlock()
	// A failure repeats every minute until it clears; it is logged once.
	message := ""
	if err != nil && !errors.Is(err, ErrUnavailable) && !errors.Is(err, ErrBusy) {
		message = err.Error()
	}
	if message != "" && message != s.tickError {
		s.logf("warn", "cpa-key-policy: the scheduled inspection did not start: "+message)
	}
	s.tickError = message
}

// StartInspection starts a round now, outside the schedule.
func (s *Service) StartInspection() error {
	return s.startRound("")
}

// roundJob is one channel of a round under way; its goroutine owns item.
type roundJob struct {
	credential Credential
	item       roundItem
}

// startRound asks every group's channels the candy question; slot names the
// schedule slot it runs for. Channels missing from CPA, disabled or cooling
// down are recorded as skipped.
func (s *Service) startRound(slot string) error {
	credentials, _, err := s.credentials()
	if err != nil {
		return err
	}
	byIndex := make(map[string]Credential, len(credentials))
	for _, c := range credentials {
		if _, seen := byIndex[c.AuthIndex]; c.AuthIndex != "" && !seen {
			byIndex[c.AuthIndex] = c
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case s.stopped || !s.canWrite():
		return ErrUnavailable
	case s.inspectionErr != nil:
		return fmt.Errorf("%w: %v", ErrPersist, s.inspectionErr)
	case s.round != nil:
		return ErrBusy
	}
	settings := s.inspection.Settings
	jobs, skipped := []roundJob{}, 0
	for _, group := range settings.Groups {
		for _, index := range group.Channels {
			job := roundJob{item: roundItem{AuthIndex: index, Model: group.Model, Effort: group.Effort}}
			c, ok := byIndex[index]
			switch {
			case !ok:
				job.item.Skipped = "missing"
			case c.Disabled:
				job.item.Skipped = "disabled"
			case c.Unavailable:
				job.item.Skipped = "unavailable"
			default:
				job.credential = c
			}
			if job.item.Skipped != "" {
				skipped++
			}
			jobs = append(jobs, job)
		}
	}
	if len(jobs) == 0 {
		return fmt.Errorf("%w: no channel is chosen for the inspection", ErrInvalid)
	}
	ctx, cancel := context.WithCancel(context.Background())
	p := &roundProgress{Slot: slot, Started: s.now().UTC(), Done: skipped, Total: len(jobs), cancel: cancel}
	s.round = p
	s.tasks.Add(1)
	go s.runRound(ctx, p, settings.Runs, jobs)
	return nil
}

// runRound asks the channels in parallel, each its questions one after
// another, and records the round once all are done. A cancelled round
// records nothing.
func (s *Service) runRound(ctx context.Context, p *roundProgress, runs int, jobs []roundJob) {
	defer s.finish("inspection", func() { s.round = nil }, p.cancel)
	var wg sync.WaitGroup
	for i := range jobs {
		if jobs[i].item.Skipped != "" {
			continue
		}
		wg.Add(1)
		go func(job *roundJob) {
			defer wg.Done()
			defer func() {
				if recovered := recover(); recovered != nil {
					s.logf("error", fmt.Sprintf("cpa-key-policy: inspection of %s panicked: %v", job.item.AuthIndex, recovered))
				}
			}()
			s.inspect(ctx, job, runs)
			s.mu.Lock()
			p.Done++
			s.mu.Unlock()
		}(&jobs[i])
	}
	wg.Wait()
	if ctx.Err() != nil {
		return
	}
	round := inspectionRound{ID: strconv.FormatInt(p.Started.UnixNano(), 10), Slot: p.Slot, Started: p.Started, Finished: s.now().UTC(), Items: make([]roundItem, len(jobs))}
	for i, job := range jobs {
		round.Items[i] = job.item
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.inspection.Rounds = append(s.inspection.Rounds, round)
	_ = s.saveInspectionLocked()
}

// inspect asks one channel runs questions.
func (s *Service) inspect(ctx context.Context, job *roundJob, runs int) {
	item := &job.item
	for range runs {
		if ctx.Err() != nil {
			return
		}
		out, _, err := s.ask(ctx, job.credential, item.Model, item.Effort, CandyPrompt)
		switch {
		case err != nil:
			item.Failed++
			item.Error = errorText(err)
		case CandyCorrect(out.Answer):
			item.Answered++
			item.Correct++
		default:
			item.Answered++
			item.Wrong = append(item.Wrong, truncate(out.Answer, inspectionAnswerLimit))
		}
	}
}

// inspectionVerdict judges a channel by its answers: at least two thirds
// right is ok, at least one third suspect, fewer degraded. Without an answer
// it failed, or was skipped.
func inspectionVerdict(answered, correct, failed int) string {
	switch {
	case answered == 0 && failed > 0:
		return "failed"
	case answered == 0:
		return "skipped"
	case 3*correct >= 2*answered:
		return "ok"
	case 3*correct >= answered:
		return "suspect"
	default:
		return "degraded"
	}
}

// InspectionView is the inspection page: the settings, the round under way,
// when the schedule runs next (zero when it is off) and each day's results
// in the plugin's timezone, newest first.
type InspectionView struct {
	Settings     InspectionSettings `json:"settings"`
	Timezone     string             `json:"timezone"`
	Running      *roundProgress     `json:"running,omitempty"`
	NextAt       time.Time          `json:"next_at,omitzero"`
	StorageError string             `json:"storage_error,omitempty"`
	Days         []InspectionDay    `json:"days"`
}

// InspectionDay is one local date's results.
type InspectionDay struct {
	Date  string           `json:"date"`
	Cells []InspectionCell `json:"cells"`
}

// InspectionCell is one channel asked with one model and effort on a day:
// its rounds' answers added up and judged.
type InspectionCell struct {
	AuthIndex string          `json:"auth_index"`
	Model     string          `json:"model"`
	Effort    string          `json:"effort"`
	Answered  int             `json:"answered"`
	Correct   int             `json:"correct"`
	Failed    int             `json:"failed"`
	Verdict   string          `json:"verdict"`
	Rounds    []InspectionRun `json:"rounds"`
}

// InspectionRun is a cell's share of one round; WrongAnswers counts the
// answers GET /quality/inspection/answers returns while they are kept.
type InspectionRun struct {
	Round        string    `json:"round"`
	Started      time.Time `json:"started"`
	Scheduled    bool      `json:"scheduled"`
	Answered     int       `json:"answered"`
	Correct      int       `json:"correct"`
	Failed       int       `json:"failed"`
	Skipped      string    `json:"skipped,omitempty"`
	Error        string    `json:"error,omitempty"`
	WrongAnswers int       `json:"wrong_answers"`
}

type cellKey struct{ authIndex, model, effort string }

// Inspection reports the schedule and the results of the last days days,
// today included.
func (s *Service) Inspection(days int) InspectionView {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now().In(s.location)
	view := InspectionView{Settings: s.inspection.Settings, Timezone: s.location.String(), StorageError: s.inspectionStorageError, Days: make([]InspectionDay, days)}
	if s.inspectionErr != nil {
		view.StorageError = s.inspectionErr.Error()
	}
	if s.round != nil {
		view.Running = copyProgress(s.round)
	}
	view.NextAt = s.nextRunLocked(now)
	index := map[string]int{}
	for i := range view.Days {
		date := now.AddDate(0, 0, -i).Format(dateLayout)
		view.Days[i] = InspectionDay{Date: date, Cells: []InspectionCell{}}
		index[date] = i
	}
	cells := make([]map[cellKey]*InspectionCell, days)
	for _, round := range s.inspection.Rounds {
		day, ok := index[round.Started.In(s.location).Format(dateLayout)]
		if !ok {
			continue
		}
		if cells[day] == nil {
			cells[day] = map[cellKey]*InspectionCell{}
		}
		for _, item := range round.Items {
			key := cellKey{item.AuthIndex, item.Model, item.Effort}
			cell := cells[day][key]
			if cell == nil {
				cell = &InspectionCell{AuthIndex: item.AuthIndex, Model: item.Model, Effort: item.Effort, Rounds: []InspectionRun{}}
				cells[day][key] = cell
			}
			cell.Answered += item.Answered
			cell.Correct += item.Correct
			cell.Failed += item.Failed
			cell.Rounds = append(cell.Rounds, InspectionRun{Round: round.ID, Started: round.Started, Scheduled: round.Slot != "",
				Answered: item.Answered, Correct: item.Correct, Failed: item.Failed, Skipped: item.Skipped, Error: item.Error, WrongAnswers: len(item.Wrong)})
		}
	}
	for i, byKey := range cells {
		for _, cell := range byKey {
			cell.Verdict = inspectionVerdict(cell.Answered, cell.Correct, cell.Failed)
			view.Days[i].Cells = append(view.Days[i].Cells, *cell)
		}
		slices.SortFunc(view.Days[i].Cells, func(a, b InspectionCell) int {
			return cmp.Or(strings.Compare(a.Model, b.Model), strings.Compare(a.Effort, b.Effort), strings.Compare(a.AuthIndex, b.AuthIndex))
		})
	}
	return view
}

// nextRunLocked is when the schedule starts its next round: now while a slot
// is due or under way, zero when the schedule is off.
func (s *Service) nextRunLocked(now time.Time) time.Time {
	if !s.scheduledLocked() {
		return time.Time{}
	}
	times := s.inspection.Settings.Times
	if dueSlot(now, s.location, times, s.slotDoneLocked) != "" {
		return now
	}
	for day := range 2 {
		for _, clock := range times {
			if at := clockOn(now.AddDate(0, 0, day), clock); at.After(now) {
				return at
			}
		}
	}
	return time.Time{}
}

// InspectionAnswers returns one channel's item of a round, with the wrong
// answers while they are kept.
func (s *Service) InspectionAnswers(round, authIndex, model, effort string) (any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range s.inspection.Rounds {
		if r.ID != round {
			continue
		}
		for _, item := range r.Items {
			if item.AuthIndex == authIndex && item.Model == model && item.Effort == effort {
				return item, nil
			}
		}
	}
	return nil, fmt.Errorf("%w: the round may have expired", ErrNotFound)
}

// InspectionAttention is a channel the rounds of the last 24 hours judge
// degraded or suspect.
type InspectionAttention struct {
	AuthIndex string `json:"auth_index"`
	Model     string `json:"model"`
	Effort    string `json:"effort"`
	Verdict   string `json:"verdict"`
	Answered  int    `json:"answered"`
	Correct   int    `json:"correct"`
}

// Attention lists the channels the last 24 hours of rounds judge degraded,
// then those they judge suspect.
func (s *Service) Attention() []InspectionAttention {
	s.mu.Lock()
	defer s.mu.Unlock()
	since := s.now().Add(-24 * time.Hour)
	sums := map[cellKey]*InspectionAttention{}
	for _, round := range s.inspection.Rounds {
		if round.Started.Before(since) {
			continue
		}
		for _, item := range round.Items {
			key := cellKey{item.AuthIndex, item.Model, item.Effort}
			if sums[key] == nil {
				sums[key] = &InspectionAttention{AuthIndex: item.AuthIndex, Model: item.Model, Effort: item.Effort}
			}
			sums[key].Answered += item.Answered
			sums[key].Correct += item.Correct
		}
	}
	out := []InspectionAttention{}
	for _, sum := range sums {
		if sum.Verdict = inspectionVerdict(sum.Answered, sum.Correct, 0); sum.Verdict == "degraded" || sum.Verdict == "suspect" {
			out = append(out, *sum)
		}
	}
	slices.SortFunc(out, func(a, b InspectionAttention) int {
		return cmp.Or(strings.Compare(a.Verdict, b.Verdict), strings.Compare(a.Model, b.Model), strings.Compare(a.AuthIndex, b.AuthIndex))
	})
	return out
}

// SetInspection replaces the settings and returns the ones replaced. A file
// that failed to load is set aside first, so the schedule resumes.
func (s *Service) SetInspection(next InspectionSettings) (previous, stored InspectionSettings, err error) {
	if stored, err = normalizeInspection(next); err != nil {
		return previous, stored, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.canWrite() {
		return previous, stored, ErrUnavailable
	}
	if s.inspectionErr != nil {
		aside := s.inspectionPath + ".corrupt-" + s.now().UTC().Format("20060102T150405Z")
		if err := os.Rename(s.inspectionPath, aside); err != nil && !errors.Is(err, os.ErrNotExist) {
			return previous, stored, fmt.Errorf("%w: cannot set the unreadable file aside: %v", ErrPersist, err)
		}
		s.logf("warn", "cpa-key-policy: inspection: moved the unreadable "+s.inspectionPath+" to "+aside)
		s.inspectionErr, s.inspection = nil, emptyInspection()
	}
	previous = s.inspection.Settings
	s.inspection.Settings = stored
	if err := s.saveInspectionLocked(); err != nil {
		s.inspection.Settings = previous
		return previous, stored, err
	}
	return previous, stored, nil
}

// openInspection loads the inspection file; a missing file is the default
// schedule, which is off. A file that cannot be read pauses the schedule and
// is kept until the settings are saved.
func (s *Service) openInspection() {
	s.inspection = emptyInspection()
	if s.inspectionPath == "" {
		return
	}
	data, err := os.ReadFile(s.inspectionPath)
	if errors.Is(err, os.ErrNotExist) {
		return
	}
	var file inspectionFile
	if err == nil {
		err = json.Unmarshal(data, &file)
	}
	if err == nil && file.Version != inspectionFileVersion {
		err = fmt.Errorf("unsupported version %d", file.Version)
	}
	if err == nil {
		file.Settings, err = normalizeInspection(file.Settings)
	}
	if err != nil {
		s.inspectionErr = fmt.Errorf("cannot read %s; the schedule is paused until the inspection settings are saved, which sets the file aside: %w", s.inspectionPath, err)
		s.logf("warn", "cpa-key-policy: inspection: "+s.inspectionErr.Error())
		return
	}
	if file.Rounds == nil {
		file.Rounds = []inspectionRound{}
	}
	s.inspection = file
	if s.canWrite() {
		if err := persist.CleanupStaleTemps(s.inspectionPath, time.Now()); err != nil {
			s.logf("warn", "cpa-key-policy: inspection: "+err.Error())
		}
	}
}

// saveInspectionLocked drops what the file no longer keeps and writes it
// whole. Nothing is written while another instance owns the data files.
func (s *Service) saveInspectionLocked() error {
	now := s.now()
	rounds := s.inspection.Rounds
	kept := slices.IndexFunc(rounds, func(r inspectionRound) bool { return !r.Started.Before(now.Add(-inspectionRetention)) })
	if kept < 0 {
		kept = len(rounds)
	}
	rounds = rounds[kept:]
	for i := range rounds {
		if rounds[i].Started.Before(now.Add(-inspectionAnswerRetention)) {
			for j := range rounds[i].Items {
				rounds[i].Items[j].Wrong = nil
			}
		}
	}
	s.inspection.Rounds = rounds
	if s.inspectionPath == "" || !s.canWrite() {
		return nil
	}
	var data bytes.Buffer
	encoder := json.NewEncoder(&data)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(s.inspection); err != nil {
		return fmt.Errorf("%w: %v", ErrPersist, err)
	}
	if err := persist.AtomicWrite(s.inspectionPath, data.Bytes()); err != nil {
		s.inspectionStorageError = "inspection not saved: " + err.Error()
		return fmt.Errorf("%w: write %s: %v", ErrPersist, s.inspectionPath, err)
	}
	s.inspectionStorageError = ""
	return nil
}
