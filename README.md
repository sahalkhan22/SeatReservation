# SeatLock

A seat-reservation service built for the one moment that matters: thousands of
people hitting the same seat in the same second.

**No seat is ever sold twice. No failure under load is a 5xx.** Both claims are
checked by a script you can run in one command.

---

## Quick start

Needs only **Docker** and **git**. No Go, no Postgres, no psql on the host.

```bash
git clone <this-repo> && cd seatlock
cp .env.example .env
docker compose up --build
```

Then, in another terminal:

```bash
./scripts/verify.sh
```

That exercises every requirement and prints a pass/fail checklist, ending with
a 500-way stampede and a reconciliation of the seat math.

```
  PASS  reserve a free seat
  PASS  a taken seat is declined, not errored
  PASS  body-supplied user_id is ignored
  PASS  replaying a key returns the original
  PASS  same key with different seats is rejected
  PASS  a partly-available request declines wholly
  PASS  the per-user limit is enforced
  PASS  a lapsed hold frees its seat with no sweeper
  ...
  ---- outcomes ----
    201 confirmed                        1
    409 seats_unavailable              499
    5xx / transport failures             0
  ---- reconciliation ----
    seat math     available 49 + held 0 + confirmed 1 = 50 / total 50   OK
    double-sell   1 requests told 201, 1 seats actually confirmed        OK

  36 passed, 0 failed
```

> **If you change `db/schema.sql`,** run `docker compose down -v` before
> bringing it back up. Postgres only applies the schema when its data volume is
> empty, so without the `-v` your change is silently ignored.

---

## See the bug it prevents

The same load against a deliberately naive implementation:

```bash
sed -i 's/^UNSAFE_MODE=.*/UNSAFE_MODE=1/' .env
docker compose up -d --force-recreate api
./burst.sh $(./scripts/seed.sh 50) A12 500
```

```
  201 confirmed                       15      <-- fifteen people bought seat A12
  seat math     ... = 50 / total 50    OK     <-- the invariant notices nothing
  double-sell   15 told 201, 1 seat    FAIL
```

`UNSAFE_MODE=1` swaps one SQL statement for the read-then-write a developer
writes before thinking about concurrency. Everything else is identical.

Note that the seat-math invariant **passes** during the double-sell. That is
why there is a second check. More on this in [WRITEUP.md](WRITEUP.md).

Set `UNSAFE_MODE=0` and recreate to go back.

---

## API

| Method | Path | Auth | Purpose |
|---|---|---|---|
| `POST` | `/shows` | admin | create a show and its seats |
| `GET` | `/shows/{id}` | — | per-seat status and summary |
| `POST` | `/shows/{id}/reserve` | bearer | book or hold seats |
| `POST` | `/reservations/{id}/confirm` | bearer | turn a hold into a booking |
| `POST` | `/reservations/{id}/cancel` | bearer | release seats |
| `GET` | `/healthz` | — | liveness; never touches the DB |
| `GET` | `/readyz` | — | readiness; 503 when the DB is unreachable |
| `GET` | `/metrics` | — | Prometheus |
| `POST` | `/dev/token` | — | mint a JWT (dev only, off by default) |

### Reserve

```http
POST /shows/{id}/reserve
Authorization: Bearer <jwt>

{ "seats": ["A12"], "idempotency_key": "abc-123", "hold_seconds": 0 }
```

```json
{
  "reservation_id": "84c3c9a5-...",
  "show_id": "eb7b81fa-...",
  "user_id": "alice",
  "seats": ["A12"],
  "amount_paise": 25000,
  "status": "confirmed"
}
```

`user_id` comes from the token's `sub` claim. The request struct has no field
for it, so a `user_id` in the body is dropped by the JSON decoder before any
code runs — spoofing is not validated against, it is unrepresentable.

`hold_seconds > 0` books the seats as a **hold** that lapses on its own and
must be completed with `/confirm`. Omit it for the normal immediate-booking
flow.

### Status codes

| Code | Reason | When |
|---|---|---|
| 201 | — | booked |
| 200 | — | idempotent replay, confirm, cancel, reads |
| 400 | `invalid_body`, `unknown_seat_label`, `duplicate_seat_label`, `hold_seconds_out_of_range` | bad input |
| 401 | `missing_token`, `invalid_token`, `expired_token` | auth |
| 403 | `admin_required` | non-admin creating a show |
| 404 | `show_not_found`, `reservation_not_found` | includes other users' reservations |
| 409 | `seats_unavailable` | lost the race |
| 409 | `per_user_limit_exceeded` | over the limit for this show |
| 409 | `idempotency_key_reused` | same key, different seats |
| 409 | `hold_expired`, `reservation_cancelled` | lifecycle |
| 503 | `db_unavailable` | database unreachable |
| **500** | — | **never emitted by design** |

