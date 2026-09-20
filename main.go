package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"runtime"
	"time"

	"github.com/redis/go-redis/v9"
)

const redisKey = "surge:key"

var (
	ctx = context.Background()

	redisClient = newRedisClient(redisAddr())
)

func newRedisClient(addr string) *redis.Client {
	// Explicit pool sizing instead of relying on defaults -- go-redis
	// opens connections lazily (MinIdleConns defaults to 0), so a sudden
	// burst of concurrent requests causes a "connection storm" of many
	// simultaneous new connections (and DNS lookups) at once. Pre-warming
	// a chunk of idle connections avoids that first-load stampede.
	return redis.NewClient(&redis.Options{
		Addr:         addr,
		PoolSize:     500,
		MinIdleConns: 50,
	})
}

func redisAddr() string {
	if v := os.Getenv("REDIS_ADDR"); v != "" {
		return v
	}
	return "redis-master:6379"
}

func workHandler(w http.ResponseWriter, r *http.Request) {
	val, err := redisClient.Get(ctx, redisKey).Result()
	if err != nil {
		http.Error(w, "redis error: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/plain")
	fmt.Fprintln(w, val)
}

func healthHandler(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	fmt.Fprint(w, "OK")
}

func main() {
	// Seed the key so /work always has something to fetch. SET is
	// idempotent here -- safe to run every time a container starts.
	if err := redisClient.Set(ctx, redisKey, "hello-from-redis", 0).Err(); err != nil {
		log.Fatalf("failed to seed redis key: %v", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/work", workHandler)
	mux.HandleFunc("/health", healthHandler)

	addr := ":8080"
	srv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       5 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	log.Printf("surge listening on %s (GOMAXPROCS=%d, redis addr=%s)", addr, runtime.GOMAXPROCS(0), redisAddr())
	log.Fatal(srv.ListenAndServe())
}
