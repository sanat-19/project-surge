package main

import (
	"crypto/sha256"
	"fmt"
	"log"
	"net/http"
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
)

func doWork(rounds int) []byte {
	sum := sha256.Sum256([]byte("surge"))
	for i := 0; i < rounds; i++ {
		sum = sha256.Sum256(sum[:])
	}
	return sum[:]
}

func workHandler(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	result := doWork(intensity)
	requestDuration.WithLabelValues("/work").Observe(time.Since(start).Seconds())
	requestsTotal.WithLabelValues("/work", "200").Inc()

	w.Header().Set("Content-Type", "text/plain")
	fmt.Fprintf(w, "%x\n", result)
}

func healthHandler(w http.ResponseWriter, r *http.Request) {
	requestsTotal.WithLabelValues("/health", "200").Inc()
	w.WriteHeader(http.StatusOK)
	fmt.Fprint(w, "OK")
}

func main() {
	mux := http.NewServeMux()
	mux.HandleFunc("/work", workHandler)
	mux.HandleFunc("/health", healthHandler)
	mux.Handle("/metrics", promhttp.Handler())

	addr := ":8080"
	log.Printf("surge listening on %s (intensity=%d)", addr, intensity)
	log.Fatal(http.ListenAndServe(addr, mux))
}
