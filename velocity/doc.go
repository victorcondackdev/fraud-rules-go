// Package velocity is an in-memory feature store of sliding-window
// counters: how many transfers, how much money and how many distinct
// counterparties a key (payer, payer→beneficiary pair, device, IP) saw in
// the last N minutes, hours or days.
//
// # Layout
//
// Each key holds one ring buffer of time buckets per resolution tier (by
// default 1-minute buckets for 3 hours, 1-hour buckets for a week, 1-day
// buckets for 181 days). Only non-empty buckets are stored, so a key
// active once a day costs a few hundred bytes rather than a full ring. A
// query uses the finest tier able to cover the window and touches at most
// horizon/granularity buckets, so its cost is bounded regardless of
// traffic.
//
// Keys are spread over independently locked shards. Memory is bounded by
// a per-shard key limit with least-recently-written eviction, plus a TTL
// sweep for idle keys.
//
// # Distinct counts
//
// Each bucket keeps the 64-bit hashes of its counterparties exactly up to
// [Config.ExactDistinct] (32 by default), then switches to a HyperLogLog
// sketch ([HLL]). Queries union exact sets when every bucket in the window
// is exact, so distinct counts below the limit are exact; otherwise the
// answer is an estimate with a relative standard error of about 1.04/√2^p
// (1.6% at the default p=12) and [Stats.Exact] is false. Fraud rules
// typically compare distinct counts against small thresholds (3, 5, 10),
// which stay in the exact range.
package velocity
