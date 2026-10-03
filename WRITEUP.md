# WRITEUP

How SeatLock decides who gets a seat, why it is built this way, and what it
costs.

---

## 1. The thesis

**All correctness lives in one SQL statement and a handful of constraints. The
Go layer is a stateless shell.**

That is the whole design. It has one consequence worth stating up front: there
is no distributed lock, no Redis, no coordination of any kind between API
processes. You can run one replica or ten and the guarantees do not change,
because no guarantee depends on anything held in a process.

The alternative — a lock service in front of a database that already has
locks — would add a second source of truth that can disagree with the first.
That is strictly worse, not more scalable.

---

## 2. The atomic decision

```sql
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
RETURNING label;
```

Three things are load-bearing here.

**`FOR UPDATE` with the predicate inside the lock.** Under `READ COMMITTED`,
when transaction B's `SELECT ... FOR UPDATE` meets a row that A has locked, B
blocks; when A commits, B *re-reads the row and re-evaluates the `WHERE`*. A
has set `status = 'confirmed'`, so B's predicate no longer matches and B's CTE
returns zero rows. Of 500 racing transactions, exactly one matches. That is not
a check we perform — it is what the lock does.

**`ORDER BY id`.** Every transaction acquires seat locks in the same physical
order. Without it, a request for `[A12, A13]` and a concurrent one for
`[A13, A12]` can each hold what the other wants, and Postgres resolves it by
killing one with a deadlock error — a 5xx. `TestMultiSeatOppositeOrderDoesNotDeadlock`
runs 50 such pairs and fails if any error is not a domain outcome.

**Comparing rows returned to seats requested.** The statement grabs whatever is
available. If that count is short, the transaction rolls back and the client
gets `409 seats_unavailable`. This is where all-or-nothing is enforced.

### Why all-or-nothing

Best-effort partial fulfilment would mean one idempotency key no longer maps to
one seat set, so a replay could not be compared against the original request.
It also makes the 409 ambiguous: *which* seat failed, and does the client still
owe money for the others? All-or-nothing makes both questions disappear.

---

## 3. The two locks, and their order

A booking takes locks in exactly this order, always:

1. `user_show_quota(user_id, show_id)` — `FOR UPDATE`
2. the seat rows — `ORDER BY id`, `FOR UPDATE`

Cancel and confirm take the same locks in the same order. A globally consistent
lock order means the wait-for graph cannot contain a cycle, so no pair of
operations in this system can deadlock against another.

### The quota lock is the non-obvious one

A per-user limit looks like a simple count-and-compare. It is not. If one user
fires four concurrent requests while holding three seats, all four transactions
read `count = 3`, all four pass the check, all four grab *different* seats, and
all four commit. The user ends up with seven. Nothing in the seat locking
prevents this, because the four requests never contend for the same row.

The fix is to take a lock on something all four requests share. The
`user_show_quota` table exists for that and nothing else:

```sql
SELECT user_id FROM user_show_quota
 WHERE user_id = $1 AND show_id = $2::uuid FOR UPDATE;
```

Taken *before* the count is read, it serialises one user's requests for one
show while leaving different users completely parallel.

`TestPerUserLimitUnderConcurrency` fires 40 simultaneous requests from one user
at 40 *uncontended* seats. Exactly 4 succeed.

### The table has no counter column, on purpose

The first version stored `seats_taken` and incremented it. That is a denormalised
count, and denormalised counts drift — a cancel that fails to decrement, a hold
that lapses with nobody to notice, a crash between two statements.

Since the transaction already holds the lock, the count can simply be derived:

```sql
SELECT count(*) FROM seats s JOIN reservations r ON r.id = s.reservation_id
 WHERE s.show_id = $2::uuid AND r.user_id = $1
   AND (s.status = 'confirmed' OR (s.status = 'held' AND s.held_until >= now()))
```

It cannot drift, it handles lapsed holds for free, and the row goes back to
being what it always was: a mutex.

---

## 4. Idempotency

Stored in Postgres, keyed `(user_id, show_id, key)`, holding a SHA-256
fingerprint of the sorted seat labels and the resulting reservation id.

**Why not Redis.** The key and the booking must agree. In Postgres they commit
in the same transaction, so there is no state in which one exists without the
other. With Redis as the key store there is a window — key written, process
dies, booking never commits — and that key is now poisoned forever. Redis buys
speed on an operation that was never the bottleneck, in exchange for a
consistency problem that did not previously exist.

