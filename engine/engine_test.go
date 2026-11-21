package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/victorcondackdev/fraud-rules-go/expr"
	"github.com/victorcondackdev/fraud-rules-go/velocity"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

func bt(b bool) *bool { return &b }

// 23:30 in São Paulo.
var night = time.Date(2026, 11, 21, 23, 30, 0, 0, saoPaulo)

func baseTx() *Transaction {
	return &Transaction{
		ID: "tx-1", Time: night, Amount: 500_00, Channel: "app",
		Payer: Payer{ID: "acc-1", Type: "PF", DayLimit: 5000_00, NightLimit: 1000_00,
			AccountCreated: night.AddDate(-2, 0, 0),
			Device:         Device{ID: "dev-1", Trusted: true, FirstSeen: night.AddDate(-1, 0, 0)}, IP: "ip-1"},
		Beneficiary: Beneficiary{ID: "ben-1", Type: "PF", KeyCreated: night.AddDate(-1, 0, 0), AccountCreated: night.AddDate(-3, 0, 0)},
	}
}

func newTestEngine(t testing.TB, rs Ruleset, opts Options) (*Engine, *velocity.Store) {
	t.Helper()
	store, err := velocity.New(velocity.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if opts.Logger == nil {
		opts.Logger = quiet
	}
	e := New(store, opts)
	c, err := Compile(rs, e.CompileOptions())
	if err != nil {
		t.Fatal(err)
	}
	e.Load(c)
	return e, store
}

func rules(rs ...Rule) Ruleset { return Ruleset{Name: "test", Version: "1", Rules: rs} }

func ids(hs []Hit) string {
	var s []string
	for _, h := range hs {
		s = append(s, h.RuleID)
	}
	return strings.Join(s, ",")
}

func TestAggregation(t *testing.T) {
	rs := rules(
		Rule{ID: "review", Expr: `tx.amount > 100_00`, Action: Review, Score: 10},
		Rule{ID: "stepup", Expr: `isNight(tx.time)`, Action: StepUp, Score: 20},
		Rule{ID: "hold.short", Expr: `payer.type == "PF"`, Action: Hold, HoldFor: Duration(10 * time.Minute), Score: 5},
		Rule{ID: "hold.long", Expr: `tx.amount >= 500_00`, Action: Hold, HoldFor: Duration(time.Hour), Score: 5},
		Rule{ID: "nomatch", Expr: `tx.amount > 1_000_000_00`, Action: Block, Score: 100},
		Rule{ID: "disabled", Expr: `true`, Action: Block, Score: 100, Enabled: bt(false)},
		Rule{ID: "offset", Expr: `payer.device.trusted`, Action: Allow, Score: -15},
	)
	e, _ := newTestEngine(t, rs, Options{})
	d, err := e.Evaluate(context.Background(), baseTx())
	if err != nil {
		t.Fatal(err)
	}
	if d.Action != Hold || d.HoldFor != Duration(time.Hour) || d.Score != 25 {
		t.Errorf("outcome = %+v", d.Outcome)
	}
	if got := ids(d.Hits); got != "review,stepup,hold.short,hold.long,offset" {
		t.Errorf("hits = %s", got)
	}
	if len(d.Reasons) != 2 || d.Reasons[0] != "rule hold.short → HOLD" {
		t.Errorf("reasons = %q", d.Reasons)
	}
	if d.Ruleset != "test@1" || d.Revision != 1 || d.Degraded || d.Shadow != nil {
		t.Errorf("metadata = %+v", d)
	}
}

func TestThresholdEscalation(t *testing.T) {
	rs := rules(
		Rule{ID: "a", Expr: `true`, Action: Review, Score: 30},
		Rule{ID: "b", Expr: `true`, Action: Review, Score: 45},
	)
	for _, tt := range []struct {
		th   Thresholds
		want Action
		hold Duration
	}{
		{Thresholds{}, Review, 0},
		{Thresholds{Review: 10, StepUp: 70, Block: 100}, StepUp, 0},
		{Thresholds{Review: 10, StepUp: 20, Block: 75}, Block, 0},
		{Thresholds{Review: 80}, Review, 0},
	} {
		rs.Thresholds = tt.th
		e, _ := newTestEngine(t, rs, Options{})
		d, _ := e.Evaluate(context.Background(), baseTx())
		if d.Action != tt.want {
			t.Errorf("%+v: action %s, want %s (%q)", tt.th, d.Action, tt.want, d.Reasons)
		}
		if tt.want == Block && !strings.Contains(d.Reasons[0], "score 75 ≥ block threshold 75") {
			t.Errorf("reasons = %q", d.Reasons)
		}
	}
	// Scores below every threshold, nothing above ALLOW.
	e, _ := newTestEngine(t, rules(Rule{ID: "a", Expr: `true`, Action: Allow, Score: 5}), Options{})
	d, _ := e.Evaluate(context.Background(), baseTx())
	if d.Action != Allow || d.Reasons[0] != "score 5 below thresholds" {
		t.Errorf("allow reasons = %q", d.Reasons)
	}
	e, _ = newTestEngine(t, rules(Rule{ID: "a", Expr: `false`, Action: Block}), Options{})
	d, _ = e.Evaluate(context.Background(), baseTx())
	if d.Action != Allow || d.Reasons[0] != "no enforced rule matched" || len(d.Hits) != 0 {
		t.Errorf("no match = %+v", d)
	}
}

func TestShadowMode(t *testing.T) {
	rs := rules(
		Rule{ID: "enforced", Expr: `tx.amount > 100_00`, Action: Review, Score: 10},
		Rule{ID: "candidate", Expr: `isNight(tx.time)`, Action: Block, Score: 50, Mode: Shadow},
		Rule{ID: "candidate.miss", Expr: `false`, Action: Block, Mode: Shadow},
	)
	e, _ := newTestEngine(t, rs, Options{})
	d, _ := e.Evaluate(context.Background(), baseTx())
	if d.Action != Review || d.Score != 10 {
		t.Errorf("shadow rule changed the decision: %+v", d.Outcome)
	}
	if ids(d.ShadowHits) != "candidate" || d.Shadow == nil || d.Shadow.Action != Block || d.Shadow.Score != 60 {
		t.Errorf("shadow = %+v %+v", d.ShadowHits, d.Shadow)
	}
	if d.ShadowHits[0].Mode != Shadow {
		t.Error("shadow hit mode")
	}
}

func TestExplanation(t *testing.T) {
	rs := rules(Rule{ID: "night.limit", Expr: `isNight(tx.time) && tx.amount > payer.night_limit && sum(payer.transfers, 1h) >= 0 && age(payer.device.first_seen) > 1d`, Action: Block})
	e, _ := newTestEngine(t, rs, Options{})
	tx := baseTx()
	tx.Amount = 1500_00
	d, _ := e.Evaluate(context.Background(), tx)
	if len(d.Hits) != 1 {
		t.Fatalf("hits = %+v", d.Hits)
	}
	var got []string
	for _, f := range d.Hits[0].Features {
		got = append(got, featureString(f))
	}
	want := []string{
		"tx.time = 2026-11-22T02:30:00Z (2026-11-21 23:30:00 -03:00)",
		"isNight(tx.time) = true",
		"tx.amount = 150000 (R$ 1.500,00)",
		"payer.night_limit = 100000 (R$ 1.000,00)",
		"sum(payer.transfers, 1h) = 0 (R$ 0,00)",
		"payer.device.first_seen = 2025-11-22T02:30:00Z (2025-11-21 23:30:00 -03:00)",
		"age(payer.device.first_seen) = 365d",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("features:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	tx.Payer.Device.FirstSeen = time.Time{}
	d, _ = e.Evaluate(context.Background(), tx)
	if f := d.Hits[0].Features[5]; f.Display != "unset" {
		t.Errorf("zero time display = %q", f.Display)
	}
}

func featureString(f Feature) string {
	if f.Display != "" {
		return fmt.Sprintf("%s = %s (%s)", f.Name, f.Value, f.Display)
	}
	return f.Name + " = " + f.Value
}

func TestVelocityThroughStore(t *testing.T) {
	rs := rules(
		Rule{ID: "burst", Expr: `count(payer.transfers, 10m) >= 3`, Action: StepUp},
		Rule{ID: "fanout", Expr: `distinct(payer.transfers, 1h) >= 3`, Action: Review},
		Rule{ID: "new.pair", Expr: `count(pair.transfers, 90d) == 0`, Action: Review, Score: 1},
		Rule{ID: "fan_in", Expr: `distinct(beneficiary.received, 1h) >= 2`, Action: Hold, HoldFor: Duration(time.Hour)},
		Rule{ID: "device", Expr: `distinct(device.transfers, 1d) >= 2 || distinct(ip.transfers, 1d) >= 2`, Action: Review},
		Rule{ID: "sum", Expr: `sum(payer.transfers, 1h) + tx.amount > 1500_00`, Action: Review},
	)
	e, _ := newTestEngine(t, rs, Options{})
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		tx := baseTx()
		tx.Time = night.Add(time.Duration(i-3) * time.Minute)
		tx.Beneficiary.ID = fmt.Sprint("ben-", i)
		e.Record(tx)
	}
	other := baseTx()
	other.Payer.ID, other.Time = "acc-2", night.Add(-time.Minute)
	e.Record(other)

	d, _ := e.Evaluate(ctx, baseTx())
	if got := ids(d.Hits); got != "burst,fanout,fan_in,device,sum" {
		t.Errorf("hits = %s", got)
	}
	// A brand-new pair and a payer without history.
	tx := baseTx()
	tx.Payer.ID, tx.Payer.Device.ID, tx.Payer.IP, tx.Beneficiary.ID = "acc-9", "", "", "ben-9"
	d, _ = e.Evaluate(ctx, tx)
	if got := ids(d.Hits); got != "new.pair" {
		t.Errorf("fresh payer hits = %s", got)
	}
}

// failingStore returns an error for every query.
type failingStore struct{ *velocity.Store }

var errStore = errors.New("store unavailable")

func (failingStore) Query(string, time.Duration, time.Time) (velocity.Stats, error) {
	return velocity.Stats{}, errStore
}

func (failingStore) QueryCounts(string, time.Duration, time.Time) (velocity.Stats, error) {
	return velocity.Stats{}, errStore
}

func TestFailModes(t *testing.T) {
	inner, _ := velocity.New(velocity.Config{})
	e := New(failingStore{inner}, Options{Logger: quiet})
	rs := rules(
		Rule{ID: "open", Expr: `count(payer.transfers, 10m) > 0`, Action: StepUp, OnError: FailOpen},
		Rule{ID: "closed", Expr: `count(payer.transfers, 1h) > 0`, Action: Review, Score: 7},
		Rule{ID: "default.block", Expr: `sum(payer.transfers, 1h) > 0`, Action: Block},
		Rule{ID: "div", Expr: `tx.amount / (payer.night_limit - payer.night_limit) > 0`, Action: Review},
	)
	rs.FailModes = map[Action]FailMode{Review: FailClosed, Block: FailOpen}
	c, err := Compile(rs, e.CompileOptions())
	if err != nil {
		t.Fatal(err)
	}
	e.Load(c)
	d, _ := e.Evaluate(context.Background(), baseTx())
	if d.Action != Review || !d.Degraded || len(d.Errors) != 4 {
		t.Fatalf("decision = %+v", d)
	}
	if got := ids(d.Hits); got != "closed,div" {
		t.Errorf("hits = %s", got)
	}
	if !d.Hits[0].FailedClosed || d.Errors[0].FailMode != FailOpen || d.Errors[1].FailMode != FailClosed {
		t.Errorf("errors = %+v", d.Errors)
	}
	if !strings.Contains(d.Errors[3].Error, "division by zero") {
		t.Errorf("div error = %q", d.Errors[3].Error)
	}
}

// stepClock advances by step on every call, so the latency budget runs
// out after a predictable number of checks.
type stepClock struct {
	mu   sync.Mutex
	now  time.Time
	step time.Duration
}

func (c *stepClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(c.step)
	return c.now
}

func TestLatencyBudget(t *testing.T) {
	var rs Ruleset
	rs.Name, rs.Version = "budget", "1"
	for i := 0; i < 10; i++ {
		rs.Rules = append(rs.Rules, Rule{ID: fmt.Sprint("r", i), Expr: `count(payer.transfers, 10m) >= 0`, Action: Review, Score: 1})
	}
	rs.Rules[9].Action, rs.Rules[9].OnError = Block, FailClosed
	clk := &stepClock{now: night, step: time.Millisecond}
	e, _ := newTestEngine(t, rs, Options{Budget: 4500 * time.Microsecond, Clock: clk.Now})
	d, _ := e.Evaluate(context.Background(), baseTx())
	// Start reads the clock once (t+1ms, deadline t+5.5ms); each rule with
	// aggregates checks it once, so r0..r3 run and the check before r4
	// (t+6ms) finds the budget spent.
	if len(d.Hits) != 5 || !d.Degraded || len(d.Errors) != 6 {
		t.Fatalf("hits %s errors %d", ids(d.Hits), len(d.Errors))
	}
	if d.Action != Block || !d.Hits[4].FailedClosed || d.Hits[4].RuleID != "r9" {
		t.Errorf("fail-closed rule after deadline: %+v", d.Hits)
	}
	if !strings.Contains(d.Errors[0].Error, "latency budget") {
		t.Errorf("error = %q", d.Errors[0].Error)
	}
}

func TestContextCanceled(t *testing.T) {
	e, _ := newTestEngine(t, rules(
		Rule{ID: "a", Expr: `count(payer.transfers, 10m) >= 0`, Action: Review},
		Rule{ID: "b", Expr: `true`, Action: Block, OnError: FailClosed},
	), Options{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	d, _ := e.Evaluate(ctx, baseTx())
	if len(d.Errors) != 2 || !strings.Contains(d.Errors[0].Error, "context canceled") || d.Action != Block {
		t.Errorf("decision = %+v", d)
	}
	// A context deadline earlier than the budget wins.
	ctx, cancel = context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	d, _ = e.Evaluate(ctx, baseTx())
	if !d.Degraded {
		t.Error("expired deadline not honored")
	}
}

func TestLoadRollbackHistory(t *testing.T) {
	e := New(nil, Options{Logger: quiet, History: 3})
	if _, err := e.Evaluate(context.Background(), baseTx()); !errors.Is(err, ErrNoRuleset) {
		t.Fatalf("no ruleset: %v", err)
	}
	if _, _, err := e.Trace(context.Background(), baseTx()); !errors.Is(err, ErrNoRuleset) {
		t.Fatalf("trace without ruleset: %v", err)
	}
	if _, err := e.Rollback(); !errors.Is(err, ErrNoPrevious) {
		t.Fatalf("rollback on empty: %v", err)
	}
	for v := 1; v <= 4; v++ {
		data := fmt.Sprintf(`{"name":"rs","version":"%d","rules":[{"id":"r","expr":"tx.amount > %d","action":"REVIEW"}]}`, v, v*100)
		c, err := e.LoadJSON([]byte(data))
		if err != nil {
			t.Fatal(err)
		}
		if c.Revision != int64(v) || e.Active() != c {
			t.Fatalf("revision %d active %v", c.Revision, e.Active().Revision)
		}
	}
	h := e.History()
	if len(h) != 3 || h[0].Ruleset.Version != "2" || h[2].Ruleset.Version != "4" {
		t.Fatalf("history = %d entries", len(h))
	}
	prev, err := e.Rollback()
	if err != nil || prev.Ruleset.Version != "3" || e.Active().Ruleset.Version != "3" {
		t.Fatalf("rollback = %v, %v", prev, err)
	}
	if _, err := e.LoadJSON([]byte(`{"name":"rs","version":"bad","rules":[{"id":"r","expr":"tx.amount >","action":"REVIEW"}]}`)); err == nil {
		t.Fatal("invalid ruleset loaded")
	}
	if e.Active().Ruleset.Version != "3" {
		t.Error("failed load must keep the active ruleset")
	}
	if _, err := e.LoadJSON([]byte(`{`)); err == nil {
		t.Error("malformed JSON loaded")
	}
	// Nil store: aggregates are zero and Record is a no-op.
	e.Record(baseTx())
	if (e.CompileOptions() != CompileOptions{}) {
		t.Error("nil store compile options")
	}
}

func TestEvaluateWithAndTrace(t *testing.T) {
	e, _ := newTestEngine(t, rules(
		Rule{ID: "a", Expr: `tx.amount > 100_00`, Action: Review},
		Rule{ID: "b", Expr: `tx.amount > 1000_00`, Action: Block},
		Rule{ID: "c", Expr: `true`, Action: Block, Enabled: bt(false)},
		Rule{ID: "d", Expr: `1 / (tx.amount - tx.amount) == 0`, Action: Review},
	), Options{})
	d, tr, err := e.Trace(context.Background(), baseTx())
	if err != nil || d.Action != Review || len(tr) != 4 {
		t.Fatalf("trace = %+v %+v %v", d, tr, err)
	}
	if !tr[0].Matched || tr[1].Matched || tr[2].Enabled || tr[3].Error == "" || len(tr[1].Features) != 1 {
		t.Errorf("trace = %+v", tr)
	}
	cand, err := Compile(rules(Rule{ID: "x", Expr: `true`, Action: Block}), CompileOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if d := e.EvaluateWith(context.Background(), cand, baseTx()); d.Action != Block {
		t.Errorf("EvaluateWith = %s", d.Action)
	}
	if e.Active().Ruleset.Name != "test" {
		t.Error("EvaluateWith must not activate the candidate")
	}
}

func TestCompileValidation(t *testing.T) {
	rs := Ruleset{
		Thresholds: Thresholds{Review: 50, StepUp: 40, Block: -1},
		TimeZone:   "Nowhere/Land",
		FailModes:  map[Action]FailMode{Review: FailUnset},
		Rules: []Rule{
			{ID: "", Expr: `true`, Action: Review},
			{ID: "dup", Expr: `true`, Action: Review},
			{ID: "dup", Expr: `true`, Action: Review},
			{ID: "hold", Expr: `true`, Action: Hold},
			{ID: "nothold", Expr: `true`, Action: Review, HoldFor: Duration(time.Minute)},
			{ID: "syntax", Expr: `tx.amount >`, Action: Review},
			{ID: "type", Expr: `tx.amount`, Action: Review},
			{ID: "window", Expr: `count(payer.transfers, 365d) > 0`, Action: Review},
			{ID: "badaction", Expr: `true`, Action: Action(9)},
			{ID: "badmode", Expr: `true`, Action: Review, Mode: Mode(7)},
		},
	}
	_, err := Compile(rs, CompileOptions{MaxWindow: 180 * 24 * time.Hour})
	var ve *ValidationError
	if !errors.As(err, &ve) || !errors.Is(err, ErrInvalidRuleset) {
		t.Fatalf("err = %v", err)
	}
	var fields []string
	for _, p := range ve.Problems {
		fields = append(fields, p.RuleID+"."+p.Field)
		_ = p.Snippet()
	}
	want := ".name .version .thresholds .thresholds .fail_modes .time_zone .id dup.id hold.hold_for nothold.hold_for syntax.expr type.expr window.expr badaction.action badmode.mode"
	if got := strings.Join(fields, " "); got != want {
		t.Errorf("problems:\n%s\nwant:\n%s", got, want)
	}
	if !strings.Contains(err.Error(), "15 problem(s)") {
		t.Errorf("error = %v", err)
	}
	snip := ve.Problems[10].Snippet()
	if !strings.Contains(snip, "tx.amount >\n") || !strings.Contains(snip, "^") {
		t.Errorf("snippet = %q", snip)
	}
	if _, err := Compile(Ruleset{Name: "x", Version: "1"}, CompileOptions{}); err == nil {
		t.Error("empty rules accepted")
	}
}

func TestParseRuleset(t *testing.T) {
	if _, err := ParseRuleset([]byte(`{"name":"x","version":"1","rules":[{"id":"a","expr":"true","acton":"BLOCK"}]}`)); err == nil {
		t.Error("unknown field accepted")
	}
	if _, err := ParseRuleset([]byte(`{"name":"x"} {}`)); err == nil {
		t.Error("trailing data accepted")
	}
	rs, err := ParseRuleset([]byte(`{"name":"x","version":"1","fail_modes":{"BLOCK":"closed"},"rules":[{"id":"a","expr":"true","action":"hold","hold_for":"1h30m","mode":"shadow","on_error":"open"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	r := rs.Rules[0]
	if r.Action != Hold || r.HoldFor != Duration(90*time.Minute) || r.Mode != Shadow || r.OnError != FailOpen || rs.FailModes[Block] != FailClosed {
		t.Errorf("rule = %+v", r)
	}
	out, _ := json.Marshal(rs)
	if !strings.Contains(string(out), `"hold_for":"1h30m"`) || !strings.Contains(string(out), `"fail_modes":{"BLOCK":"closed"}`) {
		t.Errorf("marshal = %s", out)
	}
}

func TestEnumsJSON(t *testing.T) {
	for _, s := range []string{`"x"`, `1`} {
		var a Action
		if json.Unmarshal([]byte(s), &a) == nil {
			t.Errorf("Action %s accepted", s)
		}
	}
	var m Mode
	if json.Unmarshal([]byte(`"loud"`), &m) == nil {
		t.Error("bad mode accepted")
	}
	var f FailMode
	if json.Unmarshal([]byte(`"ajar"`), &f) == nil {
		t.Error("bad fail mode accepted")
	}
	if Action(9).String() != "Action(9)" || FailUnset.String() != "" {
		t.Error("String of invalid enums")
	}
	if _, err := Action(9).MarshalText(); err == nil {
		t.Error("marshal invalid action")
	}
	var d Duration
	if err := json.Unmarshal([]byte(`60000000000`), &d); err != nil || d != Duration(time.Minute) {
		t.Errorf("numeric duration = %v, %v", d, err)
	}
	for _, bad := range []string{`"soon"`, `true`} {
		if json.Unmarshal([]byte(bad), &d) == nil {
			t.Errorf("duration %s accepted", bad)
		}
	}
	if d.String() != "1m" {
		t.Errorf("Duration.String = %s", d)
	}
}

func TestFormatBRL(t *testing.T) {
	for cents, want := range map[int64]string{
		0: "R$ 0,00", 5: "R$ 0,05", 100: "R$ 1,00", 150000: "R$ 1.500,00", 123456789: "R$ 1.234.567,89", -2050: "-R$ 20,50",
	} {
		if got := FormatBRL(cents); got != want {
			t.Errorf("FormatBRL(%d) = %q, want %q", cents, got, want)
		}
	}
}

func TestDefaultRulesetCompiles(t *testing.T) {
	data, err := os.ReadFile("../rulesets/pix-default.json")
	if err != nil {
		t.Fatal(err)
	}
	store, _ := velocity.New(velocity.Config{})
	e := New(store, Options{Logger: quiet})
	c, err := e.LoadJSON(data)
	if err != nil {
		t.Fatal(err)
	}
	if n := len(c.Ruleset.Rules); n < 30 {
		t.Errorf("default ruleset has %d rules, want at least 30", n)
	}
	if len(c.Windows()) == 0 {
		t.Error("no velocity windows")
	}
	if p, ok := c.Program("limit.night.pf"); !ok || p.Type() != expr.Bool {
		t.Error("Program lookup")
	}
	if _, ok := c.Program("nope"); ok {
		t.Error("Program lookup of unknown rule")
	}
	// An account takeover at night: new device, over the night limit.
	tx := baseTx()
	tx.Amount = 1500_00
	tx.Payer.Device = Device{ID: "dev-new", FirstSeen: night.Add(-10 * time.Minute)}
	d, _ := e.Evaluate(context.Background(), tx)
	if d.Action != Block {
		t.Errorf("ATO at night = %s (%q)", d.Action, d.Reasons)
	}
	// A routine daytime payment to a known beneficiary is allowed.
	day := baseTx()
	day.Time = night.Add(-10 * time.Hour)
	for i := 0; i < 3; i++ {
		prior := baseTx()
		prior.Time = day.Time.AddDate(0, 0, -7*(i+1))
		e.Record(prior)
	}
	d, _ = e.Evaluate(context.Background(), day)
	if d.Action != Allow {
		t.Errorf("routine payment = %s (%q, hits %s)", d.Action, d.Reasons, ids(d.Hits))
	}
}

// TestConcurrentHotSwap evaluates on many goroutines while rulesets are
// swapped and rolled back; run with -race.
func TestConcurrentHotSwap(t *testing.T) {
	e, _ := newTestEngine(t, rules(Rule{ID: "a", Expr: `tx.amount > 0`, Action: Review}), Options{})
	var wg sync.WaitGroup
	stop := make(chan struct{})
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; ; i++ {
				select {
				case <-stop:
					return
				default:
				}
				tx := baseTx()
				tx.Time = night.Add(time.Duration(i) * time.Second)
				d, err := e.Evaluate(context.Background(), tx)
				if err != nil || (d.Action != Review && d.Action != Block) {
					t.Errorf("decision %s, %v", d.Action, err)
					return
				}
				e.Record(tx)
			}
		}(w)
	}
	for i := 0; i < 50; i++ {
		act := "BLOCK"
		if i%2 == 0 {
			act = "REVIEW"
		}
		if _, err := e.LoadJSON([]byte(fmt.Sprintf(`{"name":"swap","version":"%d","rules":[{"id":"a","expr":"tx.amount > 0 && count(payer.transfers, 1h) >= 0","action":%q}]}`, i, act))); err != nil {
			t.Fatal(err)
		}
		if i%5 == 0 {
			_, _ = e.Rollback()
		}
	}
	close(stop)
	wg.Wait()
}

func TestNoExplain(t *testing.T) {
	e, _ := newTestEngine(t, rules(Rule{ID: "a", Expr: `tx.amount > 0`, Action: Review}), Options{NoExplain: true})
	d, _ := e.Evaluate(context.Background(), baseTx())
	if len(d.Hits) != 1 || d.Hits[0].Features != nil {
		t.Errorf("hits = %+v", d.Hits)
	}
}
