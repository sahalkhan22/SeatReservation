package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"seatlock/internal/api"
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

	srv := &http.Server{
		Addr:              ":" + cfg.Port,
		Handler:           api.New(cfg, db, log).Routes(),
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

	// Let in-flight requests finish. Without this a deploy aborts open
	// transactions mid-flight and clients see connection resets.
	log.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Error("graceful shutdown failed", "err", err)
	}
}
