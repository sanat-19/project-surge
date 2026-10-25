
# Project Surge: scaling a Go service from 12.9k to 60k req/sec

Single 8-core laptop (Apple Silicon, Docker Desktop VM), Go HTTP service reading one Redis key per request. Load generator: `oha`.

| Stage | Throughput | How |
|---|---|---|
| 1 container (0.5 vCPU) behind nginx | 12.9k req/sec | baseline |
| 4 containers, manual scaling | 31.3k req/sec (plateau) | nginx + machine CPU became the limit |
| Kubernetes + HPA, in-cluster load | **60k req/sec sustained**, 100% success, ~1.2ms avg latency | autoscaled 1 to 6 pods, no manual steps, no nginx |

Scaling was near-linear (~15.5k req/sec per 0.5-vCPU pod), not exponential. 60k is the rate cap I set on the load generator, not a measured ceiling.

## Docker + nginx + Redis scaling

Workload: `/work` reads one key from Redis. Stack: nginx (`least_conn`, upstream keepalive) -> N surge containers -> Redis. Each surge container = 0.5 vCPU (`cpus: 0.5`) with `GOMAXPROCS=1`. Load generator: `oha` on the same 8-core laptop.

| Surge containers (0.5 vCPU each) | Req/sec | Per container | Notes |
|---|---|---|---|
| 1 | 12.9k | 12.9k | Surge saturated at its quota; nginx/Redis mostly idle |
| 2 | 22.6k | 11.3k | 1.75x |
| 3 | 31.1k | 10.4k | 2.4x (~80% scaling efficiency), 100% success, p99 ~121ms |
| 4 | 31.3k | 7.8k | Plateau: surge tier no longer the limiter |

Capacity per core is about 25-26k req/sec on this workload (1 core: ~24.5k, 0.5 core: ~12.9k).

Saturation: nginx ~75% of its quota, Redis ~75% of its core, surge at 100% of quota, plus the load generator and Docker VM on the same 8 cores. Giving Redis 2 cores with I/O threads made throughput worse (31k -> 25k), which confirms the machine as a whole is out of CPU.

Bottlenecks found and fixed:
- nginx opened a new upstream connection per request -> ephemeral port exhaustion (`[crit] 99: Address not available`); fixed with upstream `keepalive` + HTTP/1.1 upstream.
- go-redis connection storm under sudden concurrency; fixed with explicit pool size and pre-warmed idle connections.
- Redis Cluster sharding cannot scale a single hot key (all 600k GETs landed on one node, verified with `cmdstat_get`).
- `GOMAXPROCS` mismatch under a CPU quota; set to 1 for sub-1-core containers (3x better per-core efficiency than the default of 8).
- CFS quota throttling shows up as a latency bump at 50-60ms.
- vegeta consumed enough CPU to hide capacity (~19k with vegeta vs ~30k with oha on the same stack).

Caveat for quoting: all numbers are from a single 8-core laptop with the load generator on the same machine.

## Kubernetes + HPA (minikube)

Setup: minikube (Docker driver, 8 CPUs, 6 GB). Surge pod = 500m CPU request and limit, `GOMAXPROCS=1`, readiness probe on `/health`. Redis 1 CPU. `Service` (kube-proxy) replaces nginx. HPA: min 1, max 6, CPU target 65% of the 500m request, scale-up stabilization 0s, scale-down 60s. Load: `oha` run as a pod inside the cluster against the Service DNS name.

**Autoscaling result** (30s bursts, 1500 connections, rate cap 60k/s, HPA starting from 1 pod):

| Bursts | Req/sec | Avg latency | State |
|---|---|---|---|
| 1-3 | ~15.5k | ~96ms | 1 pod, saturated |
| 4-7 | ~30.5k | ~49ms | 2 pods, saturated |
| 8-10 | ~49.5k | ~30ms | 3 pods, saturated |
| 11-16 | 59.97k (the cap) | 1.1-1.4ms | capacity exceeds demand |

All 16 bursts: 100% success. HPA went 1 -> 2 -> 3 -> 5 -> 6 about every 2 minutes, then back to 1 after the load stopped. Fixed 3 pods (60s run, no HPA): 48k req/sec. Per-pod capacity ~15.5k (vs ~12.9k per 0.5-vCPU container behind nginx).

Bottlenecks found:
- **Host path capped the test at ~10k.** Going through the published NodePort (`localhost:30080`) from the Mac reached only ~10.2k; `minikube service --url` broke above ~3k; `kubectl port-forward` pins to one pod. The Docker Desktop port mapping, not the pods, was the limit. The same pods served 48k when load ran inside the cluster.
- **Long-lived connections defeat scale-out.** kube-proxy balances per TCP connection. A single 300s `oha` run opened 1500 keep-alive connections while there was 1 pod; the HPA added a second pod, but no existing connection moved to it (new pod ~1m CPU, old pod ~501m). Average CPU across the two (~50%) stayed under the 65% target, so scaling stopped at 2 pods. Fixed by testing with repeated short bursts that reconnect, so new pods receive traffic. Production systems handle this with L7 balancing or connection recycling.
- **metrics-server** rejected the kubelet's self-signed cert on minikube (`<unknown>` CPU); fixed with `--kubelet-insecure-tls`.
- **`GOMAXPROCS` under a CFS quota:** `NumCPU()` reports the node's cores, not the pod's quota; set explicitly to 1.
- **HPA target too low** (10-30%) scaled on baseline noise; 65% of the request behaves sensibly.
- **HPA reaction lag:** about 100-120s per scale-up step (metrics-server sampling plus the 15s sync), roughly 5 minutes to go from 1 pod to a pool that fully served 60k. Latency was 30-100ms during that window.

## What Kubernetes gained over Docker here (and what it did not)

The gain is mostly from removing nginx (~1.5-2 cores at the Docker plateau) and the Docker Desktop host path from the request path, plus automatic scaling instead of a fixed container count. It is not "Kubernetes is 60% faster than Docker". The Docker baseline had `oha` on the host and the Kubernetes runs had `oha` inside the cluster, so the two are not perfectly like for like.

Known limits of the measurements:
- 60k is the load generator's rate cap; the real ceiling was not measured (bursts at 60k still had ~1ms latency, so headroom remains).
- p99 latency was not recorded for the 60k runs.
- Load generator shares the node's 8 cores with the pods and Redis; single hot Redis key; no edge load balancer or ingress in the path; single laptop.
- Only one replica count was inferred per burst from HPA events, not logged directly.