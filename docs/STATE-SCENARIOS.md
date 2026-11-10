# State and failure scenarios

This document describes how far the service can be tested, which state
transitions are covered by automated tests, and which failure modes still need
live fault injection or code work.

## Test layers

| Layer | What it proves | Where |
|---|---|---|
| Unit tests with fake xray/tester | Manager state machine, promotion, liveness, persistence, catalog, proxy framing | `internal/*/*_test.go` |
| Real-xray integration | Render, preflight, real SOCKS process, HTTP/Speed/Upload/HTTP2 stages | `internal/qualify` (`TestPromoteRealXray`), skipped unless `/usr/local/bin/xray` exists |
| Live lab (separate VM or netns) | Dead/TLS-broken/slow/flapping candidates, DNS and source outages, process kills, restarts | not automated; commands below |
| Production | Real traffic, long-lived connections, real subscription churn | monitor only; do not inject faults without a window |

Current automated suite: **260 tests**. `go test -race` is clean for the
concurrency-heavy packages (`wan`, `proxy`, `qualify`).

Coverage (`go test -cover ./...`):

| Package | Coverage |
|---|---|
| `internal/uri` | 89.8% |
| `internal/catalog` | 86.2% |
| `internal/xraycfg` | 86.1% |
| `internal/proxy` | 82.2% |
| `internal/control` | 81.7% |
| `internal/app` | 80.7% |
| `internal/provider` | 96.8% |
| `internal/qualify` | 78.3% |
| `internal/logging` | 74.8% |
| `internal/wan` | 77.8% |
| `internal/platform` | 73.2% |
| `internal/config` | 72.0% |

The uncovered remainder is mostly real-socket code paths, OS-specific
branches, and defensive error handling.

## State model

The runtime is the product of four independent pieces:

1. **Active xray child** — `alive` or `exited`, with a loopback SOCKS port.
2. **Active tunnel** — the remote WAN can be `working`, `slow`, `tunnel-dead`
   (xray alive, probes fail), or `flapping`.
3. **Ready pool** — zero or more qualified standbys, each with a score.
4. **Catalog** — in-memory entries in `catalog`, `testing`, `failed`, `ready`,
   `active`, or `unsupported`; failed entries are retried after
   `cycle.failed_retry` (30m) unless the failure is transient.

The important transitions are active→dead, active→slow, ready→active,
ready→dead, and catalog→ready.

## Scenario matrix

Automated scenarios live in `internal/wan/scenario_test.go`.

| # | State change | Expected behaviour | Test | Result |
|---|---|---|---|---|
| 1 | Active healthy, ready at target | Cycle returns without testing | `TestScenarioSteadyStateSkipsCycle` | pass |
| 2 | Active xray exits, ready > 0 | Next liveness tick detects it, stops routing to the dead port, promotes a standby | `TestScenarioDeadProcessIsDetectedAndReplaced` | pass |
| 3 | Active xray exits, ready = 0, cycle finds a passer | Cycle promotes it directly instead of only queueing it | `TestScenarioCyclePromotesAfterProcessDeath` | pass |
| 4 | Operator cancels a cycle | In-flight candidates return to `catalog`, not `failed` | `TestScenarioCancelRequeuesInFlightEntries` | pass |
| 5 | One network-wide promotion failure (EOF) | Standbys kept; eviction only after 3 failures | `TestScenarioTransientPromotionFailureKeepsPool` | pass |
| 6 | DNS resolver fails during qualification | Entry stays eligible; no 30m cooldown | `TestScenarioDNSFailureDoesNotBlacklist` | pass |
| 7 | Plain TCP refusal | Entry is marked failed and cooled down | `TestScenarioHardTCPFailureStillBlacklists` | pass |
| 8 | xray alive, tunnel dead | Cycle queues a standby; failover waits for 3 failed probes + cooldown | `TestScenarioTunnelDeadProcessAlive` | pass (known latency) |
| 9 | Operator drops a ready standby | It leaves the pool and is marked `excluded`; the exclusion survives a restart and no cycle requalifies it until restored | `TestScenarioOperatorDropSurvivesRestart` | pass |
| 10 | Active WAN is replaced | The previous active returns to `catalog` (result and score preserved) instead of being retired forever | `TestReplaceRecyclesPreviousActive` | pass |
| 9 | Active tunnel fails, ready > 0 | 3 consecutive probe failures → Replace → standby promoted | `TestProbeFailuresAcrossLivenessIntervalsReplace` | pass |
| 10 | Active speed drops | Two successful below-minimum samples → Replace; probe errors do not replace | `internal/wan` speed tests | pass |
| 11 | Ready standby unreachable | 3 consecutive dial failures → evicted; DNS failures never evict | `TestReadyLivenessDropsFailedAndKeepsGood`, `...KeepsStandbyOnDNSFailure` | pass |
| 12 | Corrupt/missing state file | Starts empty, no crash | `TestLoadStateMissingAndCorrupt` | pass |
| 13 | Restart with persisted active and ready | Active re-promoted, ready restored, proxy resumes | `TestStartRestoresPersistedActive` | pass |
| 14 | Source fetch fails | Last-known-good active and ready preserved | `TestCycleFetchFailureKeepsLastKnownGood` | pass |
| 15 | Concurrent Replace calls | Only one promotion runs | `TestReplaceIsSerialized` | pass |
| 16 | Cycle overruns its queue | 20-minute launch budget; rest stays pending | `TestCycleTimeBudgetLeavesWorkPending` | pass |

