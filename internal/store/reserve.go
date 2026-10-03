package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

type Reservation struct {
	ID          string     `json:"reservation_id"`
	ShowID      string     `json:"show_id"`
	UserID      string     `json:"user_id"`
	Seats       []string   `json:"seats"`
	AmountPaise int64      `json:"amount_paise"`
	Status      string     `json:"status"`
	ExpiresAt   *time.Time `json:"expires_at,omitempty"`
}

type ReserveParams struct {
	ShowID         string
	UserID         string
	Seats          []string
	IdempotencyKey string
	MaxPerUser     int
	Unsafe         bool

	// HoldFor > 0 books the seats as a hold that lapses after this long, to be
	// completed with Confirm. Zero means confirm immediately.
	HoldFor time.Duration
}

// The statement the whole service rests on.
//
// The availability predicate is evaluated WHILE the row lock is held, so N
// concurrent transactions queue here, the first commits and flips the status,
// and the rest re-evaluate and match zero rows.
//
// ORDER BY id gives every transaction the same lock order, so two requests for
// [A12,A13] and [A13,A12] cannot deadlock against each other.
//
// The held_until clause is lazy expiry: a lapsed hold is reclaimed by the next
// person who wants the seat, inside this same statement. No sweeper job exists,
// so there is nothing for a concurrent Confirm to race against.
const sqlGrabSeats = `
WITH locked AS (
    SELECT id FROM seats
    WHERE show_id = $1::uuid
      AND label = ANY($2::text[])
      AND (status = 'available' OR (status = 'held' AND held_until < now()))
    ORDER BY id
    FOR UPDATE
)
UPDATE seats
   SET status = $4::seat_status, reservation_id = $3::uuid, held_until = $5
 WHERE id IN (SELECT id FROM locked)
RETURNING label`

// Counted under the quota row lock. Derived, never stored, so it cannot drift
// when holds lapse or reservations are cancelled.
const sqlCountUserSeats = `
SELECT count(*)
  FROM seats s
  JOIN reservations r ON r.id = s.reservation_id
 WHERE s.show_id = $2::uuid
   AND r.user_id = $1
   AND (s.status = 'confirmed' OR (s.status = 'held' AND s.held_until >= now()))`

// errKeyRaced means another transaction committed this exact idempotency key
// while we were working. Not a client error - the client simply needs to be
// handed whatever that other transaction produced.
var errKeyRaced = errors.New("idempotency key committed concurrently")

// Reserve grabs every requested seat or none of them.
//
// Returns (reservation, replayed, error). replayed is true when an existing
// idempotency key produced this result rather than a fresh booking.
func (s *Store) Reserve(ctx context.Context, p ReserveParams) (Reservation, bool, error) {
	res, replayed, err := s.reserveOnce(ctx, p)
	if !errors.Is(err, errKeyRaced) {
		return res, replayed, err
	}

	// Two requests carried the same key at the same moment - a client retry,
	// typically after a dropped connection. The other one won and is now
	// committed, so this one replays its result instead of declining.
	return s.replayKey(ctx, p)
}

// replayKey reads a committed idempotency key and returns the reservation it
// points at. A hash mismatch here is the genuine 409: same key, other seats.
func (s *Store) replayKey(ctx context.Context, p ReserveParams) (Reservation, bool, error) {
	ctx, cancel := context.WithTimeout(ctx, s.queryTimeout)
	defer cancel()

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Reservation{}, false, err
	}
	defer tx.Rollback(context.Background())

	var seenHash, seenResID string
	err = tx.QueryRow(ctx,
		`SELECT seats_hash, reservation_id::text FROM idempotency_keys
		 WHERE user_id = $1 AND show_id = $2::uuid AND key = $3`,
		p.UserID, p.ShowID, p.IdempotencyKey,
	).Scan(&seenHash, &seenResID)
	if errors.Is(err, pgx.ErrNoRows) {
		// The winner rolled back after all; nothing to replay.
		return Reservation{}, false, ErrSeatsUnavailable
	}
	if err != nil {
		return Reservation{}, false, err
	}
	if seenHash != fingerprint(p.Seats) {
		return Reservation{}, false, ErrIdempotencyReuse
	}

	res, err := loadReservation(ctx, tx, seenResID)
	if err != nil {
		return Reservation{}, false, err
	}
	return res, true, tx.Commit(ctx)
}

