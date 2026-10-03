// Package api is the HTTP layer. It knows nothing about SQL: it decodes
// requests, calls the store, and maps domain errors onto status codes.
package api

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"seatlock/internal/config"
	"seatlock/internal/metrics"
	"seatlock/internal/store"
)

type Server struct {
	cfg config.Config
	db  *store.Store
	log *slog.Logger
}

func New(cfg config.Config, db *store.Store, log *slog.Logger) *Server {
	return &Server{cfg: cfg, db: db, log: log}
}

// statusFor is the single place an error becomes a status code.
//
// Every domain outcome is a 4xx. Anything that falls through to the default is
// a genuine server fault, and under load the project target is that this never
// happens: overload surfaces as 503 from the pool, not 500 from a panic.
func statusFor(err error) (int, string) {
	switch {
	case errors.Is(err, store.ErrShowNotFound):
		return http.StatusNotFound, "show_not_found"
	case errors.Is(err, store.ErrReservationNotFound):
		return http.StatusNotFound, "reservation_not_found"
	case errors.Is(err, store.ErrReservationCancelled):
		return http.StatusConflict, "reservation_cancelled"
	case errors.Is(err, store.ErrHoldExpired):
		return http.StatusConflict, "hold_expired"
	case errors.Is(err, store.ErrUnknownSeatLabel):
		return http.StatusBadRequest, "unknown_seat_label"
	case errors.Is(err, store.ErrDuplicateSeatLabel):
		return http.StatusBadRequest, "duplicate_seat_label"
	case errors.Is(err, store.ErrSeatsUnavailable):
		return http.StatusConflict, "seats_unavailable"
	case errors.Is(err, store.ErrPerUserLimit):
		return http.StatusConflict, "per_user_limit_exceeded"
	case errors.Is(err, store.ErrIdempotencyReuse):
		return http.StatusConflict, "idempotency_key_reused"
	default:
		return http.StatusServiceUnavailable, "db_unavailable"
	}
}

func (s *Server) fail(w http.ResponseWriter, r *http.Request, err error) {
	code, reason := statusFor(err)
	metrics.Declined.WithLabelValues(reason).Inc()
	if code >= 500 {
		// Only unexpected failures are worth a log line; a lost seat race is
		// a normal outcome and logging 499 of them per burst is noise.
		s.log.Error("request failed", "path", r.URL.Path, "err", err)
	}
	writeJSON(w, code, map[string]string{"error": reason})
}

func writeJSON(w http.ResponseWriter, code int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(body)
}

func badRequest(w http.ResponseWriter, reason string) {
	writeJSON(w, http.StatusBadRequest, map[string]string{"error": reason})
}
