// Command gen-history writes a synthetic, labeled history of Pix transfers
// as JSONL, for backtesting rulesets.
//
// The population is deterministic for a given seed. Legitimate traffic
// follows a diurnal profile with log-normal amounts, recurring
// beneficiaries, occasional new beneficiaries, legitimate phone changes and
// bill-splitting bursts, so that rules have false positives to fight.
// Fraud is injected after a clean warm-up period using four patterns:
//
//	ato_night_burst  account takeover from a new device at night
//	mule_fan_out     a new account spreading funds over many fresh accounts
//	scam_fan_in      many victims paying one collection account
//	structuring      night transfers just below the nighttime limit
//
// Usage:
//
//	gen-history [-seed 42] [-payers 100] [-days 21] [-fraud-from 7] [-start 2026-10-01] [-o file]
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"time"
)

func main() { os.Exit(run(os.Args[1:], os.Stdout, os.Stderr)) }

func run(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("gen-history", flag.ContinueOnError)
	fs.SetOutput(stderr)
	seed := fs.Uint64("seed", 42, "random seed")
	payers := fs.Int("payers", 100, "number of customers")
	days := fs.Int("days", 21, "days of history")
	fraudFrom := fs.Int("fraud-from", 7, "first day with injected fraud (warm-up before it)")
	start := fs.String("start", "2026-10-01", "first day, YYYY-MM-DD in Brasília time")
	out := fs.String("o", "", "output file (default stdout)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	t0, err := time.ParseInLocation("2006-01-02", *start, brt)
	if err != nil || *payers < 10 || *days < 1 || *fraudFrom < 0 || *fraudFrom >= *days {
		fmt.Fprintln(stderr, "gen-history: invalid flags: need -payers >= 10, -days >= 1, 0 <= -fraud-from < -days and -start YYYY-MM-DD")
		return 2
	}
	w := stdout
	if *out != "" {
		f, err := os.Create(*out)
		if err != nil {
			fmt.Fprintln(stderr, "gen-history:", err)
			return 1
		}
		defer f.Close()
		w = f
	}
	txs := generate(Config{Seed: *seed, Start: t0, Days: *days, Payers: *payers, FraudFrom: *fraudFrom})
	bw := bufio.NewWriter(w)
	enc := json.NewEncoder(bw)
	fraud := 0
	for i := range txs {
		if txs[i].Label == "fraud" {
			fraud++
		}
		if err := enc.Encode(&txs[i]); err != nil {
			fmt.Fprintln(stderr, "gen-history:", err)
			return 1
		}
	}
	if err := bw.Flush(); err != nil {
		fmt.Fprintln(stderr, "gen-history:", err)
		return 1
	}
	fmt.Fprintf(stderr, "gen-history: %d transactions, %d fraud\n", len(txs), fraud)
	return 0
}