func (s *Store) reserveOnce(ctx context.Context, p ReserveParams) (Reservation, bool, error) {
	ctx, cancel := context.WithTimeout(ctx, s.queryTimeout)
	defer cancel()

	hash := fingerprint(p.Seats)

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Reservation{}, false, err
	}
	// Background, not ctx: if ctx is already cancelled the rollback itself
	// would fail and the connection would go back to the pool dirty.
	defer tx.Rollback(context.Background())

	// 1. Idempotency replay. Cheap read, taken before any lock.
	var seenHash, seenResID string
	err = tx.QueryRow(ctx,
		`SELECT seats_hash, reservation_id::text FROM idempotency_keys
		 WHERE user_id = $1 AND show_id = $2::uuid AND key = $3`,
		p.UserID, p.ShowID, p.IdempotencyKey,
	).Scan(&seenHash, &seenResID)
	switch {
	case err == nil:
		if seenHash != hash {
			return Reservation{}, false, ErrIdempotencyReuse
		}
		res, lerr := loadReservation(ctx, tx, seenResID)
		if lerr != nil {
			return Reservation{}, false, lerr
		}
		return res, true, tx.Commit(ctx)
	case !errors.Is(err, pgx.ErrNoRows):
		return Reservation{}, false, err
	}

	// 2. Show must exist, and its price sets the amount.
	var pricePaise int64
	err = tx.QueryRow(ctx,
		`SELECT price_paise FROM shows WHERE id = $1::uuid`, p.ShowID).Scan(&pricePaise)
	if errors.Is(err, pgx.ErrNoRows) {
		return Reservation{}, false, ErrShowNotFound
	}
	if err != nil {
		return Reservation{}, false, err
	}

	// 3. Every requested label must belong to this show. Without this check an
	//    invented seat name returns 409 unavailable instead of 400.
	var known int
	err = tx.QueryRow(ctx,
		`SELECT count(*) FROM seats WHERE show_id = $1::uuid AND label = ANY($2::text[])`,
		p.ShowID, p.Seats).Scan(&known)
	if err != nil {
		return Reservation{}, false, err
	}
	if known != len(p.Seats) {
		return Reservation{}, false, ErrUnknownSeatLabel
	}

	// 4. LOCK 1 - the per-(user,show) quota row, taken BEFORE the count is
	//    read. Without this lock four concurrent requests from one user each
	//    read the same stale count and all four pass the limit check.
	//    Different users touch different rows, so they stay fully parallel.
	if err := lockQuota(ctx, tx, p.UserID, p.ShowID); err != nil {
		return Reservation{}, false, err
	}
	var taken int
	if err := tx.QueryRow(ctx, sqlCountUserSeats, p.UserID, p.ShowID).Scan(&taken); err != nil {
		return Reservation{}, false, err
	}
	if taken+len(p.Seats) > p.MaxPerUser {
		return Reservation{}, false, ErrPerUserLimit
	}

	// 5. Create the reservation first so the seats have something to point at.
	//    If step 6 declines, the rollback erases this row.
	status := "confirmed"
	var expiry *time.Time
	if p.HoldFor > 0 {
		status = "held"
		t := time.Now().Add(p.HoldFor)
		expiry = &t
	}

	res := Reservation{
		ShowID:      p.ShowID,
		UserID:      p.UserID,
		AmountPaise: pricePaise * int64(len(p.Seats)),
		Status:      status,
		ExpiresAt:   expiry,
	}
	err = tx.QueryRow(ctx,
		`INSERT INTO reservations (show_id, user_id, status, amount_paise)
		 VALUES ($1::uuid, $2, $3, $4) RETURNING id::text`,
		p.ShowID, p.UserID, status, res.AmountPaise).Scan(&res.ID)
	if err != nil {
		return Reservation{}, false, err
	}

	// 6. LOCK 2 - the seats themselves, in ascending id order.
	got, err := grabSeats(ctx, tx, p, res.ID, status, expiry)
	if err != nil {
		return Reservation{}, false, err
	}
	if len(got) != len(p.Seats) {
		// All-or-nothing: one unavailable seat declines the whole request.
		return Reservation{}, false, ErrSeatsUnavailable
	}
	sort.Strings(got)
	res.Seats = got

	// 7. The key commits with the booking, so they can never disagree.
	//    A unique violation here means two requests raced with the same key.
	_, err = tx.Exec(ctx,
		`INSERT INTO idempotency_keys (user_id, show_id, key, seats_hash, reservation_id)
		 VALUES ($1, $2::uuid, $3, $4, $5::uuid)`,
		p.UserID, p.ShowID, p.IdempotencyKey, hash, res.ID)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			// Someone else committed this key first. Our seat grab rolls back,
			// and the caller replays theirs - see Reserve.
			return Reservation{}, false, errKeyRaced
		}
		return Reservation{}, false, err
	}

	if err := tx.Commit(ctx); err != nil {
		return Reservation{}, false, err
	}
	return res, false, nil
}

