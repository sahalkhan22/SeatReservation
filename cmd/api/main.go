package main

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"seatlock/internal/config"
	"seatlock/internal/store"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	cfg, err := config.Load()
	if err != nil {
		log.Error("invalid configuration", "err", err)
		os.Exit(1)
	}
	if cfg.UnsafeMode {
		log.Warn("UNSAFE_MODE is on: seat locking is disabled, double-sells are expected")
	}

	// Cancelled on SIGINT/SIGTERM, which drives graceful shutdown below.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	db, err := store.Open(ctx, cfg.DatabaseURL, cfg.DBPoolMax, cfg.QueryTimeout)
	if err != nil {
		log.Error("cannot reach database", "err", err)
		os.Exit(1)
	}
	defer db.Close()
	log.Info("database connected", "pool_max", cfg.DBPoolMax)

	mux := http.NewServeMux()

	// Liveness. Deliberately does NOT touch the database: if it did, a brief
	// Postgres blip would make the orchestrator kill a healthy process.
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})

	// Readiness. Fails closed, so the load balancer stops sending traffic
	// while the process stays alive long enough to recover.
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		if err := db.Ping(r.Context()); err != nil {
			log.Warn("readiness failed", "err", err)
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{
				"status": "unready", "reason": "db_unavailable",
			})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
	})

	srv := &http.Server{
		Addr:              ":" + cfg.Port,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	go func() {
		log.Info("listening", "addr", srv.Addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("server failed", "err", err)
			stop()
		}
	}()

	<-ctx.Done()

	// Let in-flight requests finish. Without this, a deploy would abort
	// transactions mid-flight and clients would see connection resets.
	log.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Error("graceful shutdown failed", "err", err)
	}
}

func writeJSON(w http.ResponseWriter, code int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(body)
}
