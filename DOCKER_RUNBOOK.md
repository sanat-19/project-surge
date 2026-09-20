# Docker Compose runbook (nginx + surge + Redis)

Rebuild the manual-scaling baseline. Results and analysis are in `SUMMARY.md`.

## Prerequisites
Docker Desktop (8 CPUs), `oha` (`brew install oha`).

## Stack (`docker-compose.yml`)
- `redis-master`: `redis:7-alpine`, `cpus: 1`, plain `redis-server` (no `--io-threads`; it made throughput worse).
- `surge-1..4`: image `surge` built from the `Dockerfile`, container names `surgeP1..P4`, each `cpus: 0.5` and `GOMAXPROCS=1`. This is the unit of scaling.
- `nginx`: `nginx:alpine`, container `nginx-lb`, `cpus: 2`, published on host port 80, config mounted from `nginx.conf`, `sysctls: net.ipv4.ip_local_port_range=1024 65535`.

`nginx.conf` essentials: `worker_processes 2`, `worker_connections 20000`, upstream with `least_conn` and `keepalive 2048`, and in the `location` block `proxy_http_version 1.1` plus `proxy_set_header Connection ""` (required for upstream keepalive). Without keepalive, nginx opens a new upstream connection per request and runs out of ephemeral ports (`[crit] 99: Address not available`).

## 1. Start
Stop the Kubernetes stack first so they do not compete for CPU (`minikube stop`).
```bash
docker compose up -d --build
```
`--build` rebuilds the `surge` image. Check with `docker compose ps`.

## 2. Verify
```bash
curl -s localhost:80/work
```
It should print `hello-from-redis`.

## 3. Scale (manual)
The number of surge containers is set by which `surge-N` services are enabled. For fewer than 4:
1. In `docker-compose.yml`, comment out the unused `surge-N` services and their entries under nginx `depends_on`.
2. In `nginx.conf`, comment out the matching `server surgePN:8080;` lines in the upstream block.
3. Apply, then restart nginx (it caches upstream IPs at startup, so recreated backends silently get no traffic until nginx restarts):
```bash
docker compose up -d
docker compose restart nginx
```

## 4. Load test
```bash
oha -z 20s -c 1500 -q 50000 --no-tui http://localhost:80/work
```
Run it 1, 2, 3 and 4 containers, same command each time. Wait about 60s between runs. `oha` runs on the host, so it competes for CPU with the Docker VM. Do not use vegeta: it used enough CPU to hide about a third of the capacity.

Measured: 1 container about 12.9k, 2 about 22.6k, 3 about 31.1k, 4 about 31.3k (plateau: machine CPU is the limit).

## 5. Watch
```bash
docker stats
```
The percent is per single core, so a `cpus: 0.5` container tops out at 50%. Read it mid-run. For Redis load: `docker exec redis-master redis-cli info stats | grep instantaneous_ops_per_sec` (only meaningful while load is running).

## 6. Tear down
```bash
docker compose down
```

## Gotchas
- `cpuset` pins a container to a core but does not reserve it; use `cpus:` quotas and give nginx its own budget.
- Docker Desktop on macOS adds a networking layer between host and containers; host-side load numbers include that tax.
- Redis Cluster sharding cannot scale a single hot key (all reads land on one node).
- Redis with `--io-threads` made throughput worse when the machine was already CPU-bound.
- Restart nginx after recreating backend containers.
