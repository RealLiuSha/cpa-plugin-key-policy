package plugin

import (
	"slices"
	"sync"
	"sync/atomic"
	"time"
)

// timingSamples is how many recent durations each method keeps for its
// percentiles.
const timingSamples = 512

// hookTimings records how long each host call took inside the plugin, so the
// settings page shows what the plugin adds to a request. Writes are a few
// atomic operations; percentiles are computed only when read.
type hookTimings struct {
	methods sync.Map // method name -> *methodTiming
}

type methodTiming struct {
	count   atomic.Int64
	max     atomic.Int64
	next    atomic.Uint64
	samples [timingSamples]atomic.Int64
}

// HookTiming is one method's row on the settings page, in microseconds.
type HookTiming struct {
	Method string `json:"method"`
	Count  int64  `json:"count"`
	P50US  int64  `json:"p50_us"`
	P99US  int64  `json:"p99_us"`
	MaxUS  int64  `json:"max_us"`
}

func (t *hookTimings) record(method string, elapsed time.Duration) {
	value, ok := t.methods.Load(method)
	if !ok {
		value, _ = t.methods.LoadOrStore(method, &methodTiming{})
	}
	timing := value.(*methodTiming)
	micros := elapsed.Microseconds()
	timing.count.Add(1)
	timing.samples[timing.next.Add(1)%timingSamples].Store(micros)
	for {
		current := timing.max.Load()
		if micros <= current || timing.max.CompareAndSwap(current, micros) {
			break
		}
	}
}

func (t *hookTimings) snapshot() []HookTiming {
	rows := []HookTiming{}
	t.methods.Range(func(key, value any) bool {
		timing := value.(*methodTiming)
		count := timing.count.Load()
		kept := min(count, timingSamples)
		samples := make([]int64, 0, kept)
		for i := range kept {
			samples = append(samples, timing.samples[(uint64(i)+1)%timingSamples].Load())
		}
		slices.Sort(samples)
		row := HookTiming{Method: key.(string), Count: count, MaxUS: timing.max.Load()}
		if len(samples) > 0 {
			row.P50US = samples[len(samples)/2]
			row.P99US = samples[min(len(samples)-1, len(samples)*99/100)]
		}
		rows = append(rows, row)
		return true
	})
	slices.SortFunc(rows, func(x, y HookTiming) int {
		switch {
		case x.Method < y.Method:
			return -1
		case x.Method > y.Method:
			return 1
		}
		return 0
	})
	return rows
}
