package velocity

import (
	"errors"
	"math"
	"math/bits"
	"sort"
)

// HLL is a HyperLogLog cardinality sketch in the spirit of HyperLogLog++
// (Heule, Nunkesser and Hall, 2013), minus the empirical bias tables:
//
//   - 64-bit hashes, so there is no large-range correction and no
//     saturation below 2^64 distinct items;
//   - a sparse representation (sorted register/rank pairs) while few
//     registers are set, converted to the dense array of 2^p bytes once it
//     would stop saving memory;
//   - linear counting for small cardinalities, switching to the raw
//     HyperLogLog estimate above 2.5·m as in the original paper.
//
// The relative standard error is about 1.04/√m with m = 2^p registers:
// 3.25% at p=10, 1.63% at p=12, 0.81% at p=14. Below ~2.5·m linear counting
// is used and the error is lower. The tests check every cardinality from
// 1 to 10^6 against a 4σ bound.
//
// The zero value is not usable; call [NewHLL]. An HLL is not safe for
// concurrent use.
type HLL struct {
	p      uint8
	sparse []uint32 // sorted by register index; idx<<6 | rank
	dense  []uint8  // nil while sparse
}

// Precision limits accepted by [NewHLL].
const (
	MinPrecision = 4
	MaxPrecision = 16
)

// ErrPrecision reports an HLL precision outside [MinPrecision, MaxPrecision]
// or a merge of sketches with different precisions.
var ErrPrecision = errors.New("velocity: invalid HLL precision")

// NewHLL returns an empty sketch with 2^p registers.
func NewHLL(p uint8) (*HLL, error) {
	if p < MinPrecision || p > MaxPrecision {
		return nil, ErrPrecision
	}
	return &HLL{p: p}, nil
}

// Precision returns p.
func (h *HLL) Precision() uint8 { return h.p }

// StdError returns the theoretical relative standard error 1.04/√m.
func (h *HLL) StdError() float64 { return 1.04 / math.Sqrt(float64(uint64(1)<<h.p)) }

func (h *HLL) sparseLimit() int { return (1 << h.p) / 8 } // 4 bytes each: half the dense size

// AddHash adds an item by its 64-bit hash. Use a well-mixed hash such as
// [Hash64]; correlated low bits ruin the estimate.
func (h *HLL) AddHash(x uint64) {
	idx := uint32(x >> (64 - h.p))
	w := x<<h.p | 1<<(h.p-1) // guard bit bounds the rank at 64-p+1
	rank := uint8(bits.LeadingZeros64(w)) + 1
	h.set(idx, rank)
}

func (h *HLL) set(idx uint32, rank uint8) {
	if h.dense != nil {
		if rank > h.dense[idx] {
			h.dense[idx] = rank
		}
		return
	}
	i := sort.Search(len(h.sparse), func(i int) bool { return h.sparse[i]>>6 >= idx })
	if i < len(h.sparse) && h.sparse[i]>>6 == idx {
		if uint8(h.sparse[i]&63) < rank {
			h.sparse[i] = idx<<6 | uint32(rank)
		}
		return
	}
	if len(h.sparse) >= h.sparseLimit() {
		h.toDense()
		h.dense[idx] = max(h.dense[idx], rank)
		return
	}
	h.sparse = append(h.sparse, 0)
	copy(h.sparse[i+1:], h.sparse[i:])
	h.sparse[i] = idx<<6 | uint32(rank)
}

func (h *HLL) toDense() {
	h.dense = make([]uint8, 1<<h.p)
	for _, e := range h.sparse {
		h.dense[e>>6] = uint8(e & 63)
	}
	h.sparse = nil
}

// Merge folds o into h, so that h estimates the union. Both sketches must
// have the same precision.
func (h *HLL) Merge(o *HLL) error {
	if o.p != h.p {
		return ErrPrecision
	}
	if o.dense != nil {
		if h.dense == nil {
			h.toDense()
		}
		for i, r := range o.dense {
			if r > h.dense[i] {
				h.dense[i] = r
			}
		}
		return nil
	}
	for _, e := range o.sparse {
		h.set(e>>6, uint8(e&63))
	}
	return nil
}

// Reset empties the sketch, keeping its precision.
func (h *HLL) Reset() { h.sparse, h.dense = h.sparse[:0], nil }

// Estimate returns the estimated number of distinct hashes added.
func (h *HLL) Estimate() uint64 {
	m := float64(uint64(1) << h.p)
	if h.dense == nil {
		// Each sparse entry is a non-zero register: linear counting is the
		// right estimator in this range.
		zeros := m - float64(len(h.sparse))
		return uint64(math.Round(m * math.Log(m/zeros)))
	}
	var sum float64
	zeros := 0
	for _, r := range h.dense {
		sum += 1 / float64(uint64(1)<<r)
		if r == 0 {
			zeros++
		}
	}
	e := alpha(m) * m * m / sum
	if e <= 2.5*m && zeros > 0 {
		return uint64(math.Round(m * math.Log(m/float64(zeros))))
	}
	return uint64(math.Round(e))
}

func alpha(m float64) float64 {
	switch m {
	case 16:
		return 0.673
	case 32:
		return 0.697
	case 64:
		return 0.709
	}
	return 0.7213 / (1 + 1.079/m)
}

// SizeBytes returns the approximate memory used by the registers.
func (h *HLL) SizeBytes() int {
	if h.dense != nil {
		return len(h.dense)
	}
	return 4 * cap(h.sparse)
}

// Hash64 is FNV-1a followed by the MurmurHash3 64-bit finalizer. It is
// deterministic across processes (unlike hash/maphash), which keeps
// backtests reproducible, and the finalizer gives the avalanche HLL needs.
func Hash64(s string) uint64 {
	h := uint64(14695981039346656037)
	for i := 0; i < len(s); i++ {
		h ^= uint64(s[i])
		h *= 1099511628211
	}
	h ^= h >> 33
	h *= 0xff51afd7ed558ccd
	h ^= h >> 33
	h *= 0xc4ceb9fe1a85ec53
	h ^= h >> 33
	return h
}
