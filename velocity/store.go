package velocity

import (
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"
)

// Event is one observation recorded under a key, typically a settled
// transfer seen from one of its parties.
type Event struct {
	Key          string
	Time         time.Time
	Amount       int64  // cents; summed by [Stats.Sum]
	Counterparty string // counted by [Stats.Distinct]; empty is not counted
}

// Stats is the answer to a window query.
type Stats struct {
	Count    int64
	Sum      int64
	Distinct int64
	// Exact is false when Distinct is a HyperLogLog estimate.
	Exact bool
}

// Tier is one resolution level: events are aggregated into buckets of
// Granularity, and buckets older than Horizon are discarded.
type Tier struct {
	Granularity time.Duration
	Horizon     time.Duration
}

// Config configures a [Store]. The zero value selects the defaults noted on
// each field.
type Config struct {
	// Tiers, from finest to coarsest. A query uses the finest tier able to
	// answer it. Default: 1m buckets for 3h, 1h buckets for 7d+1h, 1d
	// buckets for 181d.
	Tiers []Tier
	// Shards is the number of independently locked partitions, rounded up
	// to a power of two. Default 64.
	Shards int
	// MaxKeys bounds memory: when a shard is full its least recently
	// written key is evicted. Default 1<<20 keys in total; negative means
	// unbounded.
	MaxKeys int
	// TTL evicts keys with no event newer than now-TTL during [Store.Sweep].
	// Default: the coarsest horizon.
	TTL time.Duration
	// ExactDistinct is the number of distinct counterparties a bucket keeps
	// exactly before switching to a HyperLogLog sketch. Default 32.
	ExactDistinct int
	// HLLPrecision is the sketch precision p (2^p registers). Default 12,
	// a relative standard error of about 1.6%.
	HLLPrecision uint8
}

// DefaultTiers is the tier layout used when [Config.Tiers] is empty.
var DefaultTiers = []Tier{
	{Granularity: time.Minute, Horizon: 3 * time.Hour},
	{Granularity: time.Hour, Horizon: 7*24*time.Hour + time.Hour},
	{Granularity: 24 * time.Hour, Horizon: 181 * 24 * time.Hour},
}

// Errors returned by [New] and [Store.Query].
var (
	ErrConfig         = errors.New("velocity: invalid config")
	ErrWindowTooLarge = errors.New("velocity: window exceeds horizon")
	ErrWindow         = errors.New("velocity: window must be positive")
)

// Store keeps sliding-window aggregates per key. It is safe for
// concurrent use.
//
// # Window semantics
//
// Time is bucketed per tier. A query for window w at instant now covers
// every bucket that overlaps [now-w, now]: from the start of the bucket
// containing now-w through the end of the bucket containing now. It
// therefore never misses an event inside the window and may include up to
// one granularity of older events, the conservative direction for fraud
// limits. Use finer tiers where that slack matters.
//
// Time is event time, not wall-clock time, so replaying history gives the
// same answers as production did. Events older than a tier's horizon
// relative to the newest event of the key are dropped from that tier.
type Store struct {
	tiers         []tier
	shards        []shard
	mask          uint64
	perShard      int
	ttl           time.Duration
	exactDistinct int
	hllP          uint8
	scratch       sync.Pool
}

type tier struct {
	g, horizon int64 // nanoseconds
	n          int64 // buckets kept
}

