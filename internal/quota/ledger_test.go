package quota

import (
	"errors"
	"sync"
	"testing"
	"time"
)

func TestPacificQuotaDaysAndReset(t *testing.T) {
	for _, test := range []struct {
		day   string
		start time.Time
		hours int64
	}{
		{"2026-03-08", time.Date(2026, 3, 8, 8, 0, 0, 0, time.UTC), 23},
		{"2026-11-01", time.Date(2026, 11, 1, 7, 0, 0, 0, time.UTC), 25},
	} {
		t.Run(test.day, func(t *testing.T) {
			now := test.start
			l, err := New(t.TempDir(), func() time.Time { return now })
			if err != nil {
				t.Fatal(err)
			}
			s, err := l.Read()
			if err != nil {
				t.Fatal(err)
			}
			if s.Day != test.day || s.ResetAfterSeconds != test.hours*3600 ||
				!s.ResetAt.Equal(test.start.Add(time.Duration(test.hours)*time.Hour)) ||
				s.ResetAt.Hour() != 0 {
				t.Fatalf("wrong Pacific day or reset: %+v", s)
			}
			if err := l.Charge("playlists.list", 1, ChargeOptions{}); err != nil {
				t.Fatal(err)
			}
			newLimit := 12000
			if _, err := l.Configure(&newLimit, false); err != nil {
				t.Fatal(err)
			}
			if _, err := l.MarkExhausted(); err != nil {
				t.Fatal(err)
			}
			now = s.ResetAt.Add(-time.Nanosecond)
			before, err := l.Read()
			if err != nil || !before.Exhausted || before.Used != 1 || before.ResetAfterSeconds != 1 {
				t.Fatalf("state just before reset = %+v, %v", before, err)
			}
			now = s.ResetAt
			next, err := l.Read()
			if err != nil || next.Day == s.Day || next.Used != 0 || next.Exhausted ||
				next.Limit != newLimit || len(next.ByOperation) != 0 {
				t.Fatalf("state at reset = %+v, %v", next, err)
			}
		})
	}
}

func TestConcurrentChargesCannotSpendReserve(t *testing.T) {
	dir := t.TempDir()
	clock := func() time.Time { return time.Date(2026, 5, 8, 15, 0, 0, 0, time.UTC) }
	first, err := New(dir, clock)
	if err != nil {
		t.Fatal(err)
	}
	second, err := New(dir, clock)
	if err != nil {
		t.Fatal(err)
	}
	limit := 1000
	if _, err := first.Configure(&limit, false); err != nil {
		t.Fatal(err)
	}

	const callers = 40
	var wg sync.WaitGroup
	results := make(chan error, callers)
	start := make(chan struct{})
	for i := range callers {
		wg.Go(func() {
			<-start
			ledger := first
			if i%2 == 1 {
				ledger = second
			}
			results <- ledger.Charge("playlistItems.insert", 50, ChargeOptions{EnforceReserve: true})
		})
	}
	close(start)
	wg.Wait()
	close(results)

	success := 0
	for err := range results {
		var outOfQuota *LimitError
		if err == nil {
			success++
		} else if !errors.As(err, &outOfQuota) || outOfQuota.ResetAt.IsZero() {
			t.Fatalf("unexpected charge error: %v", err)
		}
	}
	state, err := second.Read()
	if err != nil || success != 10 || state.Used != 500 || state.ByOperation["playlistItems.insert"] != 10 {
		t.Fatalf("charged %d requests, state %+v, error %v", success, state, err)
	}
	if err := first.Charge("manual", 500, ChargeOptions{}); err != nil {
		t.Fatalf("manual charge should access reserve: %v", err)
	}
	if err := first.Charge("manual", 1, ChargeOptions{}); err == nil {
		t.Fatal("daily ceiling was breached")
	}
}

func TestMeterAndResetExhausted(t *testing.T) {
	l, err := New(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	budget, writes := 51, 1
	meter := &Meter{Ledger: l, Budget: &budget, MaxWrites: &writes}
	for _, op := range []struct {
		cost  int
		write bool
	}{{50, true}, {1, false}} {
		if err := meter.Charge("op", op.cost, op.write); err != nil {
			t.Fatal(err)
		}
	}
	var exhausted *BudgetError
	if err := meter.Charge("op", 1, false); !errors.As(err, &exhausted) || exhausted.Writes {
		t.Fatalf("budget limit returned %v", err)
	}
	budget++
	exhausted = nil
	if err := meter.Charge("op", 1, true); !errors.As(err, &exhausted) || !exhausted.Writes {
		t.Fatalf("write limit returned %v", err)
	}
	if _, err := l.MarkExhausted(); err != nil {
		t.Fatal(err)
	}
	var limitErr *LimitError
	if err := l.Charge("op", 1, ChargeOptions{}); !errors.As(err, &limitErr) {
		t.Fatalf("remotely exhausted ledger accepted charge: %v", err)
	}
	state, err := l.Configure(nil, true)
	if err != nil || state.Exhausted || state.Used != 51 || state.ByOperation["op"] != 2 {
		t.Fatalf("reset returned %+v, %v", state, err)
	}
}