## Live fault-injection plan

Run these only on a **separate lab VM** (or a network namespace), never on the
production host. The lab needs the same binary and a fake or real xray.

| Scenario | Injection | Watch for |
|---|---|---|
| Candidate dead | `iptables -I OUTPUT -d <ip> -j DROP` | `tcp:` failure, 30m cooldown, no ready entry |
| TLS broken | reject only port 443 to one server | `tls:` stage failure, no promotion |
| Wrong credentials | corrupt the UUID in one link | `protocol:` failure |
| Verify blocked | block the verify endpoint only | `real: all verify URLs failed` |
| Slow link | `tc qdisc ... netem rate 800kbit` | `speed:` failure; should not enter ready |
| Flapping link | alternate DROP/loss every 10s | liveness threshold, cooldown, no pool wipe |
| Active tunnel dead | drop the active WAN's remote IP | 3 probe failures, Replace, new route |
| Active process crash | `kill -9 <active xray pid>` | next tick, `active xray exited; replacing` |
| DNS outage | point `resolv.conf` at a blackhole | candidates stay `catalog`; no blacklist |
| Source outage | block `raw.githubusercontent.com` | last-known-good active/ready preserved |
| Restart | `systemctl restart dissident` | state restore, brief proxy gap |
| State corruption | truncate `state.json` | clean start, no crash |
| Config reload | `kill -HUP` valid then invalid | valid applies; invalid rolls back |
| Port conflict | start a second instance on 8090/10909 | second exits; existing service untouched |
| Long transfer during replace | download a large file, force Replace | drain window (30s) then broken connection |

Useful checks during a run:

- `/metrics`: `dissident_active_wan`, `dissident_ready_entries`,
  `dissident_cycle_running`, `dissident_last_cycle_timestamp`
- `/wan`: active link, ready count, actions
- access log: `OK`/`FAIL`/`TIMEOUT`/`CANCEL` per connection
- `journalctl -u dissident -f` for `replacing`, `promotion`, `ready link`

## Residual risks

Ordered by expected impact.

1. **Client aborts count as passive WAN failures.** Any non-nil relay error is
   reported, and three reports inside `cycle.probe_window` (8s) trigger a
   replacement. A browser that aborts several connections during a page load
   can therefore churn a healthy route. Filter `CANCEL`/`ECONNRESET`/`EPIPE`
   before `Provider.Report`, or report only dial/upstream errors.
2. **Live-but-dead tunnel is slow to fail over.** With xray still alive, a
   dead tunnel needs 3 probe intervals (45s) plus the 90s replacement cooldown,
   during which a cycle can only queue good standbys. Consider promoting when
   `probeFails >= probe_count` and a ready candidate exists.
3. **Verification endpoints are single points of failure.** `urls.verify`,
   `content`, `upload`, and `speed` all use specific public services. An
   outage or block rejects otherwise good links and puts them in the 30m
   cooldown (DNS failures are now the only transient exception). Add more
   independent endpoints or classify endpoint-side errors as transient.
4. **Speed gate runs against a busy host.** Qualification measures 1MB while up
   to `real_workers` tests may be running; a usable link can measure below
   `min_speed_mbps` and be rejected. Serialize qualification speed or compare
   against a baseline.
5. **Drain is only 30s.** Every replacement stops the old xray after
   `swap.drain_timeout`, so long downloads, video, SSH, and WebSockets are cut.
   Increase the drain window or keep old routes for existing connections.
6. **Catalog is in-memory.** A restart re-fetches subscriptions and loses test
   progress; if the sources are unavailable at restart, only the persisted
   active/ready list survives. Consider persisting the catalog or a seed cache.
7. **Panel and proxy are unauthenticated on `0.0.0.0`.** Anyone on the LAN can
   Replace/Remove/Cancel WANs and use the open proxy. Bind to a private
   interface or add authentication/firewall rules.
8. **No alerting on the states that matter.** `ready_entries == 0`,
   `active_wan == 0`, repeated `replacing`, and source-fetch failures are only
   log lines. Add Prometheus alerts or a heartbeat check.
9. **Process death detection is tick-based.** A crashed child is noticed on the
   next 15s liveness tick. `platform.Xray.Exited` could trigger replacement
   immediately.
10. **Source fetch has no retry/backoff.** A transient GitHub TLS timeout
    makes the cycle fall back to the existing catalog; refill retries every
    ~100s, which is acceptable but noisy.

## Running the automated scenarios

```sh
cd viber
GOCACHE=$PWD/.cache/go-build GOTMPDIR=$PWD/.cache/tmp go test ./internal/wan -run TestScenario -v
GOCACHE=$PWD/.cache/go-build GOTMPDIR=$PWD/.cache/tmp go test -race ./internal/wan ./internal/proxy ./internal/qualify
```