// lockQuota takes the per-(user, show) mutex row, creating it if this is the
// user's first request for the show.
func lockQuota(ctx context.Context, tx pgx.Tx, userID, showID string) error {
	if _, err := tx.Exec(ctx,
		`INSERT INTO user_show_quota (user_id, show_id) VALUES ($1, $2::uuid)
		 ON CONFLICT DO NOTHING`, userID, showID); err != nil {
		return err
	}
	var ignored string
	return tx.QueryRow(ctx,
		`SELECT user_id FROM user_show_quota
		 WHERE user_id = $1 AND show_id = $2::uuid FOR UPDATE`,
		userID, showID).Scan(&ignored)
}

func grabSeats(ctx context.Context, tx pgx.Tx, p ReserveParams, resID, status string, expiry *time.Time) ([]string, error) {
	if p.Unsafe {
		return grabSeatsUnsafe(ctx, tx, p, resID, status, expiry)
	}
	rows, err := tx.Query(ctx, sqlGrabSeats, p.ShowID, p.Seats, resID, status, expiry)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return collectLabels(rows)
}

// grabSeatsUnsafe is the version a developer writes before thinking about
// concurrency: check availability, then write, with no lock spanning the two.
// Enabled by UNSAFE_MODE=1 purely to demonstrate the double-sell that the real
// implementation prevents. Unreachable in a default build.
func grabSeatsUnsafe(ctx context.Context, tx pgx.Tx, p ReserveParams, resID, status string, expiry *time.Time) ([]string, error) {
	rows, err := tx.Query(ctx,
		`SELECT label FROM seats
		 WHERE show_id = $1::uuid AND label = ANY($2::text[])
		   AND (status = 'available' OR (status = 'held' AND held_until < now()))`,
		p.ShowID, p.Seats)
	if err != nil {
		return nil, err
	}
	free, err := collectLabels(rows)
	rows.Close()
	if err != nil {
		return nil, err
	}
	if len(free) != len(p.Seats) {
		return free, nil
	}

	// The gap between the check above and the write below is the whole bug.
	// In production that gap is whatever the process happens to be doing - a
	// network hop, GC, a scheduler preemption - so its width is luck. Holding
	// it open for a fixed few milliseconds makes the resulting double-sell
	// reproducible instead of intermittent. It creates no bug that the gap
	// does not already create; it only stops the demo depending on timing.
	time.Sleep(5 * time.Millisecond)

	// No lock was held across that gap, so another transaction may have taken
	// these seats in the meantime. This overwrites them regardless.
	wrote, err := tx.Query(ctx,
		`UPDATE seats
		    SET status = $4::seat_status, reservation_id = $3::uuid, held_until = $5
		  WHERE show_id = $1::uuid AND label = ANY($2::text[])
		 RETURNING label`,
		p.ShowID, p.Seats, resID, status, expiry)
	if err != nil {
		return nil, err
	}
	defer wrote.Close()
	return collectLabels(wrote)
}

func collectLabels(rows pgx.Rows) ([]string, error) {
	var out []string
	for rows.Next() {
		var label string
		if err := rows.Scan(&label); err != nil {
			return nil, err
		}
		out = append(out, label)
	}
	return out, rows.Err()
}

// fingerprint binds an idempotency key to one exact seat set, so replaying a
// key with different seats is detectable.
func fingerprint(seats []string) string {
	s := append([]string(nil), seats...)
	sort.Strings(s)
	sum := sha256.Sum256([]byte(strings.Join(s, ",")))
	return hex.EncodeToString(sum[:])
}
