package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/victorcondackdev/fraud-rules-go/engine"
)

func TestDeterministicAndLabeled(t *testing.T) {
	dir := t.TempDir()
	a, b := filepath.Join(dir, "a.jsonl"), filepath.Join(dir, "b.jsonl")
	var errb bytes.Buffer
	for _, p := range []string{a, b} {
		if code := run([]string{"-payers", "30", "-days", "10", "-fraud-from", "3", "-o", p}, nil, &errb); code != 0 {
			t.Fatalf("exit %d: %s", code, errb.String())
		}
	}
	da, _ := os.ReadFile(a)
	db, _ := os.ReadFile(b)
	if !bytes.Equal(da, db) {
		t.Fatal("same seed produced different output")
	}
	patterns := map[string]int{}
	var prev engine.Transaction
	for i, line := range strings.Split(strings.TrimSpace(string(da)), "\n") {
		var tx engine.Transaction
		if err := json.Unmarshal([]byte(line), &tx); err != nil {
			t.Fatal(err)
		}
		if tx.Label != "fraud" && tx.Label != "legit" {
			t.Fatalf("line %d: label %q", i, tx.Label)
		}
		if (tx.Label == "fraud") != (tx.Pattern != "") {
			t.Fatalf("line %d: label/pattern mismatch", i)
		}
		if i > 0 && tx.Time.Before(prev.Time) {
			t.Fatalf("line %d: not sorted", i)
		}
		patterns[tx.Pattern]++
		prev = tx
	}
	for _, p := range []string{"", "ato_night_burst", "mule_fan_out", "scam_fan_in", "structuring"} {
		if patterns[p] == 0 {
			t.Errorf("no transactions for pattern %q", p)
		}
	}
	// The committed sample is reproducible from the default flags.
	var out bytes.Buffer
	if code := run(nil, &out, &errb); code != 0 {
		t.Fatal(errb.String())
	}
	sample, err := os.ReadFile("../../testdata/history-sample.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(out.Bytes(), sample) {
		t.Error("testdata/history-sample.jsonl is stale: regenerate with go run ./cmd/gen-history -o testdata/history-sample.jsonl")
	}
}

func TestFlags(t *testing.T) {
	var errb bytes.Buffer
	for _, args := range [][]string{
		{"-payers", "1"}, {"-days", "0"}, {"-fraud-from", "30"}, {"-start", "tomorrow"}, {"-bogus"},
	} {
		if code := run(args, nil, &errb); code != 2 {
			t.Errorf("%v: exit %d", args, code)
		}
	}
	if code := run([]string{"-o", "/nonexistent/dir/x.jsonl"}, nil, &errb); code != 1 {
		t.Errorf("unwritable output: exit %d", code)
	}
}
