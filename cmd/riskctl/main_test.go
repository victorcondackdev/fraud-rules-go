package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	ruleset = "../../rulesets/pix-default.json"
	sample  = "../../testdata/history-sample.jsonl"
	txATO   = "../../testdata/tx-ato-night.json"
	txOK    = "../../testdata/tx-routine.json"
)

func runCLI(args ...string) (int, string, string) {
	var out, errb bytes.Buffer
	code := run(args, &out, &errb)
	return code, out.String(), errb.String()
}

func TestValidate(t *testing.T) {
	code, out, _ := runCLI("validate", ruleset)
	if code != 0 || !strings.Contains(out, "ok: pix-default@0.1.0") || !strings.Contains(out, "count(pair.transfers, 90d)") {
		t.Fatalf("validate: %d %s", code, out)
	}
	bad := filepath.Join(t.TempDir(), "bad.json")
	os.WriteFile(bad, []byte(`{"name":"x","version":"1","rules":[{"id":"r","expr":"tx.amount > payer.nigth_limit","action":"BLOCK"}]}`), 0o600)
	code, out, errOut := runCLI("validate", bad)
	if code != 1 || !strings.Contains(out, "^~~~") || !strings.Contains(errOut, "ruleset is invalid") {
		t.Errorf("invalid: %d %q %q", code, out, errOut)
	}
	os.WriteFile(bad, []byte(`{`), 0o600)
	if code, _, _ := runCLI("validate", bad); code != 1 {
		t.Errorf("malformed: %d", code)
	}
	if code, _, _ := runCLI("validate", "/nonexistent.json"); code != 1 {
		t.Errorf("missing file: %d", code)
	}
	if code, _, _ := runCLI("validate"); code != 2 {
		t.Errorf("no args: %d", code)
	}
}

func TestEvalAndExplain(t *testing.T) {
	code, out, errOut := runCLI("eval", "-history", sample, ruleset, txATO)
	if code != 0 || !strings.Contains(out, "decision  BLOCK") || !strings.Contains(out, "age(payer.device.first_seen) = 22m30s") {
		t.Fatalf("eval: %d %s %s", code, out, errOut)
	}
	if !strings.Contains(errOut, "replayed") {
		t.Errorf("stderr = %q", errOut)
	}
	code, out, _ = runCLI("eval", "-json", ruleset, txOK)
	if code != 0 || !strings.Contains(out, `"action": "`) {
		t.Errorf("eval json: %d %s", code, out)
	}
	code, out, _ = runCLI("explain", "-history", sample, ruleset, txOK)
	if code != 0 || !strings.Contains(out, "trace") || !strings.Contains(out, "weekend.high_value") || !strings.Contains(out, "disabled") {
		t.Errorf("explain: %d %s", code, out)
	}
	code, out, _ = runCLI("explain", "-json", ruleset, txOK)
	if code != 0 || !strings.Contains(out, `"trace"`) {
		t.Errorf("explain json: %d", code)
	}
	dir := t.TempDir()
	hold := filepath.Join(dir, "hold.json")
	os.WriteFile(hold, []byte(`{"name":"h","version":"1","rules":[{"id":"h","expr":"true","action":"HOLD","hold_for":"2h"},
	  {"id":"s","expr":"true","action":"BLOCK","mode":"shadow"},{"id":"e","expr":"1 / (tx.amount - tx.amount) == 0","action":"REVIEW"}]}`), 0o600)
	code, out, _ = runCLI("eval", hold, txOK)
	if code != 0 || !strings.Contains(out, "HOLD for 2h") || !strings.Contains(out, "with shadow rules enforced: BLOCK") || !strings.Contains(out, "division by zero") {
		t.Errorf("hold eval: %s", out)
	}
	for _, args := range [][]string{
		{"eval", ruleset},
		{"eval", ruleset, "/nonexistent.json"},
		{"eval", "/nonexistent.json", txOK},
		{"eval", "-history", "/nonexistent.jsonl", ruleset, txOK},
		{"eval", ruleset, ruleset},
		{"eval", "-nope", ruleset, txOK},
	} {
		if code, _, _ := runCLI(args...); code == 0 {
			t.Errorf("%v succeeded", args)
		}
	}
	badHist := filepath.Join(dir, "h.jsonl")
	os.WriteFile(badHist, []byte("{oops}\n"), 0o600)
	if code, _, _ := runCLI("eval", "-history", badHist, ruleset, txOK); code != 1 {
		t.Errorf("bad history: %d", code)
	}
	badRules := filepath.Join(dir, "r.json")
	os.WriteFile(badRules, []byte(`{"name":"x","version":"1","rules":[{"id":"r","expr":"nope","action":"BLOCK"}]}`), 0o600)
	if code, out, _ := runCLI("eval", badRules, txOK); code != 1 || !strings.Contains(out, "undefined field") {
		t.Errorf("invalid ruleset eval: %d %s", code, out)
	}
	if code, out, _ := runCLI("backtest", badRules, sample); code != 1 || !strings.Contains(out, "undefined field") {
		t.Errorf("invalid ruleset backtest: %d %s", code, out)
	}
}

func TestBacktest(t *testing.T) {
	code, out, _ := runCLI("backtest", ruleset, sample)
	if code != 0 || !strings.Contains(out, "interdicted (BLOCK|HOLD)  88.3%") || !strings.Contains(out, "ato_night_burst") {
		t.Fatalf("backtest: %d\n%s", code, out)
	}
	code, out, _ = runCLI("backtest", "-json", "-promote-shadow", "-record-blocked", "-warmup", "3d", ruleset, sample)
	if code != 0 || !strings.Contains(out, `"interdicted"`) {
		t.Errorf("backtest json: %d", code)
	}
	for _, args := range [][]string{
		{"backtest", ruleset},
		{"backtest", "-warmup", "soon", ruleset, sample},
		{"backtest", "/nonexistent.json", sample},
		{"backtest", ruleset, "/nonexistent.jsonl"},
		{"backtest", ruleset, ruleset},
	} {
		if code, _, _ := runCLI(args...); code == 0 {
			t.Errorf("%v succeeded", args)
		}
	}
}

func TestMisc(t *testing.T) {
	if code, out, _ := runCLI("version"); code != 0 || !strings.HasPrefix(out, "riskctl ") {
		t.Errorf("version: %s", out)
	}
	if code, out, _ := runCLI("help"); code != 0 || !strings.Contains(out, "usage") {
		t.Errorf("help: %d", code)
	}
	if code, _, errOut := runCLI(); code != 2 || !strings.Contains(errOut, "usage") {
		t.Errorf("no args: %d", code)
	}
	if code, _, _ := runCLI("frobnicate"); code != 2 {
		t.Errorf("unknown command: %d", code)
	}
	if code, _, _ := runCLI("validate", "-h"); code != 2 {
		t.Errorf("-h: %d", code)
	}
}
