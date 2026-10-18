package policy

import (
	"math"
	"net/http"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// testClock is a settable time source safe to share with background goroutines.
type testClock struct {
	mu  sync.Mutex
	now time.Time
}

func newTestClock(now time.Time) *testClock {
	return &testClock{now: now}
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *testClock) Set(now time.Time) {
	c.mu.Lock()
	c.now = now
	c.mu.Unlock()
}

func (c *testClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

func mustShanghai(t *testing.T) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Fatalf("load Asia/Shanghai: %v", err)
	}
	return loc
}

func mustHash(t *testing.T, plain string) string {
	t.Helper()
	hash, err := HashKey(plain)
	if err != nil {
		t.Fatal(err)
	}
	return hash
}

func nearly(left, right float64) bool {
	return math.Abs(left-right) <= 1e-9
}

func tokenPrice(name, provider string, input, output float64) ModelPrice {
	return ModelPrice{Name: name, Provider: provider, BillingMode: "tokens", InputPricePerMillion: input, OutputPricePerMillion: output}
}

func perCallPrice(name, provider string, usd float64) ModelPrice {
	return ModelPrice{Name: name, Provider: provider, BillingMode: "per_call", PerCallUSD: usd}
}

func testKey(t *testing.T, id, plain string) KeyConfig {
	t.Helper()
	return KeyConfig{ID: id, Name: id, Enabled: true, KeyHash: mustHash(t, plain), KeyPreview: PreviewKey(plain)}
}

// newStoreAt configures a fresh store on a new state path at the clock's time.
func newStoreAt(t *testing.T, clock *testClock, statePath string, keys []KeyConfig, prices []ModelPrice) *Store {
	t.Helper()
	store := NewStore()
	store.SetClock(clock.Now)
	if err := store.Configure(Config{Enabled: true, StateFile: statePath, UsageTimezone: "Asia/Shanghai", Keys: keys, Models: prices}); err != nil {
		t.Fatal(err)
	}
	return store
}

func newStore(t *testing.T, clock *testClock, keys []KeyConfig, prices []ModelPrice) *Store {
	t.Helper()
	return newStoreAt(t, clock, filepath.Join(t.TempDir(), "state.json"), keys, prices)
}

func reopenStore(t *testing.T, clock *testClock, statePath string) *Store {
	t.Helper()
	store := NewStore()
	store.SetClock(clock.Now)
	if err := store.Configure(Config{Enabled: true, StateFile: statePath, UsageTimezone: "Asia/Shanghai"}); err != nil {
		t.Fatal(err)
	}
	return store
}

func bearer(plain string) http.Header {
	return http.Header{"Authorization": {"Bearer " + plain}}
}

func tokenEvent(keyID, provider, model string, detail UsageDetail) UsageEvent {
	return UsageEvent{Provider: provider, Model: model, APIKey: keyID, Detail: detail}
}

// admitRequest is what a client sees for one request: CPA authenticates it
// and, when authentication deferred RPM and quota, the plugin's request
// interceptor admits it.
type testAdmission struct {
	AuthDecision
	Denial *Denial
}

func admitRequest(store *Store, method, path string, headers http.Header, query map[string][]string) testAdmission {
	decision := store.Authenticate(method, path, headers, query)
	result := testAdmission{AuthDecision: decision, Denial: decision.Denial}
	if decision.Allowed && decision.Deferred {
		if denial := store.AdmitIntercepted(headers, path, "trace-"+path); denial != nil {
			result.Allowed, result.Reason, result.Denial = false, denial.Reason, denial
		}
	}
	return result
}

func keyByID(t *testing.T, store *Store, id string) KeyConfig {
	t.Helper()
	for _, key := range store.Keys() {
		if key.ID == id {
			return key
		}
	}
	t.Fatalf("key %q not found", id)
	return KeyConfig{}
}

// bucketSums flattens a ledger snapshot to (key, date, model) → bucket.
func bucketSums(keys map[string]*KeyUsage) map[[3]string]Bucket {
	sums := make(map[[3]string]Bucket)
	for id, usage := range keys {
		for date, models := range usage.Days {
			for model, bucket := range models {
				sums[[3]string{id, date, model}] = bucket
			}
		}
	}
	return sums
}
