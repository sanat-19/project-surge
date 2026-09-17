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

**Notes on methodology:** back-to-back high-concurrency vegeta runs without a cooldown pause between them produced misleadingly worse numbers (client-side ephemeral port exhaustion from TCP `TIME_WAIT`, plus possible leftover queued connections on the server from the prior overload). Always allow ~30-60s between heavy load-test runs, and scale vegeta's `-max-workers`/`-connections` with the offered rate (roughly `rate × expected_latency`) so the load-test client itself doesn't become the bottleneck being measured.
