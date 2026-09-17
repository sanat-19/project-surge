# Bottleneck Log

Running record of each stage's throughput ceiling, the bottleneck found, and the fix applied.
Final table for Phase 8 gets built from this.

## Phase 0 — Baseline (raw Go service, no Docker/nginx/autoscaler)

**Setup:** `go run main.go`, single instance, stdlib `http.ListenAndServe`, no tuning. `/work` does 5,000 chained sha256 rounds. Machine: 6 performance + 2 efficiency cores (8 total).

**Ceiling found: ~17,000 req/sec**

| Offered rate | Achieved throughput | p50 | p99 | Success |
|---|---|---|---|---|
| 5,000 | 4,999.91 | 416µs | 3.2ms | 100% |
| 15,000 | 14,999.71 | 459µs | 24.5ms | 100% |
| 17,000 | 16,999.60 | 421µs | 26.8ms | 100% |
| 18,000 (run 1) | 15,421.99 | 130.2ms | 276.8ms | 100% |
| 18,000 (run 2) | ~16,074 | — | — | (mean latency 51.6ms) |
| 20,000 | 14,766.43 | 170.8ms | 336.0ms | 100% |

Clean and flat through 17,000 req/sec (p99 in the tens of ms). Past that, throughput doesn't increase further and actually *drops* below the 17,000 level while latency explodes 10-100x — classic saturation: the CPU-bound handler can't drain requests as fast as they arrive, so they queue.

**Run-to-run variance at 18,000:** repeating the identical 18,000 req/sec test twice gave notably different results (mean latency 110.7ms vs. 51.6ms). The tipping point isn't a hard, fixed wall — it shifts depending on system state (leftover `TIME_WAIT` sockets, any queued connections surviving from a prior overloaded run, background load on the machine). Treat **17,000–18,000 req/sec as the practical ceiling band** rather than a single precise number, and always cooldown (~30-60s) between repeated tests at the same rate before comparing results.

**Bottleneck identified:** `main.go`'s `http.Server` is started via plain `http.ListenAndServe` with no `ReadTimeout` / `WriteTimeout` / `IdleTimeout`. Under overload, the server never fails fast or sheds load — it just keeps queueing and eventually answering every request, however long that takes. That's why success ratio stays 100% even at 20,000 offered req/sec while p99 balloons to 336ms: nothing is rejected, it's all just waiting.

**Fix:** deferred to Phase 1 (backend service tuning) — add explicit `http.Server` timeouts, revisit `GOMAXPROCS`/connection handling, and confirm the `intensity=5000` knob is deliberate.

## Phase 1 — Backend service tuning (in progress)

**Change 1: explicit `http.Server` timeouts** (`ReadHeaderTimeout`, `ReadTimeout`, `WriteTimeout=10s`, `IdleTimeout`) in place of bare `http.ListenAndServe`.

Retested at the same offered rate (20,000 req/sec) as the worst Phase 0 case:

| | Phase 0 @ 20,000 | Phase 1 @ 20,000 (with timeouts) |
|---|---|---|
| throughput | 14,766.43 | 15,889.71 |
| mean latency | 164.5ms | 139.9ms |
| p99 | 336.0ms | 355.2ms |
| max | 669.1ms | 939.6ms |
| success | 100% | 100% |

**Result: no change to the throughput ceiling (as expected).** At 20,000 offered, the server is genuinely CPU-bound — every core busy on sha256 rounds — so timeouts can't create more processing capacity; the numbers here are within normal run-to-run variance of Phase 0. What the timeouts *do* provide is a bound on worst-case latency: earlier, under connection-exhaustion contamination, we saw p99 balloon to 6.6s and max to 12.1s with no shedding at all. With `WriteTimeout=10s` in place, that kind of unbounded blowup is now capped — a request that would have hung indefinitely instead gets cut off. This didn't trigger in the clean run above because max latency (939.6ms) stayed well under the 10s threshold.

**Caveat discovered:** `WriteTimeout` only starts counting once the server has accepted the connection and read the request headers — it does not cover time a connection spends waiting in the OS-level accept backlog before Go's `http.Server` even picks it up. So this fix protects against a slow/hung *in-flight* request, but not against backlog buildup from a fully saturated accept queue.

