package metrics

import (
	"math"
	"strings"
	"sync"
	"testing"
)

func TestExposition(t *testing.T) {
	var r Registry
	c := r.NewCounterVec("requests_total", "Requests.\nBy code.", "route", "code")
	c.Inc("/a", "200")
	c.Add(2, "/a", "200")
	c.Inc(`we"ird\path`+"\n", "500")
	plain := r.NewCounterVec("events_total", "Events.")
	plain.Inc()
	r.NewGaugeFunc("up", "Up.", func() float64 { return 1 })
	r.NewGaugeFunc("inf", "Inf.", func() float64 { return math.Inf(1) })
	h := r.NewHistogram("latency_seconds", "Latency.", []float64{0.001, 0.01})
	for _, v := range []float64{0.0005, 0.001, 0.005, 1} {
		h.Observe(v)
	}
	var b strings.Builder
	if _, err := r.WriteTo(&b); err != nil {
		t.Fatal(err)
	}
	want := `# HELP requests_total Requests. By code.
# TYPE requests_total counter
requests_total{route="/a",code="200"} 3
requests_total{route="we\"ird\\path\n",code="500"} 1
# HELP events_total Events.
# TYPE events_total counter
events_total 1
# HELP up Up.
# TYPE up gauge
up 1
# HELP inf Inf.
# TYPE inf gauge
inf +Inf
# HELP latency_seconds Latency.
# TYPE latency_seconds histogram
latency_seconds_bucket{le="0.001"} 2
latency_seconds_bucket{le="0.01"} 3
latency_seconds_bucket{le="+Inf"} 4
latency_seconds_sum 1.0065
latency_seconds_count 4
`
	if b.String() != want {
		t.Errorf("got:\n%s\nwant:\n%s", b.String(), want)
	}
	if c.Value("/a", "200") != 3 || c.Value("/b", "200") != 0 || h.Count() != 4 {
		t.Error("Value/Count")
	}
	if formatFloat(math.Inf(-1)) != "-Inf" || formatFloat(math.NaN()) != "NaN" {
		t.Error("formatFloat")
	}
}

func TestPanics(t *testing.T) {
	var r Registry
	for name, f := range map[string]func(){
		"labels":   func() { r.NewCounterVec("x", "x", "a").Inc() },
		"unsorted": func() { r.NewHistogram("h", "h", []float64{2, 1}) },
	} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("%s: no panic", name)
				}
			}()
			f()
		}()
	}
}

func TestConcurrent(t *testing.T) {
	var r Registry
	c := r.NewCounterVec("c", "c", "k")
	h := r.NewHistogram("h", "h", []float64{1})
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 1000; j++ {
				c.Inc("a")
				h.Observe(0.5)
			}
		}()
	}
	wg.Wait()
	if c.Value("a") != 8000 || h.Count() != 8000 {
		t.Errorf("counts %d %d", c.Value("a"), h.Count())
	}
}
