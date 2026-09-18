package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log"
	"net/http"
	"runtime"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// intensity is the number of chained sha256 rounds performed per /work request.
// This is the dial for how expensive each request is.
const intensity = 5000

var (
	requestsTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "surge_requests_total",
			Help: "Total number of HTTP requests, by path and status.",
		},
		[]string{"path", "status"},
	)

	requestDuration = promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "surge_request_duration_seconds",
			Help:    "Request duration in seconds, by path.",
			Buckets: prometheus.DefBuckets,
		},
		[]string{"path"},
	)

	// Label values are fixed per handler, so resolve them once at startup
	// instead of paying a map lookup on every request.
	workDuration     = requestDuration.WithLabelValues("/work")
	workRequestsOK   = requestsTotal.WithLabelValues("/work", "200")
	healthRequestsOK = requestsTotal.WithLabelValues("/health", "200")
)

func doWork(rounds int) [32]byte {
	sum := sha256.Sum256([]byte("surge"))
	for i := 0; i < rounds; i++ {
		sum = sha256.Sum256(sum[:])
	}
	return sum
}

func workHandler(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	sum := doWork(intensity)
	workDuration.Observe(time.Since(start).Seconds())
	workRequestsOK.Inc()

	var buf [2*len(sum) + 1]byte
	hex.Encode(buf[:2*len(sum)], sum[:])
	buf[2*len(sum)] = '\n'

	w.Header().Set("Content-Type", "text/plain")
	w.Write(buf[:])
}

func healthHandler(w http.ResponseWriter, r *http.Request) {
	healthRequestsOK.Inc()
	w.WriteHeader(http.StatusOK)
	fmt.Fprint(w, "OK")
}

func main() {
	// No explicit runtime.GOMAXPROCS() call here -- Go's runtime already
	// reads the GOMAXPROCS env var at startup if set, and only falls back
	// to NumCPU() otherwise. In a container with a CPU limit, NumCPU()
	// reports the host's total cores, not the limit, so the env var (set
	// per-deployment) is what should actually control this.

	mux := http.NewServeMux()
	mux.HandleFunc("/work", workHandler)
	mux.HandleFunc("/health", healthHandler)
	mux.Handle("/metrics", promhttp.Handler())

	addr := ":8080"
	srv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       5 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	log.Printf("surge listening on %s (intensity=%d, GOMAXPROCS=%d)", addr, intensity, runtime.GOMAXPROCS(0))
	log.Fatal(srv.ListenAndServe())
}
