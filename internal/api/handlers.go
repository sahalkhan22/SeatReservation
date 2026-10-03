package api

import (
	"encoding/json"
	"net/http"
	"time"

	"seatlock/internal/metrics"
	"seatlock/internal/store"
)

const maxSeatsPerShow = 20000

type createShowRequest struct {
	Name       string   `json:"name"`
	Seats      []string `json:"seats"`
	PricePaise int64    `json:"price_paise"`
}

func (s *Server) createShow(w http.ResponseWriter, r *http.Request) {
	var req createShowRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		badRequest(w, "invalid_body")
		return
	}
	switch {
	case req.Name == "":
		badRequest(w, "name_required")
		return
	case len(req.Seats) == 0:
		badRequest(w, "seats_required")
		return
	case len(req.Seats) > maxSeatsPerShow:
		badRequest(w, "too_many_seats")
		return
	case req.PricePaise < 0:
		badRequest(w, "price_must_be_non_negative")
		return
	}
	if dup := firstDuplicate(req.Seats); dup != "" {
		badRequest(w, "duplicate_seat_label")
		return
	}

	show, err := s.db.CreateShow(r.Context(), req.Name, req.Seats, req.PricePaise)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, show)
}

func (s *Server) getShow(w http.ResponseWriter, r *http.Request) {
	state, err := s.db.GetShowState(r.Context(), r.PathValue("id"))
	if err != nil {
		s.fail(w, r, err)
		return
	}

	// Refreshed on read rather than by a background poller: the numbers are
	// exact whenever anyone is actually watching a show, and a show nobody
	// queries is a show nobody is scraping dashboards for either.
	metrics.SeatsGauge.WithLabelValues(state.ShowID, "available").Set(float64(state.Summary.Available))
	metrics.SeatsGauge.WithLabelValues(state.ShowID, "held").Set(float64(state.Summary.Held))
	metrics.SeatsGauge.WithLabelValues(state.ShowID, "confirmed").Set(float64(state.Summary.Confirmed))

	writeJSON(w, http.StatusOK, state)
}

// reserveRequest has no user_id field, on purpose. A client can put one in the
// JSON body and the decoder will silently drop it, because identity is read
// from the verified token in the request context. Spoofing is not something
// this handler validates against - it is structurally impossible.
type reserveRequest struct {
	Seats          []string `json:"seats"`
	IdempotencyKey string   `json:"idempotency_key"`

	// Optional. When > 0 the seats are HELD for this long and must be
	// completed with POST /reservations/{id}/confirm. Omitted or 0 confirms
	// immediately, which is the default booking flow.
	HoldSeconds int `json:"hold_seconds"`
}

func (s *Server) reserve(w http.ResponseWriter, r *http.Request) {
	var req reserveRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		badRequest(w, "invalid_body")
		return
	}
	switch {
	case len(req.Seats) == 0:
		badRequest(w, "seats_required")
		return
	case req.IdempotencyKey == "":
		badRequest(w, "idempotency_key_required")
		return
	case len(req.Seats) > s.cfg.MaxSeatsPerUser:
		// Cheap pre-check. The authoritative limit is still enforced under
		// the quota row lock inside the transaction.
		writeJSON(w, http.StatusConflict,
			map[string]string{"error": "per_user_limit_exceeded"})
		return
	}
	if dup := firstDuplicate(req.Seats); dup != "" {
		badRequest(w, "duplicate_seat_label")
		return
	}
	if req.HoldSeconds < 0 || req.HoldSeconds > 3600 {
		badRequest(w, "hold_seconds_out_of_range")
		return
	}

	res, replayed, err := s.db.Reserve(r.Context(), store.ReserveParams{
		ShowID:         r.PathValue("id"),
		UserID:         userID(r), // from the token, never the body
		Seats:          req.Seats,
		IdempotencyKey: req.IdempotencyKey,
		MaxPerUser:     s.cfg.MaxSeatsPerUser,
		Unsafe:         s.cfg.UnsafeMode,
		HoldFor:        time.Duration(req.HoldSeconds) * time.Second,
	})
	if err != nil {
		s.fail(w, r, err)
		return
	}

	// 201 for a booking that happened here, 200 when an earlier identical
	// request already made it. Both carry the same body.
	if !replayed {
		metrics.Confirmed.Inc()
	}

	code := http.StatusCreated
	if replayed {
		code = http.StatusOK
	}
	writeJSON(w, code, res)
}

func firstDuplicate(items []string) string {
	seen := make(map[string]struct{}, len(items))
	for _, it := range items {
		if _, ok := seen[it]; ok {
			return it
		}
		seen[it] = struct{}{}
	}
	return ""
}
