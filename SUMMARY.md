
## Docker + nginx + Redis scaling (latest result)

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
