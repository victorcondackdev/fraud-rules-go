package main

import (
	"fmt"
	"math"
	"math/rand/v2"
	"sort"
	"time"

	"github.com/victorcondackdev/fraud-rules-go/engine"
)

// Config controls the synthetic population.
type Config struct {
	Seed   uint64
	Start  time.Time
	Days   int
	Payers int
	// FraudFrom is the first day (0-based) on which fraud is injected, so
	// a warm-up period before it stays clean.
	FraudFrom int
}

var brt = time.FixedZone("-03", -3*60*60)

type payer struct {
	p        engine.Payer
	rate     float64 // transfers per day
	regulars []*engine.Beneficiary
	// newDevice, when set, replaces the device from newDeviceAt on:
	// untrusted for two days, trusted afterwards (a legitimate phone change).
	newDevice   *engine.Device
	newDeviceAt time.Time
}

type generator struct {
	cfg      Config
	r        *rand.Rand
	payers   []*payer
	pool     []*engine.Beneficiary // people and merchants anyone may pay
	txs      []engine.Transaction
	nextBen  int
	atoNight time.Time
}

func generate(cfg Config) []engine.Transaction {
	g := &generator{cfg: cfg, r: rand.New(rand.NewPCG(cfg.Seed, cfg.Seed^0x9e3779b97f4a7c15))}
	g.population()
	for day := 0; day < cfg.Days; day++ {
		g.legitDay(day)
	}
	g.inject()
	sort.SliceStable(g.txs, func(i, j int) bool { return g.txs[i].Time.Before(g.txs[j].Time) })
	for i := range g.txs {
		g.txs[i].ID = fmt.Sprintf("tx-%06d", i+1)
	}
	return g.txs
}

func (g *generator) day(d int) time.Time { return g.cfg.Start.AddDate(0, 0, d) }

func (g *generator) between(lo, hi int64) int64 { return lo + g.r.Int64N(hi-lo+1) }

func (g *generator) newBeneficiary(accountAge, keyAge time.Duration, at time.Time) *engine.Beneficiary {
	g.nextBen++
	keyTypes := []string{"cpf", "phone", "email", "evp", "evp"}
	b := &engine.Beneficiary{
		ID:             fmt.Sprintf("ben-%04d", g.nextBen),
		Type:           "PF",
		Bank:           fmt.Sprintf("1000%04d", 1+g.r.IntN(12)),
		KeyType:        keyTypes[g.r.IntN(len(keyTypes))],
		AccountCreated: at.Add(-accountAge).Truncate(time.Second),
		KeyCreated:     at.Add(-keyAge).Truncate(time.Second),
	}
	if g.r.IntN(5) == 0 {
		b.Type, b.KeyType = "PJ", "cnpj"
	}
	return b
}

func (g *generator) population() {
	t0 := g.cfg.Start
	for i := 0; i < g.cfg.Payers*4; i++ {
		years := time.Duration(1+g.r.IntN(8)) * 365 * 24 * time.Hour
		g.pool = append(g.pool, g.newBeneficiary(years, years/2, t0))
	}
	for i := 0; i < g.cfg.Payers; i++ {
		created := t0.Add(-time.Duration(60+g.r.IntN(2000)) * 24 * time.Hour)
		p := &payer{rate: 0.3 + 1.2*g.r.Float64()}
		p.p = engine.Payer{
			ID:             fmt.Sprintf("acc-%04d", i+1),
			Type:           "PF",
			AccountCreated: created,
			DayLimit:       []int64{5000_00, 10000_00, 20000_00}[g.r.IntN(3)],
			NightLimit:     []int64{1000_00, 1000_00, 1000_00, 500_00, 2000_00}[g.r.IntN(5)],
			Device: engine.Device{
				ID:        fmt.Sprintf("dev-%04d", i+1),
				Trusted:   true,
				FirstSeen: created.Add(time.Duration(g.r.IntN(30)) * 24 * time.Hour),
			},
			IP: fmt.Sprintf("ip-%04d", i/2+1), // households share an IP
		}
		if g.r.IntN(10) == 0 {
			p.p.Type, p.p.DayLimit, p.p.NightLimit = "PJ", 100000_00, 10000_00
			p.rate *= 3
		}
		if g.r.IntN(12) == 0 {
			p.p.Tags = []string{"senior"}
		}
		for n := 3 + g.r.IntN(6); n > 0; n-- {
			p.regulars = append(p.regulars, g.pool[g.r.IntN(len(g.pool))])
		}
		if g.r.IntN(12) == 0 {
			d := g.r.IntN(g.cfg.Days)
			p.newDeviceAt = g.day(d).Add(time.Duration(9+g.r.IntN(10)) * time.Hour)
			p.newDevice = &engine.Device{ID: fmt.Sprintf("dev-%04d-b", i+1), FirstSeen: p.newDeviceAt}
		}
		g.payers = append(g.payers, p)
	}
}