---

## Configuration

Everything is read from the environment; `.env.example` has working defaults.

| Variable | Default | Notes |
|---|---|---|
| `DATABASE_URL` | — | required; the service refuses to start without it |
| `JWT_SECRET` | — | required |
| `PORT` | `8080` | |
| `MAX_SEATS_PER_USER` | `4` | per user, per show |
| `DB_POOL_MAX` | `20` | the real throughput knob — see WRITEUP |
| `QUERY_TIMEOUT` | `2s` | context deadline; turns overload into 503, never a hang |
| `ENABLE_DEV_TOKEN` | `0` | exposes `POST /dev/token` |
| `UNSAFE_MODE` | `0` | `1` disables seat locking, for the demo above |

---

## Development

```bash
# fast loop: Postgres in a container, Go on the host
docker compose up postgres -d
export DATABASE_URL="postgres://seatlock:devpassword@localhost:5433/seatlock?sslmode=disable"
export JWT_SECRET=dev-secret-change-me
go run ./cmd/api
```

Note the port: **5433 from the host, 5432 inside the compose network.** The
host port is deliberately non-standard so it will not collide with a Postgres
you may already be running.

### Tests

```bash
export DATABASE_URL="postgres://seatlock:devpassword@localhost:5433/seatlock?sslmode=disable"
go test ./...
```

The store tests run against a **real Postgres**, not a mock — the behaviour
under test *is* the database's locking behaviour, so a mock would verify
nothing. They skip automatically when `DATABASE_URL` is unset.

### Load testing

```bash
./burst.sh <show-id> A12 500            # 500 users, one seat
./burst.sh --quota <show-id> x 40       # one user, 40 seats, tests the limit
./burst.sh --url https://your.app <show-id> A12 500
```

Without `--url` the generator runs inside the Docker network. That is not
cosmetic: on Windows the host port proxy starts refusing connections somewhere
past a few hundred simultaneous dials, which looks exactly like a service
failure but never reaches the service.

---

## Layout

```
cmd/api/          server wiring, graceful shutdown
cmd/burst/        load generator + reconciliation
internal/api/     HTTP: decoding, auth, error -> status mapping
internal/store/   every SQL statement in the service
internal/config/  environment loading
internal/metrics/ Prometheus collectors
db/schema.sql     the tables, and the constraints that do the real work
scripts/          seed.sh, verify.sh
```

`internal/api` knows nothing about SQL. `internal/store` knows nothing about
HTTP. Three third-party dependencies total: pgx, golang-jwt, client_golang.

---

## How it works, in one paragraph

Every booking is one Postgres transaction. It takes two locks in a fixed order:
first a per-`(user, show)` row that serialises one user's concurrent requests,
then the seat rows themselves in ascending id order. The seat grab is a single
`UPDATE ... WHERE status = 'available'` whose predicate is evaluated *while the
row lock is held*, so of N racing transactions exactly one matches and the rest
match zero rows and are declined. Nothing is cached, nothing is coordinated
between processes, and there is no distributed lock — which is why you can run
as many API replicas as you like.

The reasoning, the trade-offs, and the bugs found along the way are in
[WRITEUP.md](WRITEUP.md).

---

## Deploying

Fly.io, because `min_machines_running = 1` keeps a machine warm — "surviving
cold starts" is a requirement, and a free tier that scales to zero fails it by
construction.

```bash
# one-time
fly auth login
fly apps create seatlock
fly postgres create --name seatlock-db --region bom
fly postgres attach seatlock-db          # sets DATABASE_URL automatically
fly secrets set JWT_SECRET="$(openssl rand -hex 32)"

# apply the schema once
fly postgres connect -a seatlock-db < db/schema.sql

# deploy
fly deploy
```

Then verify the live instance:

```bash
./scripts/verify.sh https://seatlock.fly.dev
./burst.sh --url https://seatlock.fly.dev $(./scripts/seed.sh 50 https://seatlock.fly.dev) A12 500
```

The burst against a remote host runs from your machine rather than inside
Docker, so results include real network latency — expect higher percentiles
than the local run, and the same outcome distribution.
