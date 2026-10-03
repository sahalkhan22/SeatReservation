package store

import "errors"

// Domain outcomes. Every one of these maps to a 4xx in the API layer - none of
// them is a server fault. Anything NOT in this list is a real 5xx.
var (
	ErrShowNotFound         = errors.New("show_not_found")
	ErrReservationNotFound  = errors.New("reservation_not_found")
	ErrReservationCancelled = errors.New("reservation_cancelled")
	ErrHoldExpired          = errors.New("hold_expired")
	ErrUnknownSeatLabel     = errors.New("unknown_seat_label")
	ErrSeatsUnavailable     = errors.New("seats_unavailable")
	ErrPerUserLimit         = errors.New("per_user_limit_exceeded")
	ErrIdempotencyReuse     = errors.New("idempotency_key_reused")
	ErrDuplicateSeatLabel   = errors.New("duplicate_seat_label")
)