func (g *generator) device(p *payer, at time.Time) engine.Device {
	if p.newDevice == nil || at.Before(p.newDeviceAt) {
		return p.p.Device
	}
	d := *p.newDevice
	d.Trusted = at.Sub(p.newDeviceAt) > 48*time.Hour
	return d
}

// hourWeights is a diurnal profile: little activity between 0h and 6h.
var hourWeights = [24]int{1, 1, 1, 1, 1, 2, 4, 8, 12, 14, 14, 14, 15, 14, 13, 13, 13, 13, 14, 14, 11, 8, 5, 3}

func (g *generator) hour() int {
	total := 0
	for _, w := range hourWeights {
		total += w
	}
	n := g.r.IntN(total)
	for h, w := range hourWeights {
		if n < w {
			return h
		}
		n -= w
	}
	return 12
}

func (g *generator) poisson(lambda float64) int {
	l, k, p := math.Exp(-lambda), 0, 1.0
	for {
		p *= g.r.Float64()
		if p <= l {
			return k
		}
		k++
	}
}

// legitAmount is log-normal around R$ 80,00, capped below the limit that
// applies, with the occasional rent-sized payment.
func (g *generator) legitAmount(p *payer, night bool) int64 {
	a := int64(math.Exp(math.Log(8000) + 1.1*g.r.NormFloat64()))
	if g.r.IntN(40) == 0 {
		a = g.between(1500_00, 4000_00)
	}
	limit := p.p.DayLimit
	if night {
		limit = p.p.NightLimit
	}
	if g.r.IntN(150) == 0 {
		return limit * g.between(90, 100) / 100 // legit, just below the limit
	}
	return max(100, min(a, limit))
}

func (g *generator) emit(p *payer, ben *engine.Beneficiary, at time.Time, amount int64, label, pattern string) {
	tx := engine.Transaction{
		Time:        at.Truncate(time.Second),
		Amount:      amount,
		Channel:     "app",
		Initiation:  "key",
		Payer:       p.p,
		Beneficiary: *ben,
		Label:       label,
		Pattern:     pattern,
	}
	tx.Payer.Device = g.device(p, at)
	switch n := g.r.IntN(100); {
	case n < 10:
		tx.Channel = "web"
	case n < 13 || (p.p.Type == "PJ" && n < 40):
		tx.Channel = "api"
	}
	switch n := g.r.IntN(100); {
	case n < 20:
		tx.Initiation = "qr_dynamic"
	case n < 30:
		tx.Initiation = "qr_static"
	case n < 40:
		tx.Initiation = "manual"
	}
	g.txs = append(g.txs, tx)
}

func (g *generator) legitDay(d int) {
	for _, p := range g.payers {
		for n := g.poisson(p.rate); n > 0; n-- {
			at := g.day(d).Add(time.Duration(g.hour())*time.Hour + time.Duration(g.r.IntN(3600))*time.Second)
			h := at.Hour()
			night := h >= 20 || h < 6
			ben := p.regulars[g.r.IntN(len(p.regulars))]
			if g.r.IntN(100) < 15 {
				ben = g.pool[g.r.IntN(len(g.pool))]
				if g.r.IntN(4) == 0 {
					// A recently opened account or fresh key: new sellers,
					// freelancers, someone who just switched banks.
					ben = g.newBeneficiary(time.Duration(1+g.r.IntN(60))*24*time.Hour, time.Duration(1+g.r.IntN(240))*time.Hour, at)
					g.pool = append(g.pool, ben)
				}
				if g.r.IntN(3) == 0 {
					p.regulars = append(p.regulars, ben)
				}
			}
			g.emit(p, ben, at, g.legitAmount(p, night), "legit", "")
		}
		// Bill splitting: a legitimate burst to several friends.
		if g.r.IntN(150) == 0 {
			at := g.day(d).Add(time.Duration(19+g.r.IntN(3)) * time.Hour)
			for n := 3 + g.r.IntN(3); n > 0; n-- {
				at = at.Add(time.Duration(30+g.r.IntN(150)) * time.Second)
				g.emit(p, g.pool[g.r.IntN(len(g.pool))], at, g.between(20_00, 150_00), "legit", "")
			}
		}
	}
}

func (g *generator) victim() *payer {
	for {
		p := g.payers[g.r.IntN(len(g.payers))]
		if p.p.Type == "PF" {
			return p
		}
	}
}

func (g *generator) fraudDay() int {
	return g.cfg.FraudFrom + g.r.IntN(max(1, g.cfg.Days-g.cfg.FraudFrom))
}

// mule returns a recently opened receiving account with a fresh Pix key.
func (g *generator) mule(at time.Time) *engine.Beneficiary {
	b := g.newBeneficiary(time.Duration(3+g.r.IntN(20))*24*time.Hour, time.Duration(1+g.r.IntN(60))*time.Hour, at)
	if g.r.IntN(5) < 2 {
		// A rented ("laranja") account: old account, old key.
		b = g.newBeneficiary(time.Duration(365+g.r.IntN(1500))*24*time.Hour, time.Duration(30+g.r.IntN(400))*24*time.Hour, at)
	}
	b.Type, b.KeyType = "PF", "evp"
	return b
}