**Why scoped to the show.** Initially the key was `(user_id, key)`. A user who
reuses `"checkout-1"` on a second show would be handed their *first* show's
reservation. Found by the load generator colliding with its own previous run;
fixed by putting `show_id` in the primary key. `TestIdempotencyKeyIsScopedToShow`
pins it.

**Why the fingerprint is order-independent.** `["A1","A2"]` and `["A2","A1"]`
are the same request. A client retrying with a shuffled array should get a
replay, not a 409.

**The concurrent-retry case.** Two requests can carry the same key at the same
instant — the usual cause is an HTTP client transparently retrying a POST whose
connection dropped. Both pass the replay check (neither sees a row yet), both
proceed, and the loser hits the unique constraint at commit. The first version
returned `409 idempotency_key_reused`, which is a *lie*: the seats were
identical. Now that collision is caught, the transaction rolls back, and the
request re-reads the winner's committed key and replays its reservation.

---

## 5. Holds expire without a sweeper

A hold writes `held_until`. Nothing scans for expired holds. Instead the
availability predicate treats a lapsed hold as free:

```sql
(status = 'available' OR (status = 'held' AND held_until < now()))
```

The seat is reclaimed by whoever wants it next, inside the same locked
statement that grabs it.

This is strictly better than a background sweeper:

- **No race with confirm.** A sweeper and a confirm can both act on the same
  hold at the same moment, and ordering them correctly needs yet another lock.
  Here, expiry is evaluated under the seat row lock, by the same statement that
  does the grab — there is no second actor to race.
- **No clock coordination.** One `now()`, Postgres's, used by every path.
- **No job to operate.** Nothing to schedule, monitor, or restart.

The read API reports a lapsed hold as `available`, because it is: the next
reserver will get it. `TestExpiredHoldIsReclaimedLazily` asserts the full
sequence — hold blocks, lapses, is reclaimed by someone else, and the original
holder's `/confirm` returns `409 hold_expired` rather than half-confirming.

The cost: a lapsed hold occupies a row with `status = 'held'` until someone
wants that seat. For a system whose seats are all eventually contended, that is
free. For one with permanently cold inventory, a periodic cleanup would be
worth adding — not for correctness, only for tidiness.

---

## 6. Why the invariant was not enough

The obvious oracle for a seat system is:

```
available + held + confirmed == total
```

It is necessary and it is **not sufficient**, which I found by building the
broken implementation and discovering it passed.

In `UNSAFE_MODE` the naive code checks availability, then writes without
holding a lock across the gap. When two writers collide, the second
**overwrites the first's `reservation_id`**. The number of seat rows never
changes, and each row is still in exactly one bucket — so the invariant is
perfectly satisfied while fifteen users have each been told they own A12.

The missing check compares promises to reality:

```
201 responses == seats actually confirmed
```

Both run after every burst:

```
UNSAFE   seat math     available 49 + held 0 + confirmed 1 = 50 / 50   OK
         double-sell   15 requests told 201, 1 seat confirmed          FAIL
```

The general lesson: an invariant over stored state cannot see a bug that
preserves the shape of that state. Something has to compare what the system
*told clients* against what it *stored*.

---

## 7. No 5xx under load

The requirement is that overload produces domain outcomes, never server faults.
Four things deliver it.

**Errors are values.** Go has no exceptions, so there is no unhandled error
that can propagate into a 500. Every failure path is an explicit return the
compiler forces you to handle.

**One mapping function.** `statusFor()` is the only place an error becomes a
status code. `TestEveryDomainErrorMapsTo4xx` iterates the full list of domain
errors and fails if any maps outside 4xx.

**Unknown errors are 503, not 500.** The realistic cause of an unrecognised
error is the database being unreachable, and 503 is the honest answer — it also
tells a load balancer to retry elsewhere, which 500 does not.

**Bounded pool, bounded wait.** `DB_POOL_MAX` caps connections and
`QUERY_TIMEOUT` puts a deadline on every query via `context.Context`. Overload
queues in the application and eventually returns 503. It cannot hang, and it
cannot stampede Postgres into opening a backend process per request.

A smaller pool is often *faster* under contention on a hot seat: waiting cheaply
in Go beats waiting expensively inside Postgres.

---

## 8. Failure modes

