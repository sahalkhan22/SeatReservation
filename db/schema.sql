-- SeatLock schema.
-- Applied automatically by the postgres container on first boot
-- (mounted into /docker-entrypoint-initdb.d).
--
-- The correctness guarantees of this service live in THIS FILE, not in Go.

CREATE TYPE seat_status AS ENUM ('available', 'held', 'confirmed');

CREATE TABLE shows (
  id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  name        text   NOT NULL,
  price_paise bigint NOT NULL CHECK (price_paise >= 0),
  total_seats int    NOT NULL CHECK (total_seats > 0),
  created_at  timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE reservations (
  id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  show_id      uuid   NOT NULL REFERENCES shows(id),
  user_id      text   NOT NULL,
  status       text   NOT NULL CHECK (status IN ('held', 'confirmed', 'cancelled')),
  amount_paise bigint NOT NULL CHECK (amount_paise >= 0),
  created_at   timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX reservations_user_show_idx ON reservations (user_id, show_id);

CREATE TABLE seats (
  id             bigserial PRIMARY KEY,
  show_id        uuid NOT NULL REFERENCES shows(id),
  label          text NOT NULL,
  status         seat_status NOT NULL DEFAULT 'available',
  reservation_id uuid REFERENCES reservations(id),

  -- When a hold lapses. NULL for available and confirmed seats.
  -- Expiry is lazy: nothing sweeps this table. The next transaction that
  -- wants the seat reclaims it inside the same statement that grabs it, so
  -- there is no background job to race against a confirm.
  held_until     timestamptz,

  UNIQUE (show_id, label),

  -- A seat is owned exactly when it is not available. The database rejects
  -- any other combination, so "confirmed but ownerless" cannot be stored.
  CONSTRAINT seat_ownership_matches_status
    CHECK ((status = 'available') = (reservation_id IS NULL)),

  -- A deadline exists exactly when the seat is held.
  CONSTRAINT seat_deadline_matches_status
    CHECK ((status = 'held') = (held_until IS NOT NULL))
);
CREATE INDEX seats_show_label_idx  ON seats (show_id, label);
CREATE INDEX seats_reservation_idx ON seats (reservation_id)
  WHERE reservation_id IS NOT NULL;

-- Exists ONLY to be locked. There is no counter column on purpose: a stored
-- count drifts (cancels, expiries, crashes), whereas a count derived under
-- this row's lock cannot. Reserve takes FOR UPDATE here before counting, which
-- serialises one user's concurrent requests for one show while leaving
-- different users fully parallel.
CREATE TABLE user_show_quota (
  user_id text NOT NULL,
  show_id uuid NOT NULL REFERENCES shows(id),
  PRIMARY KEY (user_id, show_id)
);

-- Lives in Postgres, not Redis, so the key and the reservation it refers to
-- commit in the SAME transaction. They can never disagree.
CREATE TABLE idempotency_keys (
  user_id        text NOT NULL,
  show_id        uuid NOT NULL REFERENCES shows(id),
  key            text NOT NULL,
  seats_hash     text NOT NULL,   -- sha256 of the sorted seat labels
  reservation_id uuid NOT NULL REFERENCES reservations(id),
  created_at     timestamptz NOT NULL DEFAULT now(),
  -- scoped to the show: the same key on a different show is a different
  -- request, not a replay of the first one
  PRIMARY KEY (user_id, show_id, key)
);
