// Package budget tracks spend per key in memory (UTC day and month windows)
// and enforces hard limits on the request path without database access.
package budget

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/garritfra/llmtee/internal/store"
)

type window struct {
	day, month           float64
	dayStart, monthStart int64
}

// Tracker holds the current spend per key id and in total.
type Tracker struct {
	mu    sync.Mutex
	keys  map[string]*window
	total window
	now   func() time.Time
}

// New returns an empty tracker.
func New() *Tracker {
	return &Tracker{keys: map[string]*window{}, now: time.Now}
}

// Starts returns the UTC start of the day and month containing t, in unix ms.
func Starts(t time.Time) (day, month int64) {
	t = t.UTC()
	d := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
	m := time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC)
	return d.UnixMilli(), m.UnixMilli()
}

func (w *window) roll(day, month int64) {
	if w.monthStart != month {
		w.month, w.monthStart = 0, month
	}
	if w.dayStart != day {
		w.day, w.dayStart = 0, day
	}
}

// Add records cost for a key at time ts and returns the key's and the total
// spend after the update.
func (t *Tracker) Add(keyID string, cost float64, ts time.Time) (key, total store.KeySpend) {
	day, month := Starts(ts)
	nd, nm := Starts(t.now())
	t.mu.Lock()
	defer t.mu.Unlock()
	// Late events from a previous window still count for that window only.
	addTo := func(w *window) {
		w.roll(nd, nm)
		if month == w.monthStart {
			w.month += cost
		}
		if day == w.dayStart {
			w.day += cost
		}
	}
	addTo(&t.total)
	total = store.KeySpend{Day: t.total.day, Month: t.total.month}
	if keyID == "" {
		return
	}
	w := t.keys[keyID]
	if w == nil {
		w = &window{}
		t.keys[keyID] = w
	}
	addTo(w)
	return store.KeySpend{Day: w.day, Month: w.month}, total
}

// Spend returns the current spend of a key.
func (t *Tracker) Spend(keyID string) store.KeySpend {
	nd, nm := Starts(t.now())
	t.mu.Lock()
	defer t.mu.Unlock()
	w := t.keys[keyID]
	if w == nil {
		return store.KeySpend{}
	}
	w.roll(nd, nm)
	return store.KeySpend{Day: w.day, Month: w.month}
}

// Total returns the current total spend over all keys.
func (t *Tracker) Total() store.KeySpend {
	nd, nm := Starts(t.now())
	t.mu.Lock()
	defer t.mu.Unlock()
	t.total.roll(nd, nm)
	return store.KeySpend{Day: t.total.day, Month: t.total.month}
}

// ExceededError describes a budget violation.
type ExceededError struct {
	Window string // "daily" or "monthly"
	Limit  float64
	Spent  float64
}

func (e *ExceededError) Error() string {
	return fmt.Sprintf("llmtee: %s budget of $%.2f exceeded for this key (spent $%.4f); resets at the start of the next %s (UTC)",
		e.Window, e.Limit, e.Spent, map[string]string{"daily": "day", "monthly": "month"}[e.Window])
}

// Check returns an *ExceededError when the key is at or over a limit.
func (t *Tracker) Check(k *store.Key) error {
	if k == nil || (k.BudgetDayUSD == nil && k.BudgetMonthUSD == nil) {
		return nil
	}
	s := t.Spend(k.ID)
	if k.BudgetDayUSD != nil && s.Day >= *k.BudgetDayUSD {
		return &ExceededError{"daily", *k.BudgetDayUSD, s.Day}
	}
	if k.BudgetMonthUSD != nil && s.Month >= *k.BudgetMonthUSD {
		return &ExceededError{"monthly", *k.BudgetMonthUSD, s.Month}
	}
	return nil
}

// Load rebuilds all counters from the database.
func (t *Tracker) Load(ctx context.Context, s store.Store) error {
	now := t.now()
	day, month := Starts(now)
	per, total, err := s.BudgetSpend(ctx, day, month)
	if err != nil {
		return err
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.keys = make(map[string]*window, len(per))
	for id, ks := range per {
		t.keys[id] = &window{day: ks.Day, month: ks.Month, dayStart: day, monthStart: month}
	}
	t.total = window{day: total.Day, month: total.Month, dayStart: day, monthStart: month}
	return nil
}
