// Command riskctl validates, evaluates, explains and backtests rulesets
// offline.
//
//	riskctl validate ruleset.json
//	riskctl eval     [-history h.jsonl] [-json] ruleset.json tx.json
//	riskctl explain  [-history h.jsonl] ruleset.json tx.json
//	riskctl backtest [-warmup 7d] [-promote-shadow] [-record-blocked] [-json] ruleset.json history.jsonl
//
// eval and explain can replay a history file into the feature store first,
// so velocity rules see the context the transaction would have had.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/victorcondackdev/fraud-rules-go/backtest"
	"github.com/victorcondackdev/fraud-rules-go/engine"
	"github.com/victorcondackdev/fraud-rules-go/expr"
	"github.com/victorcondackdev/fraud-rules-go/velocity"
)

var version = "dev"

func main() { os.Exit(run(os.Args[1:], os.Stdout, os.Stderr)) }

const usage = `usage:
  riskctl validate ruleset.json
  riskctl eval     [-history h.jsonl] [-json] ruleset.json tx.json
  riskctl explain  [-history h.jsonl] ruleset.json tx.json
  riskctl backtest [-warmup 7d] [-promote-shadow] [-record-blocked] [-json] ruleset.json history.jsonl
  riskctl version
`

// errUsage makes run print the usage and exit with status 2.
var errUsage = errors.New("usage")

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	var err error
	switch args[0] {
	case "validate":
		err = cmdValidate(args[1:], stdout, stderr)
	case "eval":
		err = cmdEval(args[1:], stdout, stderr, false)
	case "explain":
		err = cmdEval(args[1:], stdout, stderr, true)
	case "backtest":
		err = cmdBacktest(args[1:], stdout, stderr)
	case "version":
		fmt.Fprintln(stdout, "riskctl", version)
		return 0
	case "help", "-h", "--help":
		fmt.Fprint(stdout, usage)
		return 0
	default:
		err = errUsage
	}
	switch {
	case err == nil:
		return 0
	case errors.Is(err, errUsage):
		fmt.Fprint(stderr, usage)
		return 2
	case errors.Is(err, flag.ErrHelp):
		return 2
	}
	fmt.Fprintln(stderr, "riskctl:", err)
	return 1
}

func newFlags(name string, stderr io.Writer) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	return fs
}

func loadRuleset(path string) (engine.Ruleset, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return engine.Ruleset{}, err
	}
	return engine.ParseRuleset(b)
}

func newStore() *velocity.Store {
	s, _ := velocity.New(velocity.Config{})
	return s
}

// printProblems writes each validation problem with its caret snippet.
func printProblems(w io.Writer, err error) bool {
	var ve *engine.ValidationError
	if !errors.As(err, &ve) {
		return false
	}
	for _, p := range ve.Problems {
		fmt.Fprintln(w, p.Snippet())
	}
	fmt.Fprintf(w, "%d problem(s)\n", len(ve.Problems))
	return true
}

func cmdValidate(args []string, stdout, stderr io.Writer) error {
	fs := newFlags("validate", stderr)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errUsage
	}
	rs, err := loadRuleset(fs.Arg(0))
	if err != nil {
		return err
	}
	c, err := engine.Compile(rs, engine.CompileOptions{MaxWindow: newStore().MaxWindow()})
	if err != nil {
		if printProblems(stdout, err) {
			return errors.New("ruleset is invalid")
		}
		return err
	}
	var enforce, shadow, disabled int
	for _, r := range rs.Rules {
		switch {
		case !r.IsEnabled():
			disabled++
		case r.Mode == engine.Shadow:
			shadow++
		default:
			enforce++
		}
	}
	fmt.Fprintf(stdout, "ok: %s (digest %s): %d rules: %d enforce, %d shadow, %d disabled\n",
		c.Label(), c.Digest, len(rs.Rules), enforce, shadow, disabled)
	var ws []string
	for _, a := range c.Windows() {
		ws = append(ws, a.String())
	}
	fmt.Fprintf(stdout, "velocity lookups: %s\n", strings.Join(ws, ", "))
	return nil
}

func readTx(path string) (*engine.Transaction, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var tx engine.Transaction
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&tx); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &tx, nil
}