// New validates cfg and returns an empty store.
func New(cfg Config) (*Store, error) {
	if len(cfg.Tiers) == 0 {
		cfg.Tiers = DefaultTiers
	}
	if cfg.Shards <= 0 {
		cfg.Shards = 64
	}
	if cfg.MaxKeys == 0 {
		cfg.MaxKeys = 1 << 20
	}
	if cfg.ExactDistinct <= 0 {
		cfg.ExactDistinct = 32
	}
	if cfg.HLLPrecision == 0 {
		cfg.HLLPrecision = 12
	}
	if cfg.HLLPrecision < MinPrecision || cfg.HLLPrecision > MaxPrecision {
		return nil, fmt.Errorf("%w: HLL precision %d", ErrConfig, cfg.HLLPrecision)
	}
	s := &Store{exactDistinct: cfg.ExactDistinct, hllP: cfg.HLLPrecision}
	for i, t := range cfg.Tiers {
		if t.Granularity <= 0 || t.Horizon < 2*t.Granularity || t.Horizon%t.Granularity != 0 {
			return nil, fmt.Errorf("%w: tier %d: horizon must be a multiple (>= 2) of a positive granularity", ErrConfig, i)
		}
		if i > 0 && (t.Granularity <= cfg.Tiers[i-1].Granularity || t.Horizon <= cfg.Tiers[i-1].Horizon) {
			return nil, fmt.Errorf("%w: tiers must go from finest to coarsest", ErrConfig)
		}
		if t.Horizon/t.Granularity > 1<<16 {
			return nil, fmt.Errorf("%w: tier %d keeps more than 65536 buckets", ErrConfig, i)
		}
		s.tiers = append(s.tiers, tier{g: int64(t.Granularity), horizon: int64(t.Horizon), n: int64(t.Horizon / t.Granularity)})
	}
	s.ttl = cfg.TTL
	if s.ttl <= 0 {
		s.ttl = cfg.Tiers[len(cfg.Tiers)-1].Horizon
	}
	n := 1
	for n < cfg.Shards {
		n <<= 1
	}
	s.shards = make([]shard, n)
	s.mask = uint64(n - 1)
	for i := range s.shards {
		s.shards[i].keys = make(map[string]*series)
	}
	s.perShard = -1
	if cfg.MaxKeys > 0 {
		s.perShard = max(1, (cfg.MaxKeys+n-1)/n)
	}
	s.scratch.New = func() any { b := make([]uint64, 0, 256); return &b }
	return s, nil
}

// MaxWindow is the longest window [Store.Query] accepts.
func (s *Store) MaxWindow() time.Duration {
	t := s.tiers[len(s.tiers)-1]
	return time.Duration(t.horizon - t.g)
}

// Granularity returns the bucket size used to answer a query for window.
func (s *Store) Granularity(window time.Duration) (time.Duration, error) {
	i, err := s.tierFor(window)
	if err != nil {
		return 0, err
	}
	return time.Duration(s.tiers[i].g), nil
}

func (s *Store) tierFor(window time.Duration) (int, error) {
	if window <= 0 {
		return 0, ErrWindow
	}
	for i := range s.tiers {
		if t := &s.tiers[i]; int64(window) <= t.horizon-t.g {
			return i, nil
		}
	}
	return 0, fmt.Errorf("%w: %s > %s", ErrWindowTooLarge, window, s.MaxWindow())
}

func (s *Store) shardFor(key string) *shard { return &s.shards[Hash64(key)&s.mask] }

// Record adds e to every tier of its key.
func (s *Store) Record(e Event) {
	ts := e.Time.UnixNano()
	var ch uint64
	if e.Counterparty != "" {
		ch = Hash64(e.Counterparty)
	}
	sh := s.shardFor(e.Key)
	sh.mu.Lock()
	defer sh.mu.Unlock()
	sr := sh.keys[e.Key]
	if sr == nil {
		if s.perShard > 0 && len(sh.keys) >= s.perShard {
			sh.evict(sh.lru.root.prev)
		}
		sr = &series{key: e.Key, rings: make([]ring, len(s.tiers))}
		sh.keys[e.Key] = sr
		sh.lru.pushFront(sr)
	} else {
		sh.lru.moveToFront(sr)
	}
	if ts > sr.last {
		sr.last = ts
	}
	for i := range s.tiers {
		s.recordTier(&s.tiers[i], &sr.rings[i], ts, e.Amount, ch, e.Counterparty != "")
	}
}

// countUnique counts distinct values in hs with an open-addressing table
// stored after hs in the pooled scratch buffer; it is several times faster
// than sorting for the few hundred hashes a busy window holds.
func countUnique(hs []uint64, scratch *[]uint64) int64 {
	if len(hs) <= 8 {
		n := int64(0)
		for i, h := range hs {
			if !slices.Contains(hs[:i], h) {
				n++
			}
		}
		return n
	}
	size := 16
	for size < 2*len(hs) {
		size <<= 1
	}
	buf := *scratch
	if cap(buf) < len(hs)+size {
		buf = append(buf[:cap(buf)], make([]uint64, len(hs)+size-cap(buf))...)
		*scratch = buf
	}
	table := buf[len(hs) : len(hs)+size]
	clear(table)
	mask := uint64(size - 1)
	var n int64
	zero := false
	for _, h := range hs {
		if h == 0 { // 0 marks empty slots
			if !zero {
				zero = true
				n++
			}
			continue
		}
		for i := h & mask; ; i = (i + 1) & mask {
			if table[i] == 0 {
				table[i] = h
				n++
				break
			}
			if table[i] == h {
				break
			}
		}
	}
	return n
}

