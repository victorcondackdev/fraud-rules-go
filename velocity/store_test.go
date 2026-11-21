package velocity

import (
	"errors"
	"fmt"
	"math"
	"math/rand/v2"
	"sync"
	"testing"
	"time"
)

var t0 = time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)

type naiveEvent struct {
	key    string
	t      int64
	amount int64
	cp     string
}

// naive answers a query the slow way, using the documented window
// semantics: every bucket overlapping [now-w, now] of the tier chosen.
func naive(s *Store, evs []naiveEvent, key string, w time.Duration, now time.Time) Stats {
	ti, err := s.tierFor(w)
	if err != nil {
		panic(err)
	}
	g := s.tiers[ti].g
	first := floorDiv(now.UnixNano()-int64(w), g)
	last := floorDiv(now.UnixNano(), g)
	st := Stats{Exact: true}
	seen := map[string]bool{}
	for _, e := range evs {
		if e.key != key {
			continue
		}
		if ep := floorDiv(e.t, g); ep >= first && ep <= last {
			st.Count++
			st.Sum += e.amount
			if e.cp != "" && !seen[e.cp] {
				seen[e.cp] = true
				st.Distinct++
			}
		}
	}
	return st
}

// TestQueryMatchesNaive is a property test: for random event streams and
// random windows, the bucketed store agrees exactly with a brute-force
// scan of the same window.
func TestQueryMatchesNaive(t *testing.T) {
	r := rand.New(rand.NewPCG(1, 2))
	configs := []Config{
		{},
		{Tiers: []Tier{{Granularity: 10 * time.Second, Horizon: 10 * time.Minute}, {Granularity: time.Minute, Horizon: 2 * time.Hour}}, Shards: 2},
	}
	for ci, cfg := range configs {
		cfg.ExactDistinct = 1 << 20 // keep distinct exact for this property
		for trial := 0; trial < 150; trial++ {
			s, err := New(cfg)
			if err != nil {
				t.Fatal(err)
			}
			var evs []naiveEvent
			now := t0.Add(time.Duration(r.IntN(1000)) * time.Second)
			for i := 0; i < 1+r.IntN(300); i++ {
				gap := time.Duration(r.ExpFloat64() * float64([]time.Duration{time.Second, time.Minute, time.Hour}[r.IntN(3)]))
				now = now.Add(gap)
				e := naiveEvent{key: fmt.Sprint("k", r.IntN(3)), t: now.UnixNano(), amount: r.Int64N(100_000), cp: fmt.Sprint("c", r.IntN(20))}
				if r.IntN(10) == 0 {
					e.cp = ""
				}
				evs = append(evs, e)
				s.Record(Event{Key: e.key, Time: now, Amount: e.amount, Counterparty: e.cp})
			}
			for q := 0; q < 20; q++ {
				maxW := s.MaxWindow()
				w := time.Duration(1 + r.Int64N(int64(maxW)))
				if r.IntN(2) == 0 {
					w = time.Duration(1+r.Int64N(int64(time.Hour))) % maxW
					if w <= 0 {
						w = time.Second
					}
				}
				at := now.Add(time.Duration(r.Int64N(int64(time.Hour))))
				key := fmt.Sprint("k", r.IntN(4))
				got, err := s.Query(key, w, at)
				if err != nil {
					t.Fatal(err)
				}
				want := naive(s, evs, key, w, at)
				if got != want {
					t.Fatalf("config %d trial %d: Query(%s, %v, +%v) = %+v, want %+v", ci, trial, key, w, at.Sub(t0), got, want)
				}
			}
		}
	}
}

// TestWindowNeverUndercounts pins the documented guarantee against the
// exact sliding window [now-w, now].
func TestWindowNeverUndercounts(t *testing.T) {
	s, _ := New(Config{})
	r := rand.New(rand.NewPCG(3, 4))
	var ts []time.Time
	now := t0
	for i := 0; i < 2000; i++ {
		now = now.Add(time.Duration(r.IntN(120)) * time.Second)
		ts = append(ts, now)
		s.Record(Event{Key: "k", Time: now, Amount: 1})
	}
	for _, w := range []time.Duration{time.Minute, 10 * time.Minute, time.Hour, 6 * time.Hour, 24 * time.Hour} {
		g, _ := s.Granularity(w)
		var exact, slack int64
		for _, x := range ts {
			if !x.Before(now.Add(-w)) && !x.After(now) {
				exact++
			}
			if !x.Before(now.Add(-w-g)) && !x.After(now) {
				slack++
			}
		}
		st, _ := s.Query("k", w, now)
		if st.Count < exact || st.Count > slack {
			t.Errorf("w=%v: count %d outside [%d, %d]", w, st.Count, exact, slack)
		}
	}
}

