package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"
)

// These tests run against a real Postgres, not a mock. The behaviour under
// test IS the database's locking behaviour, so mocking it would test nothing.
//
//	DATABASE_URL=postgres://seatlock:devpassword@localhost:5433/seatlock?sslmode=disable go test ./...
func testStore(t *testing.T) *Store {
	t.Helper()
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL not set; skipping integration tests")
	}
	s, err := Open(context.Background(), dsn, 20, 5*time.Second)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(s.Close)
	return s
}

// newShow gives every test its own show, so tests never contend with each
// other and can run with -race in parallel.
func newShow(t *testing.T, s *Store, seats int) Show {
	t.Helper()
	labels := make([]string, seats)
	for i := range labels {
		labels[i] = fmt.Sprintf("A%d", i+1)
	}
	show, err := s.CreateShow(context.Background(), t.Name(), labels, 25000)
	if err != nil {
		t.Fatalf("create show: %v", err)
	}
	return show
}

func reserve(s *Store, show, user string, seats []string, key string) (Reservation, bool, error) {
	return s.Reserve(context.Background(), ReserveParams{
		ShowID: show, UserID: user, Seats: seats,
		IdempotencyKey: key, MaxPerUser: 4,
	})
}

// The headline guarantee: N users, one seat, exactly one winner.
func TestNoDoubleSell(t *testing.T) {
	s := testStore(t)
	show := newShow(t, s, 10)

	const racers = 200
	var wg sync.WaitGroup
	start := make(chan struct{})
	wins := make([]bool, racers)

	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			_, _, err := reserve(s, show.ID, fmt.Sprintf("u%d", i), []string{"A1"}, fmt.Sprintf("k%d", i))
			wins[i] = err == nil
		}(i)
	}
	close(start)
	wg.Wait()

	won := 0
	for _, w := range wins {
		if w {
			won++
		}
	}
	if won != 1 {
		t.Fatalf("expected exactly 1 winner out of %d, got %d", racers, won)
	}

	state, err := s.GetShowState(context.Background(), show.ID)
	if err != nil {
		t.Fatal(err)
	}
	if state.Summary.Confirmed != 1 {
		t.Fatalf("expected 1 confirmed seat, got %d", state.Summary.Confirmed)
	}
	assertInvariant(t, state)
}

// The quota must hold even when one user fires everything at once at seats
// that are NOT contended - the seat race cannot save us here.
func TestPerUserLimitUnderConcurrency(t *testing.T) {
	s := testStore(t)
	show := newShow(t, s, 40)

	var wg sync.WaitGroup
	start := make(chan struct{})
	ok := make([]bool, 40)

	for i := 0; i < 40; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			_, _, err := reserve(s, show.ID, "greedy", []string{fmt.Sprintf("A%d", i+1)}, fmt.Sprintf("k%d", i))
			ok[i] = err == nil
		}(i)
	}
	close(start)
	wg.Wait()

	got := 0
	for _, v := range ok {
		if v {
			got++
		}
	}
	if got != 4 {
		t.Fatalf("per-user limit is 4, but %d requests succeeded", got)
	}
}

// Two requests for the same seats in OPPOSITE order must not deadlock. Without
// ORDER BY id in the locking CTE this test fails intermittently with a
// Postgres deadlock, which would surface as a 5xx.
func TestMultiSeatOppositeOrderDoesNotDeadlock(t *testing.T) {
	s := testStore(t)
	show := newShow(t, s, 10)

	var wg sync.WaitGroup
	errs := make([]error, 2*50)
	start := make(chan struct{})

	for i := 0; i < 50; i++ {
		wg.Add(2)
		go func(i int) {
			defer wg.Done()
			<-start
			_, _, errs[i*2] = reserve(s, show.ID, fmt.Sprintf("fwd%d", i), []string{"A1", "A2"}, fmt.Sprintf("f%d", i))
		}(i)
		go func(i int) {
			defer wg.Done()
			<-start
			_, _, errs[i*2+1] = reserve(s, show.ID, fmt.Sprintf("rev%d", i), []string{"A2", "A1"}, fmt.Sprintf("r%d", i))
		}(i)
	}
	close(start)
	wg.Wait()

	for _, err := range errs {
		// Losing the race is fine. A deadlock is not: it is neither nil nor a
		// domain error, and it is exactly what ORDER BY id exists to prevent.
		if err != nil && !errors.Is(err, ErrSeatsUnavailable) && !errors.Is(err, ErrPerUserLimit) {
			t.Fatalf("unexpected non-domain error (likely a deadlock): %v", err)
		}
	}
}

