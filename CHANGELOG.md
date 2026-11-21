# Changelog

All notable changes to this project are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and the project
adheres to [Semantic Versioning](https://semver.org/).

## [Unreleased]

## [0.1.0] - 2026-11-21

First public release.

### Added

- `expr`: rule language with a hand-written lexer, Pratt parser, type
  checker and closure compiler. Types int (cents), bool, string, duration,
  time, string/int lists and velocity streams; arithmetic, comparison,
  `&&`/`||`/`!`, `in`/`not in`, wrap-around `between`, `startsWith`,
  `endsWith`, `contains`; builtins `hour`, `weekday`, `isNight`, `age`,
  `len`, `count`, `sum`, `distinct`. Positional errors with caret snippets,
  static cost budget, nesting and size limits, embedded tzdata.
- `velocity`: sharded, memory-bounded sliding-window store with tiered
  time buckets (1m/1h/1d by default), LRU eviction and TTL sweep; exact
  distinct counts falling back to an HLL++-style sketch with documented and
  tested error bounds.
- `engine`: rulesets with ALLOW, REVIEW, STEP_UP, HOLD and BLOCK actions,
  additive scores and thresholds, enforce and shadow modes, per-rule and
  per-action fail-open/fail-closed, latency budget, per-rule feature
  explanations, atomic hot swap with revision history and rollback.
- `backtest`: labeled JSONL replay with precision, recall, value recall,
  false-positive rate and unique catches per rule, per pattern and overall;
  warm-up period and shadow promotion.
- `cmd/riskd`: HTTP service (evaluate, events, rulesets, rollback, rule
  validation), Prometheus metrics, JSON logs, SIGHUP reload.
- `cmd/riskctl`: `validate`, `eval`, `explain` and `backtest`.
- `cmd/gen-history`: deterministic synthetic history with account
  takeover, mule fan-out, scam fan-in and structuring patterns; committed
  sample in `testdata/`.
- `rulesets/pix-default.json`: 32 illustrative rules for outgoing Pix.

[Unreleased]: https://github.com/victorcondackdev/fraud-rules-go/compare/v0.1.0...HEAD
[0.1.0]: https://github.com/victorcondackdev/fraud-rules-go/releases/tag/v0.1.0
