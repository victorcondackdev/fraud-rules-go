package server

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/victorcondackdev/fraud-rules-go/engine"
	"github.com/victorcondackdev/fraud-rules-go/velocity"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

func newServer(t *testing.T, load bool) (*Server, *httptest.Server) {
	t.Helper()
	store, _ := velocity.New(velocity.Config{})
	eng := engine.New(store, engine.Options{Logger: quiet})
	if load {
		data, err := os.ReadFile("../../rulesets/pix-default.json")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := eng.LoadJSON(data); err != nil {
			t.Fatal(err)
		}
	}
	s := New(eng, store, quiet)
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return s, ts
}

func do(t *testing.T, method, url, body string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(method, url, strings.NewReader(body))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

const txJSON = `{"id":"tx-1","time":"2026-11-21T23:30:00-03:00","amount":150000,
 "payer":{"id":"acc-1","type":"PF","night_limit":100000,"day_limit":500000,
   "account_created":"2020-01-01T00:00:00-03:00",
   "device":{"id":"dev-9","trusted":false,"first_seen":"2026-11-21T23:20:00-03:00"}},
 "beneficiary":{"id":"ben-1","account_created":"2026-11-10T00:00:00-03:00","key_created":"2026-11-21T10:00:00-03:00"}}`

func TestEvaluateEndpoint(t *testing.T) {
	s, ts := newServer(t, true)
	code, body := do(t, "POST", ts.URL+"/v1/evaluate", txJSON)
	if code != 200 {
		t.Fatalf("status %d: %s", code, body)
	}
	var d engine.Decision
	if err := json.Unmarshal([]byte(body), &d); err != nil {
		t.Fatal(err)
	}
	if d.Action != engine.Block || len(d.Hits) == 0 || len(d.Hits[0].Features) == 0 {
		t.Errorf("decision = %+v", d)
	}
	if s.decisions.Value("BLOCK") != 1 || s.ruleHits.Value("limit.night.pf", "enforce") != 1 {
		t.Error("metrics not updated")
	}
	if s.ruleHits.Value("shadow.night.new_beneficiary.untrusted", "shadow") != 1 {
		t.Error("shadow hit not counted")
	}

	for _, bad := range []string{`{`, `{"id":"x"}`, `{"id":"x","time":"2026-11-21T23:30:00Z","bogus":1}`, txJSON + `{}`} {
		if code, _ := do(t, "POST", ts.URL+"/v1/evaluate", bad); code != 400 {
			t.Errorf("bad body %q: status %d", bad, code)
		}
	}
	if code, _ := do(t, "POST", ts.URL+"/v1/evaluate", strings.Repeat(" ", MaxBodyBytes+1)); code != 400 {
		t.Errorf("oversized body: %d", code)
	}
	if code, _ := do(t, "GET", ts.URL+"/v1/evaluate", ""); code != 405 {
		t.Errorf("GET evaluate: %d", code)
	}
}

func TestNoRuleset(t *testing.T) {
	_, ts := newServer(t, false)
	if code, _ := do(t, "GET", ts.URL+"/healthz", ""); code != 503 {
		t.Errorf("healthz without ruleset: %d", code)
	}
	if code, _ := do(t, "POST", ts.URL+"/v1/evaluate", txJSON); code != 503 {
		t.Errorf("evaluate without ruleset: %d", code)
	}
	if code, _ := do(t, "GET", ts.URL+"/v1/rulesets/active", ""); code != 404 {
		t.Errorf("active without ruleset: %d", code)
	}
}

func TestEventsChangeVelocity(t *testing.T) {
	_, ts := newServer(t, true)
	var events []string
	for i := 0; i < 6; i++ {
		ev := strings.Replace(txJSON, `"id":"ben-1"`, `"id":"ben-`+string(rune('a'+i))+`"`, 1)
		ev = strings.Replace(ev, `"amount":150000`, `"amount":1000`, 1)
		ev = strings.Replace(ev, `23:30:00-03:00","amount"`, `23:2`+string(rune('0'+i))+`:00-03:00","amount"`, 1)
		events = append(events, ev)
	}
	code, body := do(t, "POST", ts.URL+"/v1/events", "["+strings.Join(events, ",")+"]")
	if code != 202 || !strings.Contains(body, `"recorded": 6`) {
		t.Fatalf("events: %d %s", code, body)
	}
	if code, _ := do(t, "POST", ts.URL+"/v1/events", events[0]); code != 202 {
		t.Errorf("single event: %d", code)
	}
	_, body = do(t, "POST", ts.URL+"/v1/evaluate", txJSON)
	if !strings.Contains(body, `"fanout.distinct.1h"`) || !strings.Contains(body, `"distinct(payer.transfers, 1h)"`) {
		t.Errorf("velocity rule did not fire: %s", body)
	}
	for _, bad := range []string{`[{}]`, `{"time":"2026-11-21T23:30:00Z"}`, `[`, `nope`} {
		if code, _ := do(t, "POST", ts.URL+"/v1/events", bad); code != 400 {
			t.Errorf("bad event %q: %d", bad, code)
		}
	}
	if code, _ := do(t, "POST", ts.URL+"/v1/events", strings.Repeat(" ", MaxBodyBytes+1)); code != 413 {
		t.Errorf("oversized events: %d", code)
	}
}

func TestRulesetLifecycle(t *testing.T) {
	s, ts := newServer(t, true)
	code, body := do(t, "GET", ts.URL+"/v1/rulesets/active", "")
	if code != 200 || !strings.Contains(body, `"pix-default"`) {
		t.Fatalf("active: %d", code)
	}
	v2 := `{"name":"pix-default","version":"0.2.0","rules":[{"id":"all","expr":"tx.amount > 0","action":"REVIEW"}]}`
	code, body = do(t, "PUT", ts.URL+"/v1/rulesets", v2)
	if code != 200 || !strings.Contains(body, `"revision": 2`) {
		t.Fatalf("put: %d %s", code, body)
	}
	_, body = do(t, "POST", ts.URL+"/v1/evaluate", txJSON)
	if !strings.Contains(body, `"action": "REVIEW"`) || !strings.Contains(body, `"revision": 2`) {
		t.Errorf("hot swap not effective: %s", body)
	}
	_, body = do(t, "GET", ts.URL+"/v1/rulesets", "")
	var list struct{ Revisions []rulesetInfo }
	_ = json.Unmarshal([]byte(body), &list)
	if len(list.Revisions) != 2 || !list.Revisions[0].Active || list.Revisions[0].Version != "0.2.0" {
		t.Errorf("list = %+v", list)
	}

	bad := `{"name":"pix-default","version":"0.3.0","rules":[{"id":"oops","expr":"tx.amount >\n  payer.nigth_limit","action":"BLOCK"}]}`
	code, body = do(t, "PUT", ts.URL+"/v1/rulesets", bad)
	if code != 422 {
		t.Fatalf("invalid put: %d", code)
	}
	var ae apiError
	_ = json.Unmarshal([]byte(body), &ae)
	if len(ae.Problems) != 1 || ae.Problems[0].Line != 2 || ae.Problems[0].Col != 3 || ae.Problems[0].RuleID != "oops" ||
		!strings.Contains(ae.Problems[0].Snippet, "^~~~") {
		t.Errorf("problems = %+v", ae.Problems)
	}
	if code, _ := do(t, "PUT", ts.URL+"/v1/rulesets", `{"name":""}`); code != 422 {
		t.Errorf("empty ruleset: %d", code)
	}
	if code, _ := do(t, "PUT", ts.URL+"/v1/rulesets", strings.Repeat(" ", MaxBodyBytes+1)); code != 413 {
		t.Errorf("oversized ruleset: %d", code)
	}

	code, body = do(t, "POST", ts.URL+"/v1/rulesets/rollback", "")
	if code != 200 || !strings.Contains(body, `"version": "0.1.0"`) {
		t.Errorf("rollback: %d %s", code, body)
	}
	if code, _ := do(t, "POST", ts.URL+"/v1/rulesets/rollback", ""); code != 409 {
		t.Errorf("second rollback: %d", code)
	}
	if s.reloads.Value("ok") != 1 || s.reloads.Value("invalid") != 2 || s.reloads.Value("rollback") != 1 {
		t.Error("reload metrics")
	}
}

func TestValidateRule(t *testing.T) {
	_, ts := newServer(t, true)
	tests := []struct {
		body, want string
	}{
		{`{"expr":"count(payer.transfers, 10m) >= 5 && tx.amount > 100_00"}`, `"valid": true`},
		{`{"expr":"tx.amount"}`, `"type": "int"`},
		{`{"expr":"tx.amount > \"x\""}`, `"col": 11`},
		{`{"expr":"count(payer.transfers, 400d) > 1"}`, `exceeds the feature store horizon`},
	}
	for _, tt := range tests {
		code, body := do(t, "POST", ts.URL+"/v1/rules/validate", tt.body)
		if code != 200 || !strings.Contains(body, tt.want) {
			t.Errorf("%s: %d %s", tt.body, code, body)
		}
	}
	if code, _ := do(t, "POST", ts.URL+"/v1/rules/validate", `{"exp":"x"}`); code != 400 {
		t.Errorf("unknown field: %d", code)
	}
}

func TestMetricsAndHealth(t *testing.T) {
	_, ts := newServer(t, true)
	do(t, "POST", ts.URL+"/v1/evaluate", txJSON)
	if code, body := do(t, "GET", ts.URL+"/healthz", ""); code != 200 || !strings.Contains(body, "ok") {
		t.Errorf("healthz: %d", code)
	}
	resp, err := http.Get(ts.URL + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/plain; version=0.0.4") {
		t.Errorf("content type %q", ct)
	}
	b, _ := io.ReadAll(resp.Body)
	text := string(b)
	for _, want := range []string{
		`riskd_decisions_total{action="BLOCK"} 1`,
		`riskd_rule_hits_total{rule="limit.night.pf",mode="enforce"} 1`,
		`riskd_eval_duration_seconds_bucket{le="+Inf"} 1`,
		`riskd_eval_duration_seconds_count 1`,
		`riskd_http_requests_total{route="POST /v1/evaluate",code="200"} 1`,
		`riskd_ruleset_revision 1`,
		"# TYPE riskd_eval_duration_seconds histogram",
		"riskd_velocity_keys 0",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("metrics missing %q", want)
		}
	}
}

func TestDegradedDecisionLogged(t *testing.T) {
	var logs bytes.Buffer
	store, _ := velocity.New(velocity.Config{})
	eng := engine.New(store, engine.Options{Logger: quiet})
	if _, err := eng.LoadJSON([]byte(`{"name":"x","version":"1","rules":[{"id":"div","expr":"1 / (tx.amount - tx.amount) == 0","action":"BLOCK","on_error":"closed"}]}`)); err != nil {
		t.Fatal(err)
	}
	s := New(eng, nil, slog.New(slog.NewJSONHandler(&logs, nil)))
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	_, body := do(t, "POST", ts.URL+"/v1/evaluate", txJSON)
	if !strings.Contains(body, `"degraded": true`) || !strings.Contains(logs.String(), `"level":"WARN"`) {
		t.Errorf("body %s\nlogs %s", body, logs.String())
	}
	if s.ruleErrors.Value("div", "closed") != 1 {
		t.Error("rule error metric")
	}
	_ = s.Metrics()
}
