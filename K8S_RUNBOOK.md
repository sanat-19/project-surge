# Kubernetes runbook (minikube + HPA)

Rebuild the autoscaling demo from scratch. Results and analysis are in `SUMMARY.md`.

## Prerequisites
Docker Desktop (8 CPUs, 8 GB given to its VM), `minikube`, `kubectl`, `oha` (`brew install oha` if you also want host-side tests).

## Files (`k8s/`)
- `redis.yaml`: `redis-master` Deployment (1 CPU, plain `redis-server`, no io-threads) and Service. The Service name must stay `redis-master` (main.go defaults to `redis-master:6379`, and surge exits at startup if Redis is unreachable).
- `deployment.yaml`: `surge`, image `surge:latest`, `imagePullPolicy: Never`, request = limit = `500m`, `GOMAXPROCS=1`, readiness probe on `/health`.
- `service.yaml`: NodePort 30080 -> 8080.
- `hpa.yaml`: min 1, max 6, CPU target 65% of the request, scale-up stabilization 0s, scale-down 60s.

## 1. Start the cluster
```bash
minikube start --driver=docker --cpus=8 --memory=6144 --ports=30080:30080
```
`--ports` only takes effect at creation. It publishes the NodePort to the host; you only need it for host-side curls. It is not used for load tests.

## 2. Build and load the image
Minikube has its own image store, separate from Docker Desktop's.
```bash
docker build -t surge:latest .
minikube image load surge:latest
```
Re-run both after any change to `main.go`, then `kubectl rollout restart deployment/surge`.

## 3. Metrics for the HPA
```bash
minikube addons enable metrics-server
kubectl patch deployment metrics-server -n kube-system --type='json' -p='[{"op":"add","path":"/spec/template/spec/containers/0/args/-","value":"--kubelet-insecure-tls"}]'
```
If the patch says `not found`, wait about 10s and retry. Without it `kubectl get hpa` shows `<unknown>` forever.

## 4. Deploy
```bash
kubectl apply -f k8s/
kubectl get pods
kubectl get hpa surge
```
Wait for the surge pod `1/1 Running` and a real CPU value in the HPA (about 1 minute). Sanity check: `curl -s localhost:30080/work` prints `hello-from-redis`.

## 5. Watch (separate terminals)
```bash
kubectl get hpa surge -w
kubectl get pods -w
kubectl top pods
```
`kubectl top` prints millicores (a 500m pod pinned at its limit shows about `500m`) and lags 30-60s. The HPA's percentage is usage / request, so a pinned pod reads about 100%.

## 6. Load test (from inside the cluster)
Do NOT use `localhost:30080`, `minikube service --url` or `kubectl port-forward` for load. They cap at about 10k / 3k / one pod. Run `oha` as a pod against the Service DNS name.

Use repeated short bursts. kube-proxy balances per TCP connection, so a single long run opens its connections on the first pod and new pods never receive traffic (the HPA stalls at 2 pods).
```bash
for i in $(seq 1 16); do echo "burst $i"; kubectl run oha-$i --rm -i --restart=Never --image=ghcr.io/hatoo/oha:latest -- -z 30s -c 1500 -q 60000 --no-tui http://surge/work | grep -E "Requests/sec|Average:|Success rate"; done
```
`-q` is a cap on the send rate, not a target. Change it to test other loads (60000 fits in about 5-6 pods on an 8-core laptop). To also get the tail latency, change the grep to `"Requests/sec|Average:|Success rate|99.00%"`.

Expected: about 15.5k req/sec per 0.5-vCPU pod; HPA steps 1 -> 2 -> 3 -> 5 -> 6 about every 2 minutes; 60k at about 1.2ms once enough pods exist. After the load stops, it scales down after 60s.

To check the scaling history: `kubectl describe hpa surge`.

## 6b. Load test from outside the cluster (host Mac)
This is how an external client reaches the app. On this Mac it is capped by the host path, not the pods (measured: about 10k req/sec through `localhost:30080`, while the same pods served 48-60k from inside). Use it to check reachability and to see the realistic client path, not to measure pod capacity.

Requirements: the cluster was created with `--ports=30080:30080` (step 1) and `k8s/service.yaml` has `nodePort: 30080`.

Check the path first (should print `hello-from-redis`):
```bash
curl -s localhost:30080/work
```

Run `oha` on the host, in short bursts so each burst reconnects (a single long run keeps its keep-alive connections on the first pod, and the HPA stalls at 2 pods):
```bash
for i in $(seq 1 16); do echo "burst $i"; oha -z 30s -c 1000 -q 10000 --no-tui http://localhost:30080/work | grep -E "Requests/sec|Average:|Success rate"; done
```
Keep `-q` at or below about 10000. Above that you saturate the Docker Desktop port mapping and just measure it. Measured results through this path: a 300s run at `-c 1500` gave 10.2k req/sec, 147ms average latency, 100% success.

Other ways to reach the Service from the host, and why they are worse:
- `minikube service surge --url` starts a local tunnel process on a random port. It broke above about 3k req/sec (mass `connection reset by peer`) and the terminal must stay open.
- `kubectl port-forward service/surge 8080:80` forwards to ONE pod, not the pool, so it never shows load balancing or autoscaling.

Because the host path tops out near 10k, which one pod almost serves (about 15.5k per pod), you will not see the HPA scale beyond a pod or two from outside on this machine. For scaling to 5-6 pods use the in-cluster test in step 6. In production, external clients come through an Ingress or cloud load balancer that scales out itself, which is what this Mac setup cannot reproduce.

## 7. Manual scaling / fixed pod count
The HPA overrides manual scaling on its next reconcile. Delete it first:
```bash
kubectl delete hpa surge
kubectl scale deployment surge --replicas=3
```
Re-apply later with `kubectl apply -f k8s/hpa.yaml`.

## 8. Tear down
Remove only the app (keeps the cluster, image and metrics-server):
```bash
kubectl delete -f k8s/
```
Stop the cluster, keep its state (fast to resume):
```bash
minikube stop
```
Delete the cluster entirely (frees the VM's CPU and disk; you must redo steps 1-4):
```bash
minikube delete
```

## Gotchas
- Stop `docker compose` first, so the two stacks do not compete for CPU.
- Kubernetes CPU limits are CFS quotas, so `GOMAXPROCS` must be set explicitly (`NumCPU()` reports the node's cores).
- Sub-1-core pods show a 50-60ms latency bump from CFS throttling.
- HPA targets of 10-30% scale on baseline noise; use about 65%.
- HPA reacts in about 100-120s per step (metrics-server sampling plus 15s sync); scale-up is allowed to jump several pods at once.
- The dashboard (`minikube dashboard`) adds pods and CPU load; close it during measurements.