func floorDiv(a, b int64) int64 {
	q := a / b
	if (a%b != 0) && (a < 0) {
		q--
	}
	return q
}

func (s *Store) recordTier(t *tier, r *ring, ts, amount int64, ch uint64, hasCP bool) {
	epoch := floorDiv(ts, t.g)
	var b *bucket
	switch {
	case r.len() == 0 || epoch > r.back().epoch:
		b = r.pushBack(epoch)
		for r.front().epoch <= epoch-t.n {
			r.popFront()
		}
	case epoch == r.back().epoch:
		b = r.back()
	case epoch <= r.back().epoch-t.n:
		return // too late for this tier
	default:
		b = r.find(epoch)
	}
	b.count++
	b.sum += amount
	if hasCP {
		b.addDistinct(ch, s.exactDistinct, s.hllP)
	}
}

// Query aggregates key over the trailing window ending at now.
func (s *Store) Query(key string, window time.Duration, now time.Time) (Stats, error) {
	return s.query(key, window, now, true)
}

// QueryCounts is like [Store.Query] without the distinct count, which is
// the expensive part of a query over busy keys. Distinct is 0 and Exact is
// true in the result.
func (s *Store) QueryCounts(key string, window time.Duration, now time.Time) (Stats, error) {
	return s.query(key, window, now, false)
}

func (s *Store) query(key string, window time.Duration, now time.Time, distinct bool) (Stats, error) {
	ti, err := s.tierFor(window)
	if err != nil {
		return Stats{}, err
	}
	t := &s.tiers[ti]
	nowNs := now.UnixNano()
	first := floorDiv(nowNs-int64(window), t.g)
	last := floorDiv(nowNs, t.g)

	sh := s.shardFor(key)
	sh.mu.Lock()
	defer sh.mu.Unlock()
	sr := sh.keys[key]
	st := Stats{Exact: true}
	if sr == nil {
		return st, nil
	}
	r := &sr.rings[ti]
	sp := s.scratch.Get().(*[]uint64)
	hashes := (*sp)[:0]
	var sketch *HLL
	for i := r.len() - 1; i >= 0; i-- {
		b := r.at(i)
		if b.epoch > last {
			continue
		}
		if b.epoch < first {
			break
		}
		st.Count += b.count
		st.Sum += b.sum
		if !distinct {
			continue
		}
		if b.hll != nil {
			if sketch == nil {
				sketch, _ = NewHLL(s.hllP)
			}
			_ = sketch.Merge(b.hll)
		} else {
			hashes = append(hashes, b.set...)
		}
	}
	if sketch != nil {
		for _, h := range hashes {
			sketch.AddHash(h)
		}
		st.Distinct = int64(sketch.Estimate())
		st.Exact = false
	} else if len(hashes) > 0 {
		*sp = hashes
		st.Distinct = countUnique(hashes, sp)
	}
	*sp = (*sp)[:0]
	s.scratch.Put(sp)
	return st, nil
}

// Sweep evicts keys whose newest event is older than now minus the TTL and
// returns how many were removed. Call it periodically; Record and Query
// never sweep on their own, which keeps their latency flat.
func (s *Store) Sweep(now time.Time) int {
	cutoff := now.Add(-s.ttl).UnixNano()
	n := 0
	for i := range s.shards {
		sh := &s.shards[i]
		sh.mu.Lock()
		for _, sr := range sh.keys {
			if sr.last < cutoff {
				sh.evict(sr)
				n++
			}
		}
		sh.mu.Unlock()
	}
	return n
}

// Len returns the number of keys held.
func (s *Store) Len() int {
	n := 0
	for i := range s.shards {
		sh := &s.shards[i]
		sh.mu.Lock()
		n += len(sh.keys)
		sh.mu.Unlock()
	}
	return n
}