| Failure | What prevents it | What proves it |
|---|---|---|
| Two users get the same seat | predicate evaluated under `FOR UPDATE`; rowcount compared to request | `TestNoDoubleSell`, `burst.sh` |
| A reservation claims a seat it does not own | double-sell check in reconciliation | `UNSAFE_MODE` demo |
| One user exceeds the per-show limit | quota row locked before the count is read | `TestPerUserLimitUnderConcurrency` |
| A count drifts from reality | count is derived under the lock, never stored | by construction |
| Duplicate request books twice | `PRIMARY KEY (user_id, show_id, key)` in the same transaction | `TestIdempotentReplayReturnsOriginal` |
| Same key, different seats | seat fingerprint compared on replay | `TestIdempotencyKeyWithDifferentSeatsIsRejected` |
| Same key across two shows | `show_id` in the primary key | `TestIdempotencyKeyIsScopedToShow` |
| Concurrent retry of one key | unique violation caught, winner's result replayed | observed under burst load |
| Crash between seat write and key write | both in one transaction — not representable | by construction |
| `[A12,A13]` vs `[A13,A12]` deadlock | `ORDER BY id` in the locking CTE | `TestMultiSeatOppositeOrderDoesNotDeadlock` |
| Cancel releases someone else's seat | `WHERE reservation_id = $1` — cannot address other rows | `TestCancelCannotTouchAnotherUsersSeats` |
| Partial booking on a lapsed hold | confirm requires every seat to still be held | `TestExpiredHoldIsReclaimedLazily` |
| Confirmed seat with no owner | `CHECK ((status = 'available') = (reservation_id IS NULL))` | database rejects the insert |
| Identity spoofed via the body | request struct has no `user_id` field | `verify.sh` |
| JWT algorithm confusion | signing method asserted to be HMAC; `WithValidMethods` | — |
| Database down | readiness fails closed; liveness unaffected | manual chaos test |
| Pool exhausted | bounded pool + query deadline → 503 | burst at high N |
| Torn read of the show summary | one statement, one snapshot | by construction |

---

## 9. Bugs found during development

Each of these was live at some point and is now covered by a test.

1. **`pgconn` imported from the v4 module path** while the driver was pgx v5.
   `errors.As(err, &pgErr)` compared against a type that was never returned, so
   duplicate-seat detection silently never fired. The code compiled and the
   happy path worked.
2. **False `409 idempotency_key_reused` on concurrent retries** — described in
   §4. The service was declining requests it should have replayed.
3. **Idempotency keys not scoped to the show** — a key reused on a second show
   returned the first show's reservation.
4. **The reconciliation oracle had a blind spot** — §6. The most valuable of
   the four: without it the project would have *appeared* to pass its own load
   test while double-selling.
5. **`go get ...@latest` bumped the `go` directive to 1.25** while the
   Dockerfile pinned 1.23. The local toolchain auto-upgraded and hid it; the
   container sets `GOTOOLCHAIN=local` and failed. Versions are now aligned
   across `go.mod`, the Dockerfile, and CI.

---

## 10. Limits, honestly

**One hot seat serialises.** A seat is a row; concurrent buyers queue on its
lock. Measured on a laptop: 500 simultaneous requests for one seat resolve in
~320ms wall, p99 ~320ms, ~1,500 req/s, zero failures. Different seats scale
linearly — the contention is per-row, not global.

If one seat genuinely needed more, the fix would be to shard it into a token
pool and hand out claims. That trades exactness for throughput, and for
ticketing exactness *is* the product. Not worth it.

**The bottleneck is Postgres, by design.** Adding API replicas adds no capacity
for a contended seat, because the serialisation point is the row. It does add
availability and headroom for everything else.

**`verify.sh` runs against one process.** It does not prove the guarantees hold
across replicas — though nothing in the design is process-local, so there is
nothing for a second replica to break. Running the burst against several
replicas behind a load balancer would close that gap.

**Metrics gauges refresh on read.** `seatlock_seats` is updated when a show is
fetched, not on a timer. Exact whenever anyone is watching; stale for a show
nobody queries. Acceptable, and worth knowing.

**No payment step.** `hold_seconds` exists so one can be added without changing
the API, but nothing charges anybody.

---

## 11. What I would do next

- Run the burst against three replicas behind nginx, to demonstrate rather than
  argue that correctness is not process-local.
- A periodic reaper for lapsed holds on cold inventory — tidiness, not
  correctness.
- Idempotency key TTL; the table grows without bound today.
- `SELECT ... FOR UPDATE NOWAIT` on the seat grab, converting a lock wait into
  an immediate 409. Lower latency under extreme contention, at the cost of
  declining some requests that would have succeeded a few milliseconds later.
  Worth measuring before adopting.
