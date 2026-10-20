// Package channel keeps what the plugin knows about each upstream channel:
// operator metadata and statistics built from CPA usage records.
//
// A channel is one CPA credential, identified by its auth_index: 16
// lowercase hex characters, the first 8 bytes of sha256 over
// "<kind>-api-key:" + base_url + "+" + api_key (kind codex, claude, gemini,
// interactions, xai or meta) or "openai-compatibility:" + base_url + "+" +
// api_key. It does not change when the credential's proxy, prefix or headers
// change. CPA's own credential lists (GET /v0/management/<kind>-api-key and
// /auth-files) carry the same value, so the management UI joins them with
// this package's data by auth_index.
//
// Statistics come only from usage records: CPA emits one per upstream
// attempt, including failed attempts that were retried on another
// credential, so they measure upstream health without any probe request.
package channel

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"cpa-key-policy/internal/policy/persist"
)

// Errors the management routes tell apart. Validation failures wrap
// ErrInvalid and metadata write failures wrap ErrPersist, each with the
// detail appended.
var (
	// ErrInvalid rejects a malformed auth_index or metadata outside its
	// limits.
	ErrInvalid = errors.New("invalid channel request")
	// ErrReadOnly rejects a metadata write while CanWrite reports that
	// another plugin instance owns the data files.
	ErrReadOnly = errors.New("channel metadata is read-only while another plugin instance owns the data files")
	// ErrPersist wraps a failure to write the metadata file; the metadata
	// in memory is then unchanged.
	ErrPersist = errors.New("persist channel metadata")
)

// Meta is what the operator records about a channel. Files written by
// 0.21.0 and earlier may also hold cost_ratio and balance; they are ignored.
type Meta struct {
	AuthIndex   string    `json:"auth_index"`
	DisplayName string    `json:"display_name,omitempty"`
	Vendor      string    `json:"vendor,omitempty"`
	Note        string    `json:"note,omitempty"`
	UpdatedAt   time.Time `json:"updated_at,omitzero"`
}

// MetaPatch changes the given fields of one channel's Meta.
type MetaPatch struct {
	AuthIndex   string  `json:"auth_index"`
	DisplayName *string `json:"display_name,omitempty"`
	Vendor      *string `json:"vendor,omitempty"`
	Note        *string `json:"note,omitempty"`
}

// Observation is the part of one usage record the statistics need.
type Observation struct {
	AuthIndex  string
	AuthID     string
	Provider   string
	BaseURL    string
	Model      string
	Failed     bool
	StatusCode int
	Latency    time.Duration
	TTFT       time.Duration
	At         time.Time
	// RequestedAt is when CPA started the attempt (UsageRecord.RequestedAt);
	// zero when the host did not send it. TraceID is the downstream
	// request's; with RequestedAt it names one executor call.
	RequestedAt time.Time
	TraceID     string
	// Token counts as CPA reported them.
	InputTokens      int64
	OutputTokens     int64
	CacheReadTokens  int64
	CacheWriteTokens int64
	// ListCostNano is the attempt's cost at list price (nano-USD, no
	// downstream multiplier); 0 when unpriced or failed.
	ListCostNano int64
	// ErrorSnippet is a short excerpt of the upstream error; the registry
	// keeps at most 200 characters of it and drops anything that looks like
	// a credential.
	ErrorSnippet string
}

// Summary aggregates a window of attempts.
//
// StatusCodes counts failed attempts by upstream status ("0" when the
// failure carried none). The latency and TTFT percentiles are estimated
// from fixed histogram buckets over successful attempts only, TTFT only
// where one was reported, so a burst of instant rejections does not pull
// them down.
type Summary struct {
	Attempts     int64          `json:"attempts"`
	Failures     int64          `json:"failures"`
	SuccessRate  float64        `json:"success_rate"`
	StatusCodes  map[string]int `json:"status_codes,omitempty"`
	LatencyP50Ms int64          `json:"latency_p50_ms"`
	LatencyP95Ms int64          `json:"latency_p95_ms"`
	TTFTP50Ms    int64          `json:"ttft_p50_ms"`
	TTFTP95Ms    int64          `json:"ttft_p95_ms"`
	InputTokens  int64          `json:"input_tokens"`
	OutputTokens int64          `json:"output_tokens"`
	ListCostUSD  float64        `json:"list_cost_usd"`
	LastAt       time.Time      `json:"last_at,omitzero"`
}

// ErrorEntry is one recent failed attempt.
type ErrorEntry struct {
	At         time.Time `json:"at"`
	StatusCode int       `json:"status_code"`
	Model      string    `json:"model"`
	Snippet    string    `json:"snippet,omitempty"`
}

// Point is one hour of a channel's history.
type Point struct {
	Hour     time.Time `json:"hour"`
	Attempts int64     `json:"attempts"`
	Failures int64     `json:"failures"`
	// LatencyP50Ms is the hour's median latency.
	LatencyP50Ms int64   `json:"latency_p50_ms"`
	ListCostUSD  float64 `json:"list_cost_usd"`
}

