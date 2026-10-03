package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

type Show struct {
	ID         string `json:"show_id"`
	Name       string `json:"name"`
	PricePaise int64  `json:"price_paise"`
	TotalSeats int    `json:"total_seats"`
}

type Summary struct {
	Available int `json:"available"`
	Held      int `json:"held"`
	Confirmed int `json:"confirmed"`
	Total     int `json:"total"`
}

type ShowState struct {
	ShowID     string            `json:"show_id"`
	Name       string            `json:"name"`
	PricePaise int64             `json:"price_paise"`
	Seats      map[string]string `json:"seats"`
	Summary    Summary           `json:"summary"`
}

// CreateShow inserts the show and all of its seats in one transaction, so a
// show can never exist with a partial seat map.
func (s *Store) CreateShow(ctx context.Context, name string, seats []string, pricePaise int64) (Show, error) {
	ctx, cancel := context.WithTimeout(ctx, s.queryTimeout)
	defer cancel()

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Show{}, err
	}
	defer tx.Rollback(context.Background())

	var show Show
	show.Name, show.PricePaise, show.TotalSeats = name, pricePaise, len(seats)

	err = tx.QueryRow(ctx,
		`INSERT INTO shows (name, price_paise, total_seats)
		 VALUES ($1, $2, $3) RETURNING id::text`,
		name, pricePaise, len(seats),
	).Scan(&show.ID)
	if err != nil {
		return Show{}, err
	}

	// One round trip for every seat, however many there are.
	_, err = tx.Exec(ctx,
		`INSERT INTO seats (show_id, label)
		 SELECT $1::uuid, label FROM unnest($2::text[]) AS label`,
		show.ID, seats,
	)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return Show{}, ErrDuplicateSeatLabel
		}
		return Show{}, err
	}

	if err := tx.Commit(ctx); err != nil {
		return Show{}, err
	}
	return show, nil
}

// GetShowState reads the whole show in a SINGLE statement. Two statements
// would let the seat list and the counts come from different snapshots, and
// the invariant would appear violated when nothing was actually wrong.
func (s *Store) GetShowState(ctx context.Context, showID string) (ShowState, error) {
	ctx, cancel := context.WithTimeout(ctx, s.queryTimeout)
	defer cancel()

	rows, err := s.pool.Query(ctx,
		`SELECT sh.name, sh.price_paise, se.label, CASE WHEN se.status = 'held' AND se.held_until < now()
		              THEN 'available' ELSE se.status::text END
		 FROM shows sh
		 JOIN seats se ON se.show_id = sh.id
		 WHERE sh.id = $1::uuid
		 ORDER BY se.id`,
		showID,
	)
	if err != nil {
		return ShowState{}, err
	}
	defer rows.Close()

	st := ShowState{ShowID: showID, Seats: map[string]string{}}
	for rows.Next() {
		var label, status string
		if err := rows.Scan(&st.Name, &st.PricePaise, &label, &status); err != nil {
			return ShowState{}, err
		}
		st.Seats[label] = status
		switch status {
		case "available":
			st.Summary.Available++
		case "held":
			st.Summary.Held++
		case "confirmed":
			st.Summary.Confirmed++
		}
		st.Summary.Total++
	}
	if err := rows.Err(); err != nil {
		return ShowState{}, err
	}
	if st.Summary.Total == 0 {
		return ShowState{}, ErrShowNotFound
	}
	return st, nil
}

func loadReservation(ctx context.Context, tx pgx.Tx, resID string) (Reservation, error) {
	var r Reservation
	err := tx.QueryRow(ctx,
		`SELECT r.id::text, r.show_id::text, r.user_id, r.status, r.amount_paise,
		        (SELECT max(held_until) FROM seats WHERE reservation_id = r.id)
		 FROM reservations r WHERE r.id = $1::uuid`, resID,
	).Scan(&r.ID, &r.ShowID, &r.UserID, &r.Status, &r.AmountPaise, &r.ExpiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return r, ErrReservationNotFound
	}
	if err != nil {
		return r, fmt.Errorf("load reservation: %w", err)
	}

	rows, err := tx.Query(ctx,
		`SELECT label FROM seats WHERE reservation_id = $1::uuid ORDER BY id`, resID)
	if err != nil {
		return r, err
	}
	defer rows.Close()
	for rows.Next() {
		var label string
		if err := rows.Scan(&label); err != nil {
			return r, err
		}
		r.Seats = append(r.Seats, label)
	}
	return r, rows.Err()
}
