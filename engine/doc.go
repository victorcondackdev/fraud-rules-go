// Package engine evaluates Pix transfers against a ruleset and returns an
// explained decision.
//
// A [Ruleset] is a versioned JSON document of rules. Each [Rule] has an
// expression in the language of package expr, an [Action] (ALLOW, REVIEW,
// STEP_UP, HOLD, BLOCK), a score weight, a [Mode] (enforce or shadow) and a
// [FailMode] for when it cannot be evaluated. [Compile] validates the whole
// document up front and reports every problem with its position, so a bad
// ruleset never reaches traffic.
//
// [Engine.Evaluate] runs every enabled rule and aggregates the result: the
// most severe action among matched enforced rules, escalated further when
// the summed score crosses the ruleset thresholds. Every matched rule
// carries the value of each feature its expression read — amounts, local
// hour, device age, velocity counts — so analysts and customers can be told
// exactly why a transfer was held. Shadow rules run the same way but are
// reported separately, together with the outcome the decision would have
// had if they were enforced.
//
// Rulesets are hot-swapped atomically ([Engine.Load]); in-flight
// evaluations finish on the revision they started with, and earlier
// revisions are kept for [Engine.Rollback].
//
// Velocity features come from a [FeatureStore], normally a
// [velocity.Store]. Record transfers with [Engine.Record] when they settle.
// Within one evaluation, velocity lookups are memoized per (stream,
// window), no matter how many rules read them.
package engine