**Change 2: hot-path allocation cleanup** — `doWork` returns `[32]byte` instead of `[]byte` (avoids forcing the hash accumulator onto the heap), Prometheus label vectors (`workDuration`, `workRequestsOK`, `healthRequestsOK`) resolved once at startup instead of via `WithLabelValues(...)` per request, and `fmt.Fprintf(w, "%x\n", ...)` replaced with a direct `encoding/hex.Encode` into a stack buffer + single `Write`.

**Change 3: explicit `GOMAXPROCS`** — set to `runtime.NumCPU()` (8) at startup and logged, rather than relying on the implicit default (which already matches this since Go 1.5). Intent: prove the ceiling is a genuine "all cores maxed" CPU-bound limit before moving to horizontal scale-out, not an accidental software cap.

**Status: inconclusive on whether Changes 2–3 improved anything.** A repeat 17,000 req/sec test after these changes showed *worse* p99 (203.9ms) than the original Phase 0 clean run at the same rate (26.8ms), despite 100% success both times. Root cause not confirmed — candidates are thermal throttling from many repeated sustained CPU-maxing test bursts in one session (macOS `pmset -g therm` was inconclusive on this Apple Silicon hardware), general run-to-run variance (already established as real in Phase 0), or unrelated machine load drift. The planned confirmation step (watching Activity Monitor's per-core CPU history live during a load test, to verify all 8 cores pin near 100%) was not completed before moving on to Phase 2. **Revisit this if Docker-phase numbers look confusing relative to bare-metal** — it may be worth re-running a clean bare-metal comparison after a long cooldown to get an apples-to-apples Phase 1 number.

## Phase 2 — Dockerize + measure the container tax

**Setup:** identical code (Phase 1 version) built via `Dockerfile` (multi-stage: `golang:1.27-alpine` build → `alpine:3.23.5` runtime), run via `docker compose up` on Docker Desktop for macOS. No `--cpus`/`cpuset` limit set — container has full access to all 8 host cores (confirmed: container log reports `GOMAXPROCS=8`, `docker info` reports the Docker Desktop VM itself has 8 CPUs, matching the host exactly).

**Clean control-point comparison at 5,000 req/sec** (both sides hit the offered rate exactly, 100% success — no saturation or client-side artifact on either side):

| | Bare metal | Container | Delta |
|---|---|---|---|
| mean latency | 554µs | 2.8ms | ~5x |
| p50 | 416µs | 1.3ms | ~3x |
| p99 | 3.2ms | 44.5ms | ~14x |
| max | 57.5ms | 282.3ms | ~5x |
| throughput | 4999.91 | 4999.63 | — |

**Bottleneck identified: the container tax is real and substantial, even at light/unsaturated load.** Since CPU count and code are identical, this isn't a compute-capacity issue — it's per-request overhead from the container networking path. The leading suspect: Docker Desktop for macOS doesn't give containers a native Linux bridge network; published-port traffic (`-p 8080:8080`) is routed through the Desktop VM's virtualization/networking layer, adding a real latency cost to every request that wouldn't exist the same way on a native Linux Docker host. This is expected to be a macOS-specific tax on top of whatever "true" Linux cgroup/bridge-networking overhead the project brief anticipates — worth keeping in mind when interpreting all Phase 2+ numbers on this machine, and worth re-verifying if this project ever runs on native Linux.

**Ceiling ramp (container):**

| Offered rate | Achieved throughput | mean | p99 | max | Success |
|---|---|---|---|---|---|
| 5,000 | 4,999.63 | 2.8ms | 44.5ms | 282.3ms | 100% |
| 6,000 | 5,999.55 | 1.9ms | 22.2ms | 73.9ms | 100% |
| 6,500 | 6,499.29 | 3.5ms | 51.2ms | 326.9ms | 100% |
| 7,000 | 6,999.24 | 94.4ms | 460.9ms | 1.0s | 100% |
| 15,000 (offered) | ~7,048 (self-throttled) | 550.1ms | 3.6s | 6.5s | 100% |

**Container ceiling: ~6,000–6,500 req/sec**, vs. bare metal's ~17,000–18,000 (Phase 0/1) — roughly a **2.5–3x reduction** from containerizing identical code with identical CPU access (`GOMAXPROCS=8` confirmed both sides). This is a much sharper drop than the ~5x latency-only tax seen at light 5,000 req/sec load — the container doesn't just add fixed per-request latency, its actual sustainable *throughput* is capped well below bare metal. Consistent with the macOS Docker Desktop networking-virtualization hypothesis: that layer likely becomes a real concurrency/throughput bottleneck under load, not just a fixed per-packet latency add.

## Phase 3 — Horizontal scale-out, static (in progress)

**Setup:** three containers (`surgeP1`/`surgeP2`/`surgeP3`), each pinned to a distinct physical core via `cpuset: "0"/"1"/"2"` in `docker-compose.yml` (affinity-based, not CFS-quota-based `cpus:` — confirmed each container's log reports `GOMAXPROCS=1`, meaning Go itself only sees one core, avoiding CFS-throttling jitter). No nginx yet — each container independently reachable on its own host port (8080/8081/8082) for isolated per-core measurement.

**Per-core ceiling ramp (surgeP1, core 0):**

| Offered rate | Achieved throughput | mean | p99 | max | Success |
|---|---|---|---|---|---|
| 1,000 | 999.84 | 2.1ms | 27.7ms | 52.1ms | 100% |
| 1,200 | 1,199.97 | 7.9ms | 63.0ms | 85.4ms | 100% |
| 1,300 | ~1,262 (already collapsing) | 202.2ms | 257.1ms | 310.8ms | 100% |

**Per-core ceiling: ~1,200 req/sec.** Notably sharper collapse than the multi-core (8-visible-core) tests — clean at 1,200, fully collapsed by 1,300 (25x latency jump in a 100 req/sec step). Consistent with basic queueing theory: a single-server queue (`GOMAXPROCS=1`, no other core to absorb burstiness) has almost no gradual-degradation zone compared to a multi-server pool — once arrival rate exceeds single-core service rate, queue delay blows up almost immediately rather than tapering off.

**Back-of-envelope validation:** the project brief's initial guess was ~5k req/sec/core. Measured reality is **~1,200 req/sec/core** — about 4x lower than assumed. If perfectly linear, 3 cores/containers would suggest a combined ceiling around **~3,600 req/sec** — this is the number to check the upcoming nginx-balanced test against (Phase 3's real question: does horizontal scale-out actually deliver close to linear scaling, or does nginx/shared-network overhead eat into it?).

**Parity check across cores — cores are NOT interchangeable.** Tested `surgeP2` (core 1) and `surgeP3` (core 2) at the same 1,300 req/sec rate that collapsed core 0:

| Core | @ 1,300 req/sec mean latency | Verdict |
|---|---|---|
| 0 (surgeP1) | 202.2ms | collapsed (matches ~1,200 ceiling found earlier) |
| 1 (surgeP2) | 2.5ms | clean — handled 1,300+ with no issue |
| 2 (surgeP3) | 210.7ms | collapsed |

2 of 3 sampled cores show the ~1,200 ceiling; core 1 is a clear positive outlier. Root cause not conclusively identified — candidates include the machine's heterogeneous 6-performance/2-efficiency core architecture, core 0 conventionally absorbing a disproportionate share of interrupt/kernel overhead on Linux, and/or Apple's Virtualization Framework (which backs Docker Desktop's Linux VM on macOS) not guaranteeing a stable 1:1 mapping between a container's pinned `cpuset` index and a specific physical core run-to-run. Not fully diagnosable from inside the VM with tools available here. **Treating ~1,200 req/sec/core as the conservative working number** for later math, and flagging that a "good" core isn't guaranteed to stay in the same place if containers are re-pinned later.

**Next:** add nginx in front of all three containers (switching backends from `ports` to `expose`-only) and re-test through nginx's single address to compare actual combined throughput against the ~3,600 theoretical linear estimate (3 × ~1,200) — keeping in mind the per-core parity issue means real combined throughput could land anywhere from below to above that estimate depending on which physical cores the three containers actually land on.

**Notes on methodology:** back-to-back high-concurrency vegeta runs without a cooldown pause between them produced misleadingly worse numbers (client-side ephemeral port exhaustion from TCP `TIME_WAIT`, plus possible leftover queued connections on the server from the prior overload). Always allow ~30-60s between heavy load-test runs, and scale vegeta's `-max-workers`/`-connections` with the offered rate (roughly `rate × expected_latency`) so the load-test client itself doesn't become the bottleneck being measured.
