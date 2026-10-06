package main

import (
	"context"
	"encoding/json"
	"net/http"
	"time"
)

// Pinger is the narrow view of the store /ready needs: is the database
// reachable? Both meter.Store drivers satisfy it.
type Pinger interface {
	Ping(ctx context.Context) error
}

// healthHandler serves GET /health for the k8s liveness probe: the process
// is up, and it always answers 200 with {"status":"ok"}. A store outage
// must not kill or restart the pod - the background loops skip the ticks
// they cannot run, and the /ready probe routes traffic around the replica
// instead. The Pinger and timeout are kept in the signature so both probes
// wire identically; liveness never consults them.
func healthHandler(_ Pinger, _ time.Duration) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	})
}

// readyHandler serves GET /ready for the k8s readiness probe: it answers
// 200 with {"status":"ok","ready":true} when the storage Ping succeeds
// within timeout and 503 with {"status":"error","ready":false} otherwise.
// A replica that cannot reach Postgres reports unready, so k8s routes
// around it without restarting it. The 2 second budget keeps a slow
// database from stalling the probe's own deadline.
func readyHandler(p Pinger, timeout time.Duration) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), timeout)
		defer cancel()
		err := p.Ping(ctx)
		w.Header().Set("Content-Type", "application/json")
		if err != nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			_ = json.NewEncoder(w).Encode(struct {
				Status string `json:"status"`
				Ready  bool   `json:"ready"`
			}{"error", false})
			return
		}
		_ = json.NewEncoder(w).Encode(struct {
			Status string `json:"status"`
			Ready  bool   `json:"ready"`
		}{"ok", true})
	})
}
