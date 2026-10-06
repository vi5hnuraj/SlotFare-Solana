package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

type stubPinger func(context.Context) error

func (s stubPinger) Ping(ctx context.Context) error { return s(ctx) }

func TestHealthHandlerIsLivenessOnly(t *testing.T) {
	// Liveness is the process, not the database: a store outage must not
	// kill or restart the pod, so /health answers 200 even when the Pinger
	// fails.
	bad := healthHandler(stubPinger(func(context.Context) error { return errors.New("db down") }), 2*time.Second)

	t.Run("ok", func(t *testing.T) {
		rec := httptest.NewRecorder()
		bad.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/health", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
		}
		body, _ := io.ReadAll(rec.Result().Body)
		if string(body) != `{"status":"ok"}`+"\n" {
			t.Fatalf("body = %q", body)
		}
	})

	t.Run("method not allowed", func(t *testing.T) {
		rec := httptest.NewRecorder()
		bad.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/health", nil))
		if rec.Code != http.StatusMethodNotAllowed {
			t.Fatalf("status = %d, want %d", rec.Code, http.StatusMethodNotAllowed)
		}
	})
}

func TestReadyHandlerFlipsOnStoreLoss(t *testing.T) {
	ok := readyHandler(stubPinger(func(context.Context) error { return nil }), 2*time.Second)
	bad := readyHandler(stubPinger(func(context.Context) error { return errors.New("db down") }), 2*time.Second)

	t.Run("ready", func(t *testing.T) {
		rec := httptest.NewRecorder()
		ok.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/ready", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
		}
		body, _ := io.ReadAll(rec.Result().Body)
		if string(body) != `{"status":"ok","ready":true}`+"\n" {
			t.Fatalf("body = %q", body)
		}
	})

	t.Run("unready on store loss", func(t *testing.T) {
		rec := httptest.NewRecorder()
		bad.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/ready", nil))
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("status = %d, want %d", rec.Code, http.StatusServiceUnavailable)
		}
		body, _ := io.ReadAll(rec.Result().Body)
		if string(body) != `{"status":"error","ready":false}`+"\n" {
			t.Fatalf("body = %q", body)
		}
	})

	t.Run("method not allowed", func(t *testing.T) {
		rec := httptest.NewRecorder()
		ok.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/ready", nil))
		if rec.Code != http.StatusMethodNotAllowed {
			t.Fatalf("status = %d, want %d", rec.Code, http.StatusMethodNotAllowed)
		}
	})
}
