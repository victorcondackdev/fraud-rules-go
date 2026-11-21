package velocity

import (
	"fmt"
	"sync/atomic"
	"testing"
	"time"
)

func BenchmarkRecord(b *testing.B) {
	s, _ := New(Config{})
	keys := make([]string, 4096)
	for i := range keys {
		keys[i] = fmt.Sprint("p:", i)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		s.Record(Event{Key: keys[i&4095], Time: t0.Add(time.Duration(i) * time.Millisecond), Amount: 100, Counterparty: keys[(i*7)&4095]})
	}
}

func BenchmarkQuery(b *testing.B) {
	s, _ := New(Config{})
	for i := 0; i < 50_000; i++ {
		s.Record(Event{Key: fmt.Sprint("p:", i%100), Time: t0.Add(time.Duration(i) * 100 * time.Millisecond), Amount: 100, Counterparty: fmt.Sprint("b:", i%17)})
	}
	now := t0.Add(5000 * time.Second)
	for _, c := range []struct {
		name string
		w    time.Duration
	}{{"10m", 10 * time.Minute}, {"1h", time.Hour}, {"1d", 24 * time.Hour}, {"90d", 90 * 24 * time.Hour}} {
		w := c.w
		b.Run(c.name, func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				if _, err := s.Query("p:7", w, now); err != nil {
					b.Fatal(err)
				}
			}
		})
		b.Run(c.name+"/counts", func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				if _, err := s.QueryCounts("p:7", w, now); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkParallelMixed runs one Record and two Queries per iteration on
// every GOMAXPROCS goroutine, over 100k keys.
func BenchmarkParallelMixed(b *testing.B) {
	s, _ := New(Config{})
	const nkeys = 100_000
	keys := make([]string, nkeys)
	for i := range keys {
		keys[i] = fmt.Sprint("p:", i)
	}
	var clock atomic.Int64
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			n := clock.Add(1)
			at := t0.Add(time.Duration(n) * time.Millisecond)
			k := keys[(n*7919)%nkeys]
			s.Record(Event{Key: k, Time: at, Amount: 100, Counterparty: keys[n%nkeys]})
			_, _ = s.Query(k, 10*time.Minute, at)
			_, _ = s.Query(k, 24*time.Hour, at)
		}
	})
}