func cmdEval(args []string, stdout, stderr io.Writer, trace bool) error {
	fs := newFlags("eval", stderr)
	history := fs.String("history", "", "JSONL history replayed into the feature store first")
	asJSON := fs.Bool("json", false, "print the decision as JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 2 {
		return errUsage
	}
	rs, err := loadRuleset(fs.Arg(0))
	if err != nil {
		return err
	}
	tx, err := readTx(fs.Arg(1))
	if err != nil {
		return err
	}
	eng := engine.New(newStore(), engine.Options{Logger: discard})
	c, err := engine.Compile(rs, eng.CompileOptions())
	if err != nil {
		if printProblems(stdout, err) {
			return errors.New("ruleset is invalid")
		}
		return err
	}
	eng.Load(c)
	if *history != "" {
		f, err := os.Open(*history)
		if err != nil {
			return err
		}
		txs, err := backtest.ReadHistory(f)
		f.Close()
		if err != nil {
			return err
		}
		n := 0
		for i := range txs {
			if txs[i].Time.Before(tx.Time) && txs[i].ID != tx.ID {
				eng.Record(&txs[i])
				n++
			}
		}
		fmt.Fprintf(stderr, "replayed %d prior transactions\n", n)
	}
	d, tr, err := eng.Trace(context.Background(), tx)
	if err != nil {
		return err
	}
	if *asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if trace {
			return enc.Encode(struct {
				Decision engine.Decision `json:"decision"`
				Trace    []engine.Trace  `json:"trace"`
			}{d, tr})
		}
		return enc.Encode(d)
	}
	writeDecision(stdout, d)
	if trace {
		writeTrace(stdout, tr)
	}
	return nil
}

func featureLine(f engine.Feature) string {
	if f.Display != "" {
		return fmt.Sprintf("%s = %s (%s)", f.Name, f.Value, f.Display)
	}
	return fmt.Sprintf("%s = %s", f.Name, f.Value)
}

func writeDecision(w io.Writer, d engine.Decision) {
	fmt.Fprintf(w, "decision  %s", d.Action)
	if d.Action == engine.Hold {
		fmt.Fprintf(w, " for %s", d.HoldFor)
	}
	fmt.Fprintf(w, "   score %d   ruleset %s rev %d\n", d.Score, d.Ruleset, d.Revision)
	for _, r := range d.Reasons {
		fmt.Fprintf(w, "reason    %s\n", r)
	}
	hits := func(title string, hs []engine.Hit) {
		if len(hs) == 0 {
			return
		}
		fmt.Fprintf(w, "\n%s\n", title)
		for _, h := range hs {
			fmt.Fprintf(w, "  %-34s %-7s %+d\n", h.RuleID, h.Action, h.Score)
			for _, f := range h.Features {
				fmt.Fprintf(w, "      %s\n", featureLine(f))
			}
		}
	}
	hits("matched rules", d.Hits)
	hits("shadow rules (not enforced)", d.ShadowHits)
	if d.Shadow != nil {
		fmt.Fprintf(w, "\nwith shadow rules enforced: %s, score %d\n", d.Shadow.Action, d.Shadow.Score)
	}
	for _, e := range d.Errors {
		fmt.Fprintf(w, "error     %s: %s (fail %s)\n", e.RuleID, e.Error, e.FailMode)
	}
}

func writeTrace(w io.Writer, tr []engine.Trace) {
	fmt.Fprintln(w, "\ntrace")
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	for _, t := range tr {
		state := "-"
		switch {
		case !t.Enabled:
			state = "disabled"
		case t.Error != "":
			state = "error: " + t.Error
		case t.Matched:
			state = "MATCH"
		}
		var fs []string
		for _, f := range t.Features {
			fs = append(fs, featureLine(f))
		}
		fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\n", t.RuleID, t.Mode, state, strings.Join(fs, "; "))
	}
	tw.Flush()
}

func cmdBacktest(args []string, stdout, stderr io.Writer) error {
	fs := newFlags("backtest", stderr)
	warmup := fs.String("warmup", "7d", "leading period recorded but not scored")
	promote := fs.Bool("promote-shadow", false, "score shadow rules as enforced")
	recordBlocked := fs.Bool("record-blocked", false, "record blocked transactions into the feature store")
	asJSON := fs.Bool("json", false, "print the report as JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 2 {
		return errUsage
	}
	wu, err := expr.ParseDuration(*warmup)
	if err != nil {
		return fmt.Errorf("-warmup: %w", err)
	}
	rs, err := loadRuleset(fs.Arg(0))
	if err != nil {
		return err
	}
	f, err := os.Open(fs.Arg(1))
	if err != nil {
		return err
	}
	defer f.Close()
	txs, err := backtest.ReadHistory(f)
	if err != nil {
		return err
	}
	start := time.Now()
	rep, err := backtest.Run(context.Background(), rs, txs, backtest.Options{
		Warmup: wu, PromoteShadow: *promote, RecordBlocked: *recordBlocked,
	})
	if err != nil {
		if printProblems(stdout, err) {
			return errors.New("ruleset is invalid")
		}
		return err
	}
	if *asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(rep)
	}
	if err := rep.WriteText(stdout); err != nil {
		return err
	}
	fmt.Fprintf(stderr, "replayed %d transactions in %s\n", len(txs), time.Since(start).Round(time.Millisecond))
	return nil
}

var discard = slog.New(slog.NewTextHandler(io.Discard, nil))
