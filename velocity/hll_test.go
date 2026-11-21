package velocity

import (
	"errors"
	"fmt"
	"math"
	"testing"
)

// TestHLLErrorBound checks the estimate against the true cardinality for
// sizes spanning the sparse, linear-counting and raw-estimator ranges. The
// tolerance is 4 standard errors (1.04/√m), which a correct implementation
// exceeds with probability well under 0.01% per check; the inputs are
// deterministic, so the test is not flaky.
func TestHLLErrorBound(t *testing.T) {
	for _, p := range []uint8{10, 12, 14} {
		h, _ := NewHLL(p)
		sigma := h.StdError()
		next := 0
		var worst float64
		for _, n := range []int{1, 2, 10, 100, 500, 1000, 2500, 5000, 10_000, 30_000, 100_000, 300_000, 1_000_000} {
			for ; next < n; next++ {
				h.AddHash(Hash64(fmt.Sprintf("beneficiary-%d", next)))
			}
			est := float64(h.Estimate())
			rel := math.Abs(est-float64(n)) / float64(n)
			worst = max(worst, rel/sigma)
			// Tiny cardinalities are exact up to hash collisions in a register.
			tol := 4 * sigma
			if n <= 10 {
				tol = 0.5
			}
			if rel > tol {
				t.Errorf("p=%d n=%d: estimate %.0f, relative error %.4f > %.4f", p, n, est, rel, tol)
			}
		}
		t.Logf("p=%d: σ=%.4f, worst |error| = %.2fσ, size %d bytes", p, sigma, worst, h.SizeBytes())
	}
}

func TestHLLSparseToDense(t *testing.T) {
	h, _ := NewHLL(10)
	for i := 0; i < 50; i++ {
		h.AddHash(Hash64(fmt.Sprint(i)))
		h.AddHash(Hash64(fmt.Sprint(i))) // duplicates do not count
	}
	if h.dense != nil {
		t.Fatal("50 items should stay sparse at p=10")
	}
	if e := h.Estimate(); e < 48 || e > 52 {
		t.Errorf("sparse estimate %d, want ~50", e)
	}
	sparse := h.SizeBytes()
	for i := 50; i < 5000; i++ {
		h.AddHash(Hash64(fmt.Sprint(i)))
	}
	if h.dense == nil || h.SizeBytes() != 1024 || sparse >= 1024 {
		t.Errorf("expected dense 1024-byte registers, sparse was %d", sparse)
	}
	h.Reset()
	if h.Estimate() != 0 || h.Precision() != 10 {
		t.Error("Reset")
	}
}

func TestHLLMerge(t *testing.T) {
	a, _ := NewHLL(12)
	b, _ := NewHLL(12)
	c, _ := NewHLL(12) // sparse
	for i := 0; i < 20_000; i++ {
		a.AddHash(Hash64(fmt.Sprint("a", i)))
		b.AddHash(Hash64(fmt.Sprint("b", i)))
	}
	for i := 0; i < 100; i++ {
		c.AddHash(Hash64(fmt.Sprint("c", i)))
		c.AddHash(Hash64(fmt.Sprint("a", i))) // overlaps a
	}
	if err := a.Merge(b); err != nil {
		t.Fatal(err)
	}
	if err := a.Merge(c); err != nil {
		t.Fatal(err)
	}
	if rel := math.Abs(float64(a.Estimate())-40_100) / 40_100; rel > 4*a.StdError() {
		t.Errorf("union estimate %d, relative error %.4f", a.Estimate(), rel)
	}
	// Merging dense into sparse densifies the receiver.
	d, _ := NewHLL(12)
	if err := d.Merge(b); err != nil || d.dense == nil {
		t.Errorf("merge into sparse: %v", err)
	}
	wrong, _ := NewHLL(10)
	if err := a.Merge(wrong); !errors.Is(err, ErrPrecision) {
		t.Errorf("precision mismatch: %v", err)
	}
}

func TestNewHLLPrecision(t *testing.T) {
	for _, p := range []uint8{0, 3, 17} {
		if _, err := NewHLL(p); !errors.Is(err, ErrPrecision) {
			t.Errorf("p=%d: %v", p, err)
		}
	}
	for _, p := range []uint8{4, 5, 6, 16} { // alpha special cases
		h, err := NewHLL(p)
		if err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 1<<p*4; i++ {
			h.AddHash(Hash64(fmt.Sprint(i)))
		}
		if e := float64(h.Estimate()); math.Abs(e-float64(int(1)<<p*4))/float64(int(1)<<p*4) > 5*h.StdError() {
			t.Errorf("p=%d: estimate %.0f", p, e)
		}
	}
}

func TestHash64Deterministic(t *testing.T) {
	if Hash64("payer-1") != Hash64("payer-1") || Hash64("payer-1") == Hash64("payer-2") {
		t.Error("Hash64")
	}
	// Golden value: changing the hash would silently change persisted
	// sketches and backtest results.
	if got := Hash64("abc"); got != 0x33ebaf9927cbc5bd {
		t.Errorf("Hash64(abc) = %#x", got)
	}
}

func FuzzHLL(f *testing.F) {
	f.Add([]byte("seed"), uint8(12))
	f.Add([]byte{}, uint8(4))
	f.Fuzz(func(t *testing.T, data []byte, p uint8) {
		h, err := NewHLL(p)
		if err != nil {
			return
		}
		exact := map[string]bool{}
		for i := 0; i+2 <= len(data); i += 2 {
			k := string(data[i : i+2])
			exact[k] = true
			h.AddHash(Hash64(k))
		}
		est := h.Estimate()
		n := len(exact)
		if n == 0 && est != 0 {
			t.Fatalf("empty sketch estimates %d", est)
		}
		if n > 0 && est == 0 {
			t.Fatalf("non-empty sketch estimates 0")
		}
		// Merge is idempotent and commutative with itself.
		cp, _ := NewHLL(p)
		_ = cp.Merge(h)
		_ = cp.Merge(h)
		if cp.Estimate() != est {
			t.Fatalf("merge changed estimate: %d != %d", cp.Estimate(), est)
		}
	})
}
