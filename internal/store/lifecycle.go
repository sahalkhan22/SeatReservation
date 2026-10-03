package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// Cancel releases a reservation's seats back to the pool.
//
// Lock order is identical to Reserve - quota row first, then seats - so the
// two can never deadlock against each other.
func (s *Store) Cancel(ctx context.Context, reservationID, userID string) (Reservation, error) {
	ctx, cancel := context.WithTimeout(ctx, s.queryTimeout)
	defer cancel()

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Reservation{}, err
	}
	defer tx.Rollback(context.Background())

	// Unlocked read, only to find which quota row to lock first.
	var showID, owner string
	err = tx.QueryRow(ctx,
		`SELECT show_id::text, user_id FROM reservations WHERE id = $1::uuid`,
		reservationID).Scan(&showID, &owner)
	if errors.Is(err, pgx.ErrNoRows) {
		return Reservation{}, ErrReservationNotFound
	}
	if err != nil {
		return Reservation{}, err
	}
	// 404 rather than 403: someone else's reservation id should not be
	// confirmed to exist by the error we return.
	if owner != userID {
		return Reservation{}, ErrReservationNotFound
	}

	if err := lockQuota(ctx, tx, userID, showID); err != nil {
		return Reservation{}, err
	}

	var status string
	err = tx.QueryRow(ctx,
		`SELECT status FROM reservations WHERE id = $1::uuid FOR UPDATE`,
		reservationID).Scan(&status)
	if err != nil {
		return Reservation{}, err
	}
	if status == "cancelled" {
		// Idempotent: cancelling twice is not an error.
		res, lerr := loadReservation(ctx, tx, reservationID)
		if lerr != nil {
			return Reservation{}, lerr
		}
		return res, tx.Commit(ctx)
	}

	// reservation_id = $1 is the safety property: this statement is incapable
	// of touching a seat that belongs to anybody else, so a cancel can never
	// release a seat someone has already confirmed.
	rows, err := tx.Query(ctx,
		`UPDATE seats
		    SET status = 'available', reservation_id = NULL, held_until = NULL
		  WHERE reservation_id = $1::uuid
		 RETURNING label`,
		reservationID)
	if err != nil {
		return Reservation{}, err
	}
	released, err := collectLabels(rows)
	rows.Close()
	if err != nil {
		return Reservation{}, err
	}

	if _, err := tx.Exec(ctx,
		`UPDATE reservations SET status = 'cancelled' WHERE id = $1::uuid`,
		reservationID); err != nil {
		return Reservation{}, err
	}

	res, err := loadReservation(ctx, tx, reservationID)
	if err != nil {
		return Reservation{}, err
	}
	res.Seats = released
	return res, tx.Commit(ctx)
}

// Confirm turns a hold into a confirmed booking, provided it has not lapsed.
//
// There is no sweeper to race: expiry is evaluated here, under the seat row
// locks, against the same clock the reclaim path uses.
func (s *Store) Confirm(ctx context.Context, reservationID, userID string) (Reservation, error) {
	ctx, cancel := context.WithTimeout(ctx, s.queryTimeout)
	defer cancel()

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Reservation{}, err
	}
	defer tx.Rollback(context.Background())

	var showID, owner, status string
	err = tx.QueryRow(ctx,
		`SELECT show_id::text, user_id, status FROM reservations WHERE id = $1::uuid`,
		reservationID).Scan(&showID, &owner, &status)
	if errors.Is(err, pgx.ErrNoRows) {
		return Reservation{}, ErrReservationNotFound
	}
	if err != nil {
		return Reservation{}, err
	}
	if owner != userID {
		return Reservation{}, ErrReservationNotFound
	}

	if err := lockQuota(ctx, tx, userID, showID); err != nil {
		return Reservation{}, err
	}

	switch status {
	case "confirmed":
		res, lerr := loadReservation(ctx, tx, reservationID) // idempotent
		if lerr != nil {
			return Reservation{}, lerr
		}
		return res, tx.Commit(ctx)
	case "cancelled":
		return Reservation{}, ErrReservationCancelled
	}

	// Only seats still held by THIS reservation and not yet lapsed convert.
	rows, err := tx.Query(ctx,
		`UPDATE seats
		    SET status = 'confirmed', held_until = NULL
		  WHERE reservation_id = $1::uuid
		    AND status = 'held'
		    AND held_until >= now()
		 RETURNING label`,
		reservationID)
	if err != nil {
		return Reservation{}, err
	}
	confirmed, err := collectLabels(rows)
	rows.Close()
	if err != nil {
		return Reservation{}, err
	}

	var expected int
	if err := tx.QueryRow(ctx,
		`SELECT count(*) FROM seats WHERE reservation_id = $1::uuid`,
		reservationID).Scan(&expected); err != nil {
		return Reservation{}, err
	}
	if len(confirmed) != expected || expected == 0 {
		// The hold lapsed, wholly or partly. Decline rather than confirm a
		// subset, so the client never half-owns a booking.
		return Reservation{}, ErrHoldExpired
	}

	if _, err := tx.Exec(ctx,
		`UPDATE reservations SET status = 'confirmed' WHERE id = $1::uuid`,
		reservationID); err != nil {
		return Reservation{}, err
	}

	res, err := loadReservation(ctx, tx, reservationID)
	if err != nil {
		return Reservation{}, err
	}
	return res, tx.Commit(ctx)
}

// Now is exposed so handlers can report hold deadlines using the same clock.
func Now() time.Time { return time.Now() }
