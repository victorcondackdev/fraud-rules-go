package backtest

import (
	"fmt"
	"io"
	"log/slog"
	"strings"
	"text/tabwriter"

	"github.com/victorcondackdev/fraud-rules-go/engine"
)

var discardLogger = slog.New(slog.NewTextHandler(io.Discard, nil))

func pct(f float64) string { return fmt.Sprintf("%.1f%%", 100*f) }

// WriteText renders the report as aligned plain-text tables.
func (r *Report) WriteText(w io.Writer) error {
	var b strings.Builder
	fmt.Fprintf(&b, "ruleset        %s\n", r.Ruleset)
	fmt.Fprintf(&b, "transactions   %d scored (%d warm-up)\n", r.Transactions, r.Warmup)
	fmt.Fprintf(&b, "fraud          %d (%s), %s\n", r.Fraud, pct(ratio(r.Fraud, r.Transactions)), engine.FormatBRL(r.FraudAmount))
	fmt.Fprintf(&b, "legit          %d, %s\n", r.Legit, engine.FormatBRL(r.LegitAmount))
	fmt.Fprintf(&b, "mean eval      %.1f µs\n\n", float64(r.MeanEvalNs)/1e3)

	tw := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "overall\tprecision\trecall\tvalue recall\tFP rate\tlegit value stopped")
	for _, row := range []struct {
		name string
		c    Confusion
	}{{"flagged (≠ALLOW)", r.Flagged}, {"interdicted (BLOCK|HOLD)", r.Interdicted}} {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", row.name, pct(row.c.Precision()), pct(row.c.Recall()),
			pct(row.c.AmountRecall()), pct(row.c.FalsePositiveRate()), engine.FormatBRL(row.c.FPAmount))
	}
	tw.Flush()

	b.WriteString("\nactions       ")
	for a := engine.Allow; a <= engine.Block; a++ {
		fmt.Fprintf(&b, " %s=%d", a, r.Actions[a])
	}
	b.WriteString("\n\n")

	tw = tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "rule\tmode\taction\thits\tTP\tFP\tprecision\trecall\tunique\thit volume")
	for _, rs := range r.Rules {
		c := rs.Confusion
		fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%d\t%d\t%s\t%s\t%d\t%s\n", rs.ID, rs.Mode, rs.Action, rs.Hits,
			c.TP, c.FP, pct(c.Precision()), pct(c.Recall()), rs.Unique, engine.FormatBRL(c.TPAmount+c.FPAmount))
	}
	tw.Flush()

	if len(r.Patterns) > 0 {
		b.WriteString("\n")
		tw = tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "fraud pattern\ttotal\tflagged\tinterdicted")
		for _, p := range r.Patterns {
			fmt.Fprintf(tw, "%s\t%d\t%d (%s)\t%d (%s)\n", p.Pattern, p.Total,
				p.Flagged, pct(ratio(p.Flagged, p.Total)), p.Interdicted, pct(ratio(p.Interdicted, p.Total)))
		}
		tw.Flush()
	}
	_, err := io.WriteString(w, b.String())
	return err
}