func TestIdempotentReplayReturnsOriginal(t *testing.T) {
	s := testStore(t)
	show := newShow(t, s, 5)

	first, replayed, err := reserve(s, show.ID, "alice", []string{"A1"}, "same-key")
	if err != nil || replayed {
		t.Fatalf("first reserve: err=%v replayed=%v", err, replayed)
	}

	second, replayed, err := reserve(s, show.ID, "alice", []string{"A1"}, "same-key")
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if !replayed {
		t.Fatal("expected the second call to be flagged as a replay")
	}
	if second.ID != first.ID {
		t.Fatalf("replay returned a different reservation: %s vs %s", second.ID, first.ID)
	}
}

func TestIdempotencyKeyWithDifferentSeatsIsRejected(t *testing.T) {
	s := testStore(t)
	show := newShow(t, s, 5)

	if _, _, err := reserve(s, show.ID, "alice", []string{"A1"}, "k"); err != nil {
		t.Fatal(err)
	}
	_, _, err := reserve(s, show.ID, "alice", []string{"A2"}, "k")
	if !errors.Is(err, ErrIdempotencyReuse) {
		t.Fatalf("expected ErrIdempotencyReuse, got %v", err)
	}
}

// The same key on a DIFFERENT show is a different request, not a replay.
func TestIdempotencyKeyIsScopedToShow(t *testing.T) {
	s := testStore(t)
	showA := newShow(t, s, 5)
	showB := newShow(t, s, 5)

	a, _, err := reserve(s, showA.ID, "alice", []string{"A1"}, "shared")
	if err != nil {
		t.Fatal(err)
	}
	b, replayed, err := reserve(s, showB.ID, "alice", []string{"A1"}, "shared")
	if err != nil {
		t.Fatal(err)
	}
	if replayed {
		t.Fatal("a key reused on another show must not replay the first show's booking")
	}
	if a.ID == b.ID {
		t.Fatal("got the same reservation for two different shows")
	}
}

func TestAllOrNothing(t *testing.T) {
	s := testStore(t)
	show := newShow(t, s, 5)

	if _, _, err := reserve(s, show.ID, "bob", []string{"A2"}, "b1"); err != nil {
		t.Fatal(err)
	}

	// A1 is free, A2 is taken: the whole request must decline.
	_, _, err := reserve(s, show.ID, "alice", []string{"A1", "A2"}, "a1")
	if !errors.Is(err, ErrSeatsUnavailable) {
		t.Fatalf("expected ErrSeatsUnavailable, got %v", err)
	}

	state, err := s.GetShowState(context.Background(), show.ID)
	if err != nil {
		t.Fatal(err)
	}
	if state.Seats["A1"] != "available" {
		t.Fatalf("A1 should have been left untouched, got %q", state.Seats["A1"])
	}
}

func TestCancelReleasesSeatsAndRefundsQuota(t *testing.T) {
	s := testStore(t)
	show := newShow(t, s, 10)
	ctx := context.Background()

	// Fill the user's quota.
	var last Reservation
	for i := 0; i < 4; i++ {
		r, _, err := reserve(s, show.ID, "alice", []string{fmt.Sprintf("A%d", i+1)}, fmt.Sprintf("k%d", i))
		if err != nil {
			t.Fatalf("reserve %d: %v", i, err)
		}
		last = r
	}
	if _, _, err := reserve(s, show.ID, "alice", []string{"A5"}, "k5"); !errors.Is(err, ErrPerUserLimit) {
		t.Fatalf("expected the 5th to hit the limit, got %v", err)
	}

	if _, err := s.Cancel(ctx, last.ID, "alice"); err != nil {
		t.Fatalf("cancel: %v", err)
	}

	// Quota refunded, so a fifth booking now fits.
	if _, _, err := reserve(s, show.ID, "alice", []string{"A5"}, "k5"); err != nil {
		t.Fatalf("after cancel the quota should allow another seat, got %v", err)
	}

	state, err := s.GetShowState(ctx, show.ID)
	if err != nil {
		t.Fatal(err)
	}
	assertInvariant(t, state)
}