// View is one channel in the channel list.
type View struct {
	AuthIndex string  `json:"auth_index"`
	AuthID    string  `json:"auth_id,omitempty"`
	Provider  string  `json:"provider,omitempty"`
	BaseURL   string  `json:"base_url,omitempty"`
	Meta      Meta    `json:"meta"`
	Last24h   Summary `json:"last_24h"`
	Today     Summary `json:"today"`
}

// Detail adds history and recent errors to a View.
type Detail struct {
	View
	Last7d Summary `json:"last_7d"`
	// Hours holds the last 168 hours, oldest first, empty hours included.
	Hours []Point `json:"hours"`
	// RecentErrors holds up to 20 failed attempts of the last 7 days,
	// newest first.
	RecentErrors []ErrorEntry `json:"recent_errors"`
}

// Credential is one API key of CPA's config.yaml, which detection tests and
// adapter rule tries use; it never leaves the process. ID and Provider are what
// CPA's auth manager calls it, so a model request can be pinned to it; Group
// is its v8 group or openai-compatibility provider name. ProxyURL is the
// proxy CPA sends its requests through: the key's proxy-url, else CPA's
// global one; empty means the environment's proxy, and "direct" or "none"
// means none. Disabled says the key's excluded-models holds "*", which is how
// CPA's panel disables a config API key: CPA routes no model to it, so a
// detection test pinned to it can only fail.
type Credential struct {
	AuthIndex string
	ID        string
	Provider  string
	Kind      string
	Group     string
	BaseURL   string
	APIKey    string
	ProxyURL  string
	Disabled  bool
}

// Registry owns channel metadata and statistics. Observe is safe
// for concurrent use and takes constant time; nothing on the request path
// waits for I/O.
type Registry struct {
	metaPath  string
	statsPath string
	loc       *time.Location
	canWrite  func() bool
	now       func() time.Time
	logf      func(level, message string)

	// mu guards the channels map; each channel guards its own statistics.
	// Lock order: flushMu, metaWriteMu, handoffMu, mu, then a channel's mu.
	// metaMu comes last: nothing is taken while it is held.
	mu       sync.RWMutex
	channels map[string]*channelStats
	// generation counts statistics changes; flushed is the generation the
	// last successful FlushStats wrote.
	generation atomic.Uint64
	flushMu    sync.Mutex
	flushed    uint64

	// metaWriteMu serializes UpdateMeta from validation to the swap; metaMu
	// guards the metas map, which is replaced, never modified in place.
	metaWriteMu sync.Mutex
	metaMu      sync.RWMutex
	metas       map[string]Meta

	// handoffMu makes HandOff a single instant for Observe: Observe holds it
	// shared for its whole update, HandOff exclusively while it takes the
	// snapshot and starts passing observations to handoff.
	handoffMu sync.RWMutex
	handoff   func(Observation)
}

// Options configures a Registry. Paths are files next to the plugin state
// file. Location sets the day boundary of Today.
type Options struct {
	MetaPath  string
	StatsPath string
	Location  *time.Location
	CanWrite  func() bool
	Now       func() time.Time
	Logf      func(level, message string)
}

// NewRegistry loads metadata and the last statistics snapshot (missing
// files mean empty). Statistics older than 7 days are dropped; a malformed
// file is an error. Nil options default to UTC, always writable, time.Now
// and no logging.
func NewRegistry(opts Options) (*Registry, error) {
	r := &Registry{
		metaPath:  opts.MetaPath,
		statsPath: opts.StatsPath,
		loc:       opts.Location,
		canWrite:  opts.CanWrite,
		now:       opts.Now,
		logf:      opts.Logf,
	}
	if r.loc == nil {
		r.loc = time.UTC
	}
	if r.canWrite == nil {
		r.canWrite = func() bool { return true }
	}
	if r.now == nil {
		r.now = time.Now
	}
	if r.logf == nil {
		r.logf = func(string, string) {}
	}
	var err error
	if r.metas, err = loadMeta(r.metaPath); err != nil {
		return nil, err
	}
	if r.channels, err = loadStats(r.statsPath, r.now()); err != nil {
		return nil, err
	}
	if r.canWrite() {
		for _, path := range []string{r.metaPath, r.statsPath} {
			// Temp files carry wall-clock modification times, so their age is
			// not measured with the injected clock.
			if path != "" {
				if err := persist.CleanupStaleTemps(path, time.Now()); err != nil {
					r.logf("warn", "cpa-key-policy: channel files: "+err.Error())
				}
			}
		}
	}
	return r, nil
}

// isAuthIndex reports whether s is exactly 16 lowercase hex characters, the
// form CPA emits.
func isAuthIndex(s string) bool {
	if len(s) != 16 {
		return false
	}
	for i := 0; i < len(s); i++ {
		if c := s[i]; (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// normalizeAuthIndex accepts an operator-supplied auth_index in any case.
func normalizeAuthIndex(s string) (string, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	if !isAuthIndex(s) {
		return "", invalidf("auth_index must be 16 hexadecimal characters")
	}
	return s, nil
}

// invalidf wraps ErrInvalid with the detail of a rejected input.
func invalidf(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, args...))
}
