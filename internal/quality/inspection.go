package quality

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"cpa-key-policy/internal/policy/persist"
)

// The daily inspection asks chosen channels variants of the candy question
// at fixed times of day, each group of channels with the model it serves;
// every channel of a round gets the same variants, so that their answers
// compare. Its settings and rounds live in a file of their own: rounds do
// not push the manual test records out, and clearing those keeps the
// schedule.
//
// A group follows CPA: each round also asks every other channel the operator
// did not ignore, and CPA answers at once, without an upstream request, when
// a channel has no such model. The round then adds the channels that serve
// the model to the group and drops those that stopped serving it or left
// CPA, so the groups need no upkeep when upstreams change.

// Inspection limits.
const (
	inspectionMaxTimes    = 6
	inspectionMaxGroups   = 50
	inspectionMaxChannels = 500
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

// efforts are the reasoning efforts a question can be asked with; none
// leaves it to CPA.
var efforts = []string{"none", "minimal", "low", "medium", "high", "xhigh"}

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
// for, empty for a round the operator started. Questions are the variants
// every channel was asked, in order; rounds of 0.24.0 asked the published
// question and have none.
type inspectionRound struct {
	ID        string         `json:"id"`
	Slot      string         `json:"slot,omitempty"`
	Started   time.Time      `json:"started"`
	Finished  time.Time      `json:"finished"`
	Questions []CandyVariant `json:"questions,omitempty"`
	Items     []roundItem    `json:"items"`
}

// roundItem is one channel of a round, asked with its group's model.
// Answered counts the questions that got an answer, Failed those that did
// not; Error is the last failure. Skipped says why the channel was not asked:
// missing from CPA, disabled, cooling down, or not serving the model any
// more. Wrong holds the wrong answers a 0.24.0 file has; loading moves them
// into Misses.
type roundItem struct {
	AuthIndex string      `json:"auth_index"`
	Model     string      `json:"model"`
	Effort    string      `json:"effort"`
	Answered  int         `json:"answered"`
	Correct   int         `json:"correct"`
	Failed    int         `json:"failed"`
	Skipped   string      `json:"skipped,omitempty"`
	Error     string      `json:"error,omitempty"`
	Misses    []roundMiss `json:"misses,omitempty"`
	Wrong     []string    `json:"wrong,omitempty"`
}

// roundMiss is a wrong answer to the round's question at Question; -1 is the
// published question.
type roundMiss struct {
	Question int    `json:"question"`
	Answer   string `json:"answer"`
}

type inspectionFile struct {
	Version  int                `json:"version"`
	Settings InspectionSettings `json:"settings"`
	Rounds   []inspectionRound  `json:"rounds"`
}

func emptyInspection() inspectionFile {
	return inspectionFile{Version: inspectionFileVersion, Settings: defaultInspectionSettings(), Rounds: []inspectionRound{}}
}

// roundProgress is the round under way, asking questions.
type roundProgress struct {
	Slot      string    `json:"slot,omitempty"`
	Started   time.Time `json:"started"`
	Done      int       `json:"done"`
	Total     int       `json:"total"`
	questions []CandyVariant
	cancel    context.CancelFunc
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
	if len(out.Times) == 0 || len(out.Times) > inspectionMaxTimes {
		return out, fmt.Errorf("%w: choose 1-%d times of day", ErrInvalid, inspectionMaxTimes)
	}
	if len(in.Groups) > inspectionMaxGroups {
		return out, fmt.Errorf("%w: at most %d groups", ErrInvalid, inspectionMaxGroups)
	}
	for _, group := range in.Groups {
		group.Model, group.Effort = strings.TrimSpace(group.Model), cmp.Or(strings.TrimSpace(group.Effort), "none")
		if !ValidModelName(group.Model, true) {
			return out, fmt.Errorf("%w: choose a model for every group", ErrInvalid)
		}
		if !slices.Contains(efforts, group.Effort) {
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
		if len(group.Channels)+len(group.Ignored) > inspectionMaxChannels {
			return out, fmt.Errorf("%w: at most %d channels per group", ErrInvalid, inspectionMaxChannels)
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

// scheduledLocked reports whether the schedule has anything to run. A group
// whose channels all left still runs, to find the channels serving its model.
func (s *Service) scheduledLocked() bool {
	settings := s.inspection.Settings
	return settings.Enabled && s.inspectionErr == nil && len(settings.Groups) > 0
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

// roundJob is one channel of a round under way; its goroutine owns item and
// notServing. member says the group asked the channel before the round; any
// other channel is asked only to find out whether it serves the model now.
type roundJob struct {
	credential Credential
	item       roundItem
	member     bool
	notServing bool
}

// startRound asks every group's channels the same new variants of the candy
// question, and every other enabled channel the group does not ignore; slot
// names the schedule slot it runs for. Members missing from CPA, disabled or
// cooling down are recorded as skipped; other channels in that state are left
// alone.
func (s *Service) startRound(slot string) error {
	credentials, configErr, err := s.Credentials()
	if err != nil {
		return err
	}
	byIndex := make(map[string]Credential, len(credentials))
	indexes := []string{}
	for _, c := range credentials {
		if _, seen := byIndex[c.AuthIndex]; c.AuthIndex != "" && !seen {
			byIndex[c.AuthIndex] = c
			indexes = append(indexes, c.AuthIndex)
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
			job := roundJob{member: true, item: roundItem{AuthIndex: index, Model: group.Model, Effort: group.Effort}}
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
		for _, index := range indexes {
			c := byIndex[index]
			if c.Disabled || c.Unavailable || slices.Contains(group.Channels, index) || slices.Contains(group.Ignored, index) {
				continue
			}
			jobs = append(jobs, roundJob{credential: c, item: roundItem{AuthIndex: index, Model: group.Model, Effort: group.Effort}})
		}
	}
	if len(jobs) == 0 {
		return fmt.Errorf("%w: no channel to inspect", ErrInvalid)
	}
	started := s.now()
	rng := rand.New(rand.NewSource(started.UnixNano()))
	questions := make([]CandyVariant, settings.Runs)
	for i := range questions {
		questions[i] = NewCandyVariant(rng)
	}
	ctx, cancel := context.WithCancel(context.Background())
	p := &roundProgress{Slot: slot, Started: started.UTC(), Done: skipped, Total: len(jobs), questions: questions, cancel: cancel}
	s.round = p
	s.tasks.Add(1)
	go s.runRound(ctx, p, jobs, configErr == "")
	return nil
}

// runRound asks the channels in parallel, each its questions one after
// another, records the round once all are done and lets the groups follow
// what CPA answered. listed says the credential list was complete, so a
// member missing from it has left CPA. A cancelled round records nothing.
func (s *Service) runRound(ctx context.Context, p *roundProgress, jobs []roundJob, listed bool) {
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
			s.inspect(ctx, job, p.questions)
			s.mu.Lock()
			p.Done++
			s.mu.Unlock()
		}(&jobs[i])
	}
	wg.Wait()
	if ctx.Err() != nil {
		return
	}
	round := inspectionRound{ID: strconv.FormatInt(p.Started.UnixNano(), 10), Slot: p.Slot, Started: p.Started, Finished: s.now().UTC(),
		Questions: p.questions, Items: []roundItem{}}
	for _, job := range jobs {
		// A channel outside the group that lacks the model was only looked
		// for.
		if job.member || !job.notServing {
			round.Items = append(round.Items, job.item)
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.inspection.Rounds = append(s.inspection.Rounds, round)
	s.followCPALocked(jobs, listed)
	_ = s.saveInspectionLocked()
}

// followCPALocked updates each group's channels from a finished round:
// channels that answered join, members that no longer serve the model or
// left CPA leave. It merges into the settings as they are now, which the
// operator may have saved during the round, so an ignored channel never
// joins. When no channel of a group served the model, CPA most likely had
// nothing registered for a moment (a full reload), so its members stay;
// with an incomplete credential list, so do missing ones. Views share the
// settings' slices, so changed groups are new slices.
func (s *Service) followCPALocked(jobs []roundJob, listed bool) {
	groups := slices.Clone(s.inspection.Settings.Groups)
	changed := false
	for i, group := range groups {
		var joined, left, stopped []string
		serving := false
		for _, job := range jobs {
			index := job.item.AuthIndex
			if job.item.Model != group.Model || job.item.Effort != group.Effort {
				continue
			}
			switch {
			case job.notServing:
				if job.member {
					stopped = append(stopped, index)
				}
			case job.item.Skipped == "missing":
				if listed {
					left = append(left, index)
				}
			case job.item.Skipped == "":
				serving = true
				if !slices.Contains(group.Channels, index) && !slices.Contains(group.Ignored, index) &&
					len(group.Channels)+len(group.Ignored)+len(joined) < inspectionMaxChannels {
					joined = append(joined, index)
				}
			}
		}
		if serving {
			left = append(left, stopped...)
		}
		if len(joined)+len(left) == 0 {
			continue
		}
		channels := slices.DeleteFunc(slices.Clone(group.Channels), func(index string) bool { return slices.Contains(left, index) })
		groups[i].Channels = append(channels, joined...)
		changed = true
		s.logf("info", fmt.Sprintf("cpa-key-policy: inspection group %s (%s) follows CPA: joined %v, left %v", group.Model, group.Effort, joined, left))
	}
	if changed {
		s.inspection.Settings.Groups = groups
	}
}

// NotServing reports CPA's answer to a request pinned to a credential that
// has no such model: its scheduler finds no candidate (auth_not_found),
// unlike a credential cooling down (auth_unavailable). CPA answers every
// failed callback with the code host_call_failed, so only the message, which
// starts with the scheduler's own code, tells them apart.
func NotServing(err error) bool {
	var hostErr *HostError
	return errors.As(err, &hostErr) && strings.HasPrefix(hostErr.Message, "auth_not_found:")
}

// inspect asks one channel the round's questions one after another. A
// channel CPA has no such model for is asked once.
func (s *Service) inspect(ctx context.Context, job *roundJob, questions []CandyVariant) {
	item := &job.item
	for i, question := range questions {
		if ctx.Err() != nil {
			return
		}
		out, _, err := s.ask(ctx, job.credential, item.Model, item.Effort, question.Prompt())
		switch {
		case i == 0 && NotServing(err):
			job.notServing, item.Skipped = true, "not_serving"
			return
		case err != nil:
			item.Failed++
			item.Error = errorText(err)
		case candyVariantCorrect(out.Answer, question.Answer):
			item.Answered++
			item.Correct++
		default:
			item.Answered++
			item.Misses = append(item.Misses, roundMiss{Question: i, Answer: truncate(out.Answer, inspectionAnswerLimit)})
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

// InspectionDay is one local date's results, by channel and by group.
type InspectionDay struct {
	Date   string           `json:"date"`
	Cells  []InspectionCell `json:"cells"`
	Groups []InspectionBest `json:"groups"`
}

// InspectionBest is a group's best channel over a period: Channels counts the
// channels that answered, BestCorrect and BestAnswered are the best one's
// answers. Low says two or more answered and even the best is not judged ok:
// the model or the effort cannot do the question, not one channel.
type InspectionBest struct {
	Model        string `json:"model"`
	Effort       string `json:"effort"`
	Channels     int    `json:"channels"`
	BestCorrect  int    `json:"best_correct"`
	BestAnswered int    `json:"best_answered"`
	Low          bool   `json:"low"`
}

type groupKey struct{ model, effort string }

// bestOf adds up a period's channels into their groups' best channels.
func bestOf(channels map[cellKey]*InspectionCell) map[groupKey]*InspectionBest {
	groups := map[groupKey]*InspectionBest{}
	for key, cell := range channels {
		if cell.Answered == 0 {
			continue
		}
		g := groups[groupKey{key.model, key.effort}]
		if g == nil {
			g = &InspectionBest{Model: key.model, Effort: key.effort}
			groups[groupKey{key.model, key.effort}] = g
		}
		g.Channels++
		// A higher share of right answers wins; on a tie, more answers.
		if better := cell.Correct*g.BestAnswered - g.BestCorrect*cell.Answered; g.BestAnswered == 0 || better > 0 || better == 0 && cell.Answered > g.BestAnswered {
			g.BestCorrect, g.BestAnswered = cell.Correct, cell.Answered
		}
	}
	for _, g := range groups {
		g.Low = g.Channels >= 2 && inspectionVerdict(g.BestAnswered, g.BestCorrect, 0) != "ok"
	}
	return groups
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
		view.Days[i] = InspectionDay{Date: date, Cells: []InspectionCell{}, Groups: []InspectionBest{}}
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
				Answered: item.Answered, Correct: item.Correct, Failed: item.Failed, Skipped: item.Skipped, Error: item.Error, WrongAnswers: len(item.Misses)})
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
		for _, g := range bestOf(byKey) {
			view.Days[i].Groups = append(view.Days[i].Groups, *g)
		}
		slices.SortFunc(view.Days[i].Groups, func(a, b InspectionBest) int {
			return cmp.Or(strings.Compare(a.Model, b.Model), strings.Compare(a.Effort, b.Effort))
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

// InspectionAnswers is one channel's share of a round with its wrong answers,
// each beside the question it answered, while they are kept.
type InspectionAnswers struct {
	Answered int              `json:"answered"`
	Correct  int              `json:"correct"`
	Failed   int              `json:"failed"`
	Error    string           `json:"error,omitempty"`
	Misses   []InspectionMiss `json:"misses"`
}

// InspectionMiss is a wrong answer and the question with its answer.
// Question is the question's place in the round from 0; -1 is the published
// question a 0.24.0 round asked.
type InspectionMiss struct {
	Question int    `json:"question"`
	Prompt   string `json:"prompt"`
	Expected int    `json:"expected"`
	Answer   string `json:"answer"`
}

// InspectionAnswers returns one channel's share of a round.
func (s *Service) InspectionAnswers(round, authIndex, model, effort string) (InspectionAnswers, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range s.inspection.Rounds {
		if r.ID != round {
			continue
		}
		for _, item := range r.Items {
			if item.AuthIndex != authIndex || item.Model != model || item.Effort != effort {
				continue
			}
			out := InspectionAnswers{Answered: item.Answered, Correct: item.Correct, Failed: item.Failed, Error: item.Error, Misses: []InspectionMiss{}}
			for _, miss := range item.Misses {
				question := InspectionMiss{Question: -1, Prompt: CandyPrompt, Expected: 21, Answer: miss.Answer}
				if miss.Question >= 0 && miss.Question < len(r.Questions) {
					question.Question, question.Prompt, question.Expected = miss.Question, r.Questions[miss.Question].Prompt(), r.Questions[miss.Question].Answer
				}
				out.Misses = append(out.Misses, question)
			}
			return out, nil
		}
	}
	return InspectionAnswers{}, fmt.Errorf("%w: the round may have expired", ErrNotFound)
}

// InspectionAttention is a channel the rounds of the last 24 hours judge
// degraded or suspect, or a group judged low (no AuthIndex; Answered and
// Correct are its best channel's).
type InspectionAttention struct {
	AuthIndex string `json:"auth_index"`
	Model     string `json:"model"`
	Effort    string `json:"effort"`
	Verdict   string `json:"verdict"`
	Answered  int    `json:"answered"`
	Correct   int    `json:"correct"`
}

// Attention lists what the last 24 hours of rounds point at: channels
// judged degraded, groups judged low, then channels judged suspect. A low
// group stands in for its channels, which share the same cause.
func (s *Service) Attention() []InspectionAttention {
	s.mu.Lock()
	defer s.mu.Unlock()
	since := s.now().Add(-24 * time.Hour)
	sums := map[cellKey]*InspectionCell{}
	for _, round := range s.inspection.Rounds {
		if round.Started.Before(since) {
			continue
		}
		for _, item := range round.Items {
			key := cellKey{item.AuthIndex, item.Model, item.Effort}
			if sums[key] == nil {
				sums[key] = &InspectionCell{}
			}
			sums[key].Answered += item.Answered
			sums[key].Correct += item.Correct
		}
	}
	groups := bestOf(sums)
	out := []InspectionAttention{}
	for _, g := range groups {
		if g.Low {
			out = append(out, InspectionAttention{Model: g.Model, Effort: g.Effort, Verdict: "group_low", Answered: g.BestAnswered, Correct: g.BestCorrect})
		}
	}
	for key, sum := range sums {
		if g := groups[groupKey{key.model, key.effort}]; g != nil && g.Low {
			continue
		}
		if verdict := inspectionVerdict(sum.Answered, sum.Correct, 0); verdict == "degraded" || verdict == "suspect" {
			out = append(out, InspectionAttention{AuthIndex: key.authIndex, Model: key.model, Effort: key.effort, Verdict: verdict, Answered: sum.Answered, Correct: sum.Correct})
		}
	}
	severity := map[string]int{"degraded": 0, "group_low": 1, "suspect": 2}
	slices.SortFunc(out, func(a, b InspectionAttention) int {
		return cmp.Or(cmp.Compare(severity[a.Verdict], severity[b.Verdict]), strings.Compare(a.Model, b.Model), strings.Compare(a.AuthIndex, b.AuthIndex))
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
	for i := range file.Rounds {
		for j := range file.Rounds[i].Items {
			item := &file.Rounds[i].Items[j]
			for _, answer := range item.Wrong {
				item.Misses = append(item.Misses, roundMiss{Question: -1, Answer: answer})
			}
			item.Wrong = nil
		}
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
				rounds[i].Items[j].Misses = nil
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