func TestDistinctFallsBackToHLL(t *testing.T) {
	s, _ := New(Config{ExactDistinct: 8})
	for i := 0; i < 5000; i++ {
		s.Record(Event{Key: "mule", Time: t0.Add(time.Duration(i) * 500 * time.Millisecond), Amount: 100, Counterparty: fmt.Sprint("ben-", i%3000)})
	}
	st, err := s.Query("mule", time.Hour, t0.Add(2500*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if st.Exact {
		t.Error("expected an HLL estimate")
	}
	if rel := math.Abs(float64(st.Distinct)-3000) / 3000; rel > 0.065 {
		t.Errorf("distinct = %d, want ~3000 (rel err %.3f)", st.Distinct, rel)
	}
	if st.Count != 5000 || st.Sum != 500000 {
		t.Errorf("count/sum = %d/%d", st.Count, st.Sum)
	}
	// Buckets below the exact limit stay exact, even unioned.
	for i := 0; i < 8; i++ {
		s.Record(Event{Key: "quiet", Time: t0.Add(time.Duration(i) * time.Minute), Counterparty: fmt.Sprint("ben-", i%5)})
	}
	small, _ := s.Query("quiet", time.Hour, t0.Add(10*time.Minute))
	if !small.Exact || small.Distinct != 5 || small.Count != 8 {
		t.Errorf("small window = %+v", small)
	}
}

func TestOutOfOrder(t *testing.T) {
	s, _ := New(Config{})
	s.Record(Event{Key: "k", Time: t0.Add(time.Hour), Amount: 1})
	s.Record(Event{Key: "k", Time: t0.Add(30 * time.Minute), Amount: 2}) // late, inside the fine horizon
	s.Record(Event{Key: "k", Time: t0.Add(50 * time.Minute), Amount: 4}) // late, between existing buckets
	s.Record(Event{Key: "k", Time: t0.Add(30 * time.Minute), Amount: 8}) // same late bucket again
	st, _ := s.Query("k", 45*time.Minute, t0.Add(time.Hour))
	if st.Count != 4 || st.Sum != 15 {
		t.Errorf("late events: %+v", st)
	}
	// Older than the 3h fine horizon: dropped from the minute tier, kept by the hour tier.
	s.Record(Event{Key: "k", Time: t0.Add(-5 * time.Hour), Amount: 100})
	if st, _ := s.Query("k", 2*time.Hour, t0.Add(time.Hour)); st.Sum != 15 {
		t.Errorf("too-late event leaked into the fine tier: %+v", st)
	}
	if st, _ := s.Query("k", 24*time.Hour, t0.Add(time.Hour)); st.Sum != 115 {
		t.Errorf("hour tier lost the late event: %+v", st)
	}
}

func TestRingWraps(t *testing.T) {
	s, _ := New(Config{Tiers: []Tier{{Granularity: time.Second, Horizon: 8 * time.Second}}})
	for i := 0; i < 100; i++ {
		s.Record(Event{Key: "k", Time: t0.Add(time.Duration(i) * time.Second), Amount: 1})
		st, _ := s.Query("k", 5*time.Second, t0.Add(time.Duration(i)*time.Second))
		if want := int64(min(i+1, 6)); st.Count != want {
			t.Fatalf("i=%d: count %d, want %d", i, st.Count, want)
		}
	}
	sr := s.shardFor("k").keys["k"]
	if l := sr.rings[0].len(); l > 8 {
		t.Errorf("ring holds %d buckets, horizon allows 8", l)
	}
}

func TestQueryErrors(t *testing.T) {
	s, _ := New(Config{})
	if _, err := s.Query("k", 0, t0); !errors.Is(err, ErrWindow) {
		t.Errorf("zero window: %v", err)
	}
	if _, err := s.Query("k", 365*24*time.Hour, t0); !errors.Is(err, ErrWindowTooLarge) {
		t.Errorf("huge window: %v", err)
	}
	if _, err := s.Granularity(-time.Second); err == nil {
		t.Error("Granularity of negative window")
	}
	if g, _ := s.Granularity(time.Hour); g != time.Minute {
		t.Errorf("1h granularity = %v", g)
	}
	if g, _ := s.Granularity(90 * 24 * time.Hour); g != 24*time.Hour {
		t.Errorf("90d granularity = %v", g)
	}
	if st, err := s.Query("missing", time.Hour, t0); err != nil || st != (Stats{Exact: true}) {
		t.Errorf("missing key = %+v, %v", st, err)
	}
}

func TestConfigValidation(t *testing.T) {
	bad := []Config{
		{HLLPrecision: 3},
		{Tiers: []Tier{{Granularity: 0, Horizon: time.Hour}}},
		{Tiers: []Tier{{Granularity: time.Hour, Horizon: time.Hour}}},
		{Tiers: []Tier{{Granularity: time.Minute, Horizon: 90 * time.Second}}},
		{Tiers: []Tier{{Granularity: time.Hour, Horizon: 10 * time.Hour}, {Granularity: time.Minute, Horizon: 20 * time.Hour}}},
		{Tiers: []Tier{{Granularity: time.Millisecond, Horizon: time.Hour}}},
	}
	for i, c := range bad {
		if _, err := New(c); !errors.Is(err, ErrConfig) {
			t.Errorf("config %d: %v", i, err)
		}
	}
	s, err := New(Config{Shards: 5})
	if err != nil || len(s.shards) != 8 {
		t.Errorf("shards rounded to %d", len(s.shards))
	}
}

func TestMaxKeysEvictsLRU(t *testing.T) {
	s, _ := New(Config{Shards: 1, MaxKeys: 2})
	rec := func(k string) { s.Record(Event{Key: k, Time: t0, Amount: 1}) }
	rec("a")
	rec("b")
	rec("a") // a is now most recent
	rec("c") // evicts b
	if s.Len() != 2 || s.Evictions() != 1 {
		t.Fatalf("len %d evictions %d", s.Len(), s.Evictions())
	}
	if st, _ := s.Query("b", time.Hour, t0); st.Count != 0 {
		t.Error("b should have been evicted")
	}
	if st, _ := s.Query("a", time.Hour, t0); st.Count != 2 {
		t.Error("a should have survived")
	}
	unbounded, _ := New(Config{MaxKeys: -1, Shards: 1})
	for i := 0; i < 100; i++ {
		unbounded.Record(Event{Key: fmt.Sprint(i), Time: t0})
	}
	if unbounded.Len() != 100 {
		t.Errorf("unbounded store has %d keys", unbounded.Len())
	}
}

func TestSweepTTL(t *testing.T) {
	s, _ := New(Config{TTL: time.Hour})
	s.Record(Event{Key: "old", Time: t0})
	s.Record(Event{Key: "new", Time: t0.Add(2 * time.Hour)})
	if n := s.Sweep(t0.Add(150 * time.Minute)); n != 1 {
		t.Errorf("swept %d, want 1", n)
	}
	if s.Len() != 1 {
		t.Errorf("len = %d", s.Len())
	}
	if n := s.Sweep(t0.Add(150 * time.Minute)); n != 0 {
		t.Errorf("second sweep removed %d", n)
	}
}

func TestConcurrentRecordQuery(t *testing.T) {
	s, _ := New(Config{Shards: 4})
	var wg sync.WaitGroup
	const workers, per = 8, 2000
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < per; i++ {
				at := t0.Add(time.Duration(i) * time.Second)
				s.Record(Event{Key: fmt.Sprint("k", i%10), Time: at, Amount: 1, Counterparty: fmt.Sprint(w)})
				if _, err := s.Query(fmt.Sprint("k", (i+w)%10), 10*time.Minute, at); err != nil {
					t.Error(err)
					return
				}
			}
		}(w)
	}
	wg.Wait()
	var total int64
	for k := 0; k < 10; k++ {
		st, _ := s.Query(fmt.Sprint("k", k), 2*time.Hour+50*time.Minute, t0.Add(per*time.Second))
		total += st.Count
		if st.Distinct != workers {
			t.Errorf("k%d distinct = %d, want %d", k, st.Distinct, workers)
		}
	}
	if total != workers*per {
		t.Errorf("total = %d, want %d", total, workers*per)
	}
}