// Evictions returns the number of keys evicted so far by the MaxKeys bound
// or by [Store.Sweep].
func (s *Store) Evictions() int64 {
	var n int64
	for i := range s.shards {
		sh := &s.shards[i]
		sh.mu.Lock()
		n += sh.evictions
		sh.mu.Unlock()
	}
	return n
}

type shard struct {
	mu        sync.Mutex
	keys      map[string]*series
	lru       lruList
	evictions int64
	_         [40]byte // keep hot shards on separate cache lines
}

func (sh *shard) evict(sr *series) {
	if sr == nil || sr == &sh.lru.root {
		return
	}
	sh.lru.remove(sr)
	delete(sh.keys, sr.key)
	sh.evictions++
}

// series is the per-key state: one ring per tier plus LRU links.
type series struct {
	key        string
	last       int64
	rings      []ring
	prev, next *series
}

// lruList is an intrusive circular doubly linked list with a sentinel;
// the front is the most recently written key.
type lruList struct{ root series }

func (l *lruList) init() {
	if l.root.next == nil {
		l.root.next, l.root.prev = &l.root, &l.root
	}
}

func (l *lruList) pushFront(s *series) {
	l.init()
	s.prev, s.next = &l.root, l.root.next
	l.root.next.prev = s
	l.root.next = s
}

func (l *lruList) remove(s *series) {
	s.prev.next = s.next
	s.next.prev = s.prev
	s.prev, s.next = nil, nil
}

func (l *lruList) moveToFront(s *series) {
	if l.root.next == s {
		return
	}
	l.remove(s)
	l.pushFront(s)
}

// bucket aggregates the events of one time slot.
type bucket struct {
	epoch int64
	count int64
	sum   int64
	set   []uint64 // exact counterparty hashes, unsorted, unique
	hll   *HLL     // replaces set past ExactDistinct entries
}

func (b *bucket) addDistinct(h uint64, limit int, p uint8) {
	if b.hll != nil {
		b.hll.AddHash(h)
		return
	}
	if slices.Contains(b.set, h) {
		return
	}
	if len(b.set) < limit {
		b.set = append(b.set, h)
		return
	}
	b.hll, _ = NewHLL(p)
	for _, x := range b.set {
		b.hll.AddHash(x)
	}
	b.hll.AddHash(h)
	b.set = nil
}

// ring is a growable circular buffer of buckets ordered by epoch. Only
// non-empty buckets are stored, so a quiet key costs a few dozen bytes
// instead of a full ring per tier.
type ring struct {
	buf  []bucket
	head int
	n    int
}

func (r *ring) len() int         { return r.n }
func (r *ring) at(i int) *bucket { return &r.buf[(r.head+i)&(len(r.buf)-1)] }
func (r *ring) front() *bucket   { return r.at(0) }
func (r *ring) back() *bucket    { return r.at(r.n - 1) }

func (r *ring) grow() {
	size := max(2, 2*len(r.buf))
	nb := make([]bucket, size)
	for i := 0; i < r.n; i++ {
		nb[i] = *r.at(i)
	}
	r.buf, r.head = nb, 0
}

func (r *ring) pushBack(epoch int64) *bucket {
	if r.n == len(r.buf) {
		r.grow()
	}
	r.n++
	b := r.back()
	*b = bucket{epoch: epoch}
	return b
}

func (r *ring) popFront() {
	*r.front() = bucket{}
	r.head = (r.head + 1) & (len(r.buf) - 1)
	r.n--
}

// find returns the bucket for epoch, inserting it in order if missing.
// Out-of-order events are rare, so the O(n) shift is acceptable.
func (r *ring) find(epoch int64) *bucket {
	lo, hi := 0, r.n
	for lo < hi {
		m := (lo + hi) / 2
		if r.at(m).epoch < epoch {
			lo = m + 1
		} else {
			hi = m
		}
	}
	if lo < r.n && r.at(lo).epoch == epoch {
		return r.at(lo)
	}
	r.pushBack(0)
	for i := r.n - 1; i > lo; i-- {
		*r.at(i) = *r.at(i - 1)
	}
	b := r.at(lo)
	*b = bucket{epoch: epoch}
	return b
}