func (g *generator) inject() {
	n := max(1, g.cfg.Payers/10)
	for i := 0; i < n; i++ {
		g.atoBurst(i)
	}
	for i := 0; i < max(1, n*2/3); i++ {
		g.muleFanOut(i)
	}
	for i := 0; i < max(1, n/2); i++ {
		g.scamFanIn()
	}
	for i := 0; i < max(1, n*2/3); i++ {
		g.structuring()
	}
}

// atoBurst: account takeover at night from a brand-new device, draining
// the account into one to three mule accounts within minutes.
//
// Incidents come in groups of three sharing one device and night (one
// fraudster working through several accounts), and one in four uses the
// victim's own trusted phone through a remote-access trojan.
func (g *generator) atoBurst(i int) {
	p := g.victim()
	if i%3 == 0 {
		g.atoNight = g.day(g.fraudDay()).Add(time.Duration(30+g.r.IntN(180)) * time.Minute)
	}
	at := g.atoNight.Add(time.Duration(i%3*40+g.r.IntN(20)) * time.Minute)
	stolen := *p
	stolen.newDevice = nil
	if g.r.IntN(4) != 0 {
		stolen.p.Device = engine.Device{ID: fmt.Sprintf("dev-ato-%02d", i/3+1), FirstSeen: at.Add(-time.Duration(2+g.r.IntN(18)) * time.Minute)}
		stolen.p.IP = fmt.Sprintf("ip-ato-%02d", i/3+1)
	}
	mules := []*engine.Beneficiary{g.mule(at)}
	for n := g.r.IntN(3); n > 0; n-- {
		mules = append(mules, g.mule(at))
	}
	for n := 3 + g.r.IntN(4); n > 0; n-- {
		amount := p.p.NightLimit * g.between(60, 99) / 100
		if g.r.IntN(5) == 0 {
			amount = p.p.NightLimit + g.between(1_00, 500_00) // probing the limit
		}
		g.emit(&stolen, mules[g.r.IntN(len(mules))], at, amount, "fraud", "ato_night_burst")
		at = at.Add(time.Duration(40+g.r.IntN(240)) * time.Second)
	}
}

// muleFanOut: a recently opened account receives scam proceeds and
// spreads them over many fresh accounts in under an hour.
func (g *generator) muleFanOut(i int) {
	at := g.day(g.fraudDay()).Add(time.Duration(9*60+g.r.IntN(10*60)) * time.Minute)
	created := at.Add(-time.Duration(2+g.r.IntN(12)) * 24 * time.Hour)
	m := &payer{p: engine.Payer{
		ID: fmt.Sprintf("acc-mule-%02d", i+1), Type: "PF", AccountCreated: created,
		DayLimit: 5000_00, NightLimit: 1000_00,
		Device: engine.Device{ID: fmt.Sprintf("dev-mule-%02d", i+1), Trusted: true, FirstSeen: created},
		IP:     fmt.Sprintf("ip-mule-%02d", i%3+1), // mule herders reuse infrastructure
	}}
	for n := 6 + g.r.IntN(7); n > 0; n-- {
		g.emit(m, g.mule(at), at, g.between(300_00, 1500_00), "fraud", "mule_fan_out")
		at = at.Add(time.Duration(60+g.r.IntN(300)) * time.Second)
	}
}

// scamFanIn: social-engineering victims (fake sales, "golpe do Pix")
// pay one collection account; reports start arriving as the scam runs.
func (g *generator) scamFanIn() {
	at := g.day(g.fraudDay()).Add(time.Duration(10*60+g.r.IntN(8*60)) * time.Minute)
	collector := g.mule(at)
	collector.AccountCreated = at.Add(-time.Duration(5+g.r.IntN(40)) * 24 * time.Hour)
	victims := g.r.Perm(len(g.payers))
	for k, n := 0, 6+g.r.IntN(7); k < n; k++ {
		p := g.payers[victims[k]]
		if k == n/2 || k == n-2 {
			collector.FraudMarks++ // MED reports from earlier victims land
		}
		g.emit(p, collector, at, g.between(150_00, 2500_00), "fraud", "scam_fan_in")
		at = at.Add(time.Duration(3+g.r.IntN(12)) * time.Minute)
	}
}

// structuring: a coerced or complicit customer splits a large amount into
// night transfers just below the nighttime limit.
func (g *generator) structuring() {
	p := g.victim()
	at := g.day(g.fraudDay()).Add(time.Duration(20*60+g.r.IntN(150)) * time.Minute)
	targets := []*engine.Beneficiary{g.mule(at)}
	if g.r.IntN(2) == 0 {
		targets = append(targets, g.mule(at))
	}
	for n := 3 + g.r.IntN(3); n > 0; n-- {
		g.emit(p, targets[g.r.IntN(len(targets))], at, p.p.NightLimit*g.between(900, 999)/1000, "fraud", "structuring")
		at = at.Add(time.Duration(8+g.r.IntN(25)) * time.Minute)
	}
}
