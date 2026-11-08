package budget

import (
	"errors"
	"testing"
	"time"

	"github.com/garritfra/llmtee/internal/store"
)

func TestTrackerWindows(t *testing.T) {
	now := time.Date(2026, 3, 31, 23, 0, 0, 0, time.UTC)
	tr := New()
	tr.now = func() time.Time { return now }
	day := 1.0
	month := 10.0
	k := &store.Key{ID: "k", BudgetDayUSD: &day, BudgetMonthUSD: &month}
	tr.Add("k", 0.6, now)
	if err := tr.Check(k); err != nil {
		t.Fatal(err)
	}
	tr.Add("k", 0.4, now)
	var ex *ExceededError
	if err := tr.Check(k); !errors.As(err, &ex) || ex.Window != "daily" {
		t.Fatalf("expected daily exceeded, got %v", err)
	}
	// Late event from yesterday counts for the month only.
	tr.Add("k", 5, now.Add(-24*time.Hour))
	if s := tr.Spend("k"); s.Day != 1 || s.Month != 6 {
		t.Fatalf("spend %+v", s)
	}
	// Next day (and next month): reset.
	now = now.Add(2 * time.Hour)
	if s := tr.Spend("k"); s.Day != 0 || s.Month != 0 {
		t.Fatalf("not reset: %+v", s)
	}
	if err := tr.Check(k); err != nil {
		t.Fatal(err)
	}
	if tr.Check(nil) != nil || tr.Check(&store.Key{ID: "free"}) != nil {
		t.Error("keys without budget must pass")
	}
	if tot := tr.Total(); tot.Month != 0 {
		t.Errorf("total = %+v", tot)
	}
}
