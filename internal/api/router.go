package api

import (
	"net/http"
	"time"

	"seatlock/internal/metrics"
)

// Routes uses the Go 1.22 method-and-pattern mux, so no third-party router is
// needed: "POST /shows/{id}/reserve" is matched by the standard library.
func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()

	// Liveness never touches the database. If it did, a brief Postgres blip
	// would make an orchestrator kill a perfectly healthy process.
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})

	// Readiness fails closed, so traffic is shed while the process survives.
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		if err := s.db.Ping(r.Context()); err != nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{
				"status": "unready", "reason": "db_unavailable",
			})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
	})

	mux.Handle("GET /metrics", metrics.Handler())

	s.route(mux, "POST /shows", s.requireAdmin(s.createShow))
	s.route(mux, "GET /shows/{id}", s.getShow)
	s.route(mux, "POST /shows/{id}/reserve", s.authenticate(s.reserve))
	s.route(mux, "POST /reservations/{id}/cancel", s.authenticate(s.cancel))
	s.route(mux, "POST /reservations/{id}/confirm", s.authenticate(s.confirm))

	// Not registered at all when the flag is off, so there is no dev-only
	// route sitting in production waiting to be found.
	if s.cfg.EnableDevToken {
		mux.HandleFunc("POST /dev/token", s.devToken)
	}

	return mux
}

// route registers a handler wrapped in latency instrumentation, labelled by
// the PATTERN rather than the path so a million show ids stay one time series.
func (s *Server) route(mux *http.ServeMux, pattern string, h http.HandlerFunc) {
	mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
		rec := &statusRecorder{ResponseWriter: w, code: http.StatusOK}
		t0 := time.Now()
		h(rec, r)
		metrics.Observe(pattern, rec.code, time.Since(t0))
		metrics.PoolInUse.Set(float64(s.db.InUse()))
	})
}

type statusRecorder struct {
	http.ResponseWriter
	code int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.code = code
	r.ResponseWriter.WriteHeader(code)
}