func TestCancelCannotTouchAnotherUsersSeats(t *testing.T) {
	s := testStore(t)
	show := newShow(t, s, 5)

	r, _, err := reserve(s, show.ID, "alice", []string{"A1"}, "k")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Cancel(context.Background(), r.ID, "mallory"); !errors.Is(err, ErrReservationNotFound) {
		t.Fatalf("expected ErrReservationNotFound for a non-owner, got %v", err)
	}
}

// A lapsed hold is reclaimed by the next reserver, with no sweeper involved,
// and the original holder can no longer confirm it.
func TestExpiredHoldIsReclaimedLazily(t *testing.T) {
	s := testStore(t)
	show := newShow(t, s, 5)
	ctx := context.Background()

	held, _, err := s.Reserve(ctx, ReserveParams{
		ShowID: show.ID, UserID: "alice", Seats: []string{"A1"},
		IdempotencyKey: "h", MaxPerUser: 4, HoldFor: 300 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	if held.Status != "held" {
		t.Fatalf("expected status held, got %q", held.Status)
	}

	// Still held: nobody else may take it.
	if _, _, err := reserve(s, show.ID, "bob", []string{"A1"}, "b1"); !errors.Is(err, ErrSeatsUnavailable) {
		t.Fatalf("expected the live hold to block bob, got %v", err)
	}

	time.Sleep(500 * time.Millisecond)

	if _, _, err := reserve(s, show.ID, "bob", []string{"A1"}, "b2"); err != nil {
		t.Fatalf("bob should reclaim the lapsed hold, got %v", err)
	}
	if _, err := s.Confirm(ctx, held.ID, "alice"); !errors.Is(err, ErrHoldExpired) {
		t.Fatalf("expected ErrHoldExpired confirming a lapsed hold, got %v", err)
	}
}

func TestConfirmTurnsHoldIntoBooking(t *testing.T) {
	s := testStore(t)
	show := newShow(t, s, 5)
	ctx := context.Background()

	held, _, err := s.Reserve(ctx, ReserveParams{
		ShowID: show.ID, UserID: "alice", Seats: []string{"A1"},
		IdempotencyKey: "h", MaxPerUser: 4, HoldFor: 30 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}

	done, err := s.Confirm(ctx, held.ID, "alice")
	if err != nil {
		t.Fatalf("confirm: %v", err)
	}
	if done.Status != "confirmed" {
		t.Fatalf("expected confirmed, got %q", done.Status)
	}

	// Confirming twice is a no-op, not an error.
	if _, err := s.Confirm(ctx, held.ID, "alice"); err != nil {
		t.Fatalf("second confirm should be idempotent, got %v", err)
	}
}

func TestUnknownSeatLabelIsNotAConflict(t *testing.T) {
	s := testStore(t)
	show := newShow(t, s, 5)

	_, _, err := reserve(s, show.ID, "alice", []string{"Z99"}, "k")
	if !errors.Is(err, ErrUnknownSeatLabel) {
		t.Fatalf("expected ErrUnknownSeatLabel, got %v", err)
	}
}

func assertInvariant(t *testing.T, state ShowState) {
	t.Helper()
	sum := state.Summary.Available + state.Summary.Held + state.Summary.Confirmed
	if sum != state.Summary.Total {
		t.Fatalf("invariant broken: %d available + %d held + %d confirmed = %d, total is %d",
			state.Summary.Available, state.Summary.Held, state.Summary.Confirmed,
			sum, state.Summary.Total)
	}
}