func TestFloorDiv(t *testing.T) {
	for _, c := range [][3]int64{{7, 2, 3}, {-7, 2, -4}, {-8, 2, -4}, {0, 5, 0}, {-1, 5, -1}} {
		if got := floorDiv(c[0], c[1]); got != c[2] {
			t.Errorf("floorDiv(%d, %d) = %d, want %d", c[0], c[1], got, c[2])
		}
	}
}

func TestQueryCounts(t *testing.T) {
	s, _ := New(Config{})
	for i := 0; i < 10; i++ {
		s.Record(Event{Key: "k", Time: t0.Add(time.Duration(i) * time.Minute), Amount: 10, Counterparty: fmt.Sprint(i % 4)})
	}
	full, _ := s.Query("k", time.Hour, t0.Add(10*time.Minute))
	counts, _ := s.QueryCounts("k", time.Hour, t0.Add(10*time.Minute))
	if full.Count != counts.Count || full.Sum != counts.Sum || full.Distinct != 4 || counts.Distinct != 0 || !counts.Exact {
		t.Errorf("full %+v, counts %+v", full, counts)
	}
	if _, err := s.QueryCounts("k", 0, t0); !errors.Is(err, ErrWindow) {
		t.Error(err)
	}
}

func TestCountUnique(t *testing.T) {
	r := rand.New(rand.NewPCG(5, 6))
	for trial := 0; trial < 200; trial++ {
		n := r.IntN(2000)
		hs := make([]uint64, n)
		want := map[uint64]bool{}
		for i := range hs {
			hs[i] = r.Uint64N(uint64(n/2 + 2)) // includes 0 and duplicates
			want[hs[i]] = true
		}
		var scratch []uint64
		if trial%2 == 0 {
			scratch = append(make([]uint64, 0, 4), hs...)
		}
		if got := countUnique(hs, &scratch); got != int64(len(want)) {
			t.Fatalf("n=%d: got %d, want %d", n, got, len(want))
		}
	}
}
