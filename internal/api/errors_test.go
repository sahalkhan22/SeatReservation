package api

import (
	"errors"
	"fmt"
	"net/http"
	"testing"

	"seatlock/internal/store"
)

// Every domain error must map to a 4xx. A 5xx here means a normal outcome -
// losing a seat race, hitting a limit - would be reported as a server fault,
// which is exactly what the load test forbids.
func TestEveryDomainErrorMapsTo4xx(t *testing.T) {
	domain := []error{
		store.ErrShowNotFound,
		store.ErrReservationNotFound,
		store.ErrReservationCancelled,
		store.ErrHoldExpired,
		store.ErrUnknownSeatLabel,
		store.ErrDuplicateSeatLabel,
		store.ErrSeatsUnavailable,
		store.ErrPerUserLimit,
		store.ErrIdempotencyReuse,
	}
	for _, err := range domain {
		code, reason := statusFor(err)
		if code < 400 || code >= 500 {
			t.Errorf("%v mapped to %d, expected a 4xx", err, code)
		}
		if reason == "" {
			t.Errorf("%v mapped to an empty reason string", err)
		}
	}
}

// An unrecognised error must become 503, never 500: the realistic cause is the
// database being unreachable, and 503 is the honest answer.
func TestUnknownErrorIsUnavailableNotInternal(t *testing.T) {
	code, reason := statusFor(errors.New("something nobody anticipated"))
	if code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", code)
	}
	if reason != "db_unavailable" {
		t.Fatalf("expected db_unavailable, got %q", reason)
	}
}

// errors.Is must survive wrapping, or the mapping silently degrades to 503 as
// soon as any layer adds context to an error.
func TestMappingSurvivesWrapping(t *testing.T) {
	wrapped := fmt.Errorf("grabbing seats: %w", store.ErrSeatsUnavailable)
	code, reason := statusFor(wrapped)
	if code != http.StatusConflict || reason != "seats_unavailable" {
		t.Fatalf("wrapped error mapped to %d %q", code, reason)
	}
}

func TestFirstDuplicate(t *testing.T) {
	if got := firstDuplicate([]string{"A1", "A2", "A1"}); got != "A1" {
		t.Fatalf("expected A1, got %q", got)
	}
	if got := firstDuplicate([]string{"A1", "A2"}); got != "" {
		t.Fatalf("expected no duplicate, got %q", got)
	}
}
