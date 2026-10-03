#!/usr/bin/env bash
# Exercises every requirement against a running instance and prints a
# pass/fail checklist. Exits non-zero if anything fails.
#
#   ./scripts/verify.sh                      # against localhost:8080
#   ./scripts/verify.sh https://your.app     # against a deployment
#
# Needs only bash + curl. No jq, no test framework.
set -uo pipefail

BASE="${1:-http://localhost:8080}"
PASS=0
FAIL=0

green() { printf '  \033[32mPASS\033[0m  %s\n' "$1"; PASS=$((PASS+1)); }
red()   { printf '  \033[31mFAIL\033[0m  %s\n       expected %s, got %s\n' "$1" "$2" "$3"; FAIL=$((FAIL+1)); }

check() { # check <description> <expected> <actual>
  if [[ "$2" == "$3" ]]; then green "$1"; else red "$1" "$2" "$3"; fi
}

# --- helpers -----------------------------------------------------------------
BODY=$(mktemp)
trap 'rm -f "$BODY"' EXIT

token() { curl -fsS -X POST "$BASE/dev/token?user_id=$1${2:+&admin=1}" | field token; }
field() { sed -E "s/.*\"$1\":\"?([^\",}]*)\"?.*/\1/"; }

post() { # post <path> <token> [json] -> prints status, body in $BODY
  curl -sS -o "$BODY" -w '%{http_code}' -X POST "$BASE$1" \
    -H "Authorization: Bearer $2" -H 'Content-Type: application/json' \
    -d "${3:-\{\}}"
}
get() { curl -sS -o "$BODY" -w '%{http_code}' "$BASE$1"; }

summary() { # summary <field>
  curl -fsS "$BASE/shows/$SHOW" | sed -E "s/.*\"$1\":([0-9]+).*/\1/"
}

echo
echo "SeatLock verification against $BASE"
echo "============================================================"

# --- setup -------------------------------------------------------------------
ADMIN=$(token admin 1)
ALICE=$(token alice)
BOB=$(token bob)
CAROL=$(token carol)   # untouched quota, for checks that must not hit the limit

SEATS=""; for i in $(seq 1 30); do SEATS="${SEATS}\"A${i}\","; done; SEATS="${SEATS%,}"
CODE=$(post /shows "$ADMIN" "{\"name\":\"verify\",\"seats\":[$SEATS],\"price_paise\":25000}")
SHOW=$(field show_id < "$BODY")
check "POST /shows creates a show" 201 "$CODE"
check "  ...with 30 available seats" 30 "$(summary available)"

# --- auth --------------------------------------------------------------------
CODE=$(curl -sS -o /dev/null -w '%{http_code}' -X POST "$BASE/shows/$SHOW/reserve" \
       -H 'Content-Type: application/json' -d '{"seats":["A1"],"idempotency_key":"x"}')
check "reserve without a token is rejected" 401 "$CODE"

CODE=$(post /shows "$ALICE" "{\"name\":\"nope\",\"seats\":[\"A1\"],\"price_paise\":1}")
check "non-admin cannot create a show" 403 "$CODE"

# --- the core booking path ---------------------------------------------------
CODE=$(post "/shows/$SHOW/reserve" "$ALICE" '{"seats":["A1"],"idempotency_key":"k1"}')
RES1=$(field reservation_id < "$BODY")
AMOUNT=$(sed -E 's/.*"amount_paise":([0-9]+).*/\1/' < "$BODY")
check "reserve a free seat" 201 "$CODE"
check "  ...priced in integer paise" 25000 "$AMOUNT"

CODE=$(post "/shows/$SHOW/reserve" "$BOB" '{"seats":["A1"],"idempotency_key":"k2"}')
check "a taken seat is declined, not errored" 409 "$CODE"
check "  ...with a machine-readable reason" "seats_unavailable" "$(field error < "$BODY")"

# --- identity cannot come from the body --------------------------------------
CODE=$(post "/shows/$SHOW/reserve" "$BOB" '{"seats":["A2"],"idempotency_key":"k3","user_id":"alice"}')
check "body-supplied user_id is ignored" 201 "$CODE"
check "  ...identity taken from the token" "bob" "$(field user_id < "$BODY")"

# --- idempotency -------------------------------------------------------------
CODE=$(post "/shows/$SHOW/reserve" "$ALICE" '{"seats":["A1"],"idempotency_key":"k1"}')
check "replaying a key returns the original" 200 "$CODE"
check "  ...same reservation id" "$RES1" "$(field reservation_id < "$BODY")"

CODE=$(post "/shows/$SHOW/reserve" "$ALICE" '{"seats":["A9"],"idempotency_key":"k1"}')
check "same key with different seats is rejected" 409 "$CODE"
check "  ...distinguished from a lost race" "idempotency_key_reused" "$(field error < "$BODY")"

# --- validation --------------------------------------------------------------
CODE=$(post "/shows/$SHOW/reserve" "$ALICE" '{"seats":["Z99"],"idempotency_key":"k4"}')
check "an unknown seat is a 400, not a 409" 400 "$CODE"

# --- all-or-nothing ----------------------------------------------------------
post "/shows/$SHOW/reserve" "$BOB" '{"seats":["A10"],"idempotency_key":"k5"}' >/dev/null
CODE=$(post "/shows/$SHOW/reserve" "$ALICE" '{"seats":["A11","A10"],"idempotency_key":"k6"}')
check "a partly-available request declines wholly" 409 "$CODE"
STATE=$(curl -fsS "$BASE/shows/$SHOW" | sed -E 's/.*"A11":"([a-z]+)".*/\1/')
check "  ...and leaves the free seat untouched" "available" "$STATE"

# --- per-user limit ----------------------------------------------------------
for i in 20 21 22; do
  post "/shows/$SHOW/reserve" "$ALICE" "{\"seats\":[\"A$i\"],\"idempotency_key\":\"lim$i\"}" >/dev/null
done
CODE=$(post "/shows/$SHOW/reserve" "$ALICE" '{"seats":["A23"],"idempotency_key":"lim23"}')
check "the per-user limit is enforced" 409 "$CODE"
check "  ...with its own reason" "per_user_limit_exceeded" "$(field error < "$BODY")"

# --- holds -------------------------------------------------------------------
CODE=$(post "/shows/$SHOW/reserve" "$BOB" '{"seats":["A25"],"idempotency_key":"h1","hold_seconds":2}')
HELD=$(field reservation_id < "$BODY")
check "a seat can be held rather than confirmed" 201 "$CODE"
check "  ...and reads as held" "held" "$(field status < "$BODY")"
check "  ...visible in the show summary" 1 "$(summary held)"

CODE=$(post "/reservations/$HELD/confirm" "$BOB")
check "a live hold can be confirmed" 200 "$CODE"
check "  ...and becomes confirmed" "confirmed" "$(field status < "$BODY")"

CODE=$(post "/shows/$SHOW/reserve" "$BOB" '{"seats":["A26"],"idempotency_key":"h2","hold_seconds":1}')
LAPSE=$(field reservation_id < "$BODY")
sleep 2
check "a lapsed hold frees its seat with no sweeper" 0 "$(summary held)"
CODE=$(post "/shows/$SHOW/reserve" "$CAROL" '{"seats":["A26"],"idempotency_key":"h3"}')
check "  ...and the next buyer gets the seat" 201 "$CODE"
CODE=$(post "/reservations/$LAPSE/confirm" "$BOB")
check "  ...while the original holder cannot confirm" 409 "$CODE"

# --- cancel ------------------------------------------------------------------
CODE=$(post "/reservations/$RES1/cancel" "$ALICE")
check "cancel releases the seats" 200 "$CODE"
CODE=$(post "/reservations/$RES1/cancel" "$ALICE")
check "  ...and is idempotent" 200 "$CODE"
CODE=$(post "/reservations/$RES1/cancel" "$BOB")
check "another user cannot cancel it" 404 "$CODE"

# --- invariant ---------------------------------------------------------------
A=$(summary available); H=$(summary held); C=$(summary confirmed); T=$(summary total)
check "available + held + confirmed == total" "$T" "$((A+H+C))"

# --- operational -------------------------------------------------------------
check "GET /healthz" 200 "$(get /healthz)"
check "GET /readyz" 200 "$(get /readyz)"
check "GET /metrics exposes confirmations" 0 \
  "$(curl -fsS "$BASE/metrics" | grep -cq '^seatlock_reservations_confirmed_total'; echo $?)"
check "  ...and declines by reason" 0 \
  "$(curl -fsS "$BASE/metrics" | grep -cq 'seatlock_reservations_declined_total{reason='; echo $?)"

# --- the stampede ------------------------------------------------------------
echo
echo "------------------------------------------------------------"
echo "  500-way stampede on a single seat"
echo "------------------------------------------------------------"
STAMPEDE_SHOW=$("$(dirname "$0")/seed.sh" 50 "$BASE")

# Without --url, burst.sh runs inside the docker network and would hammer the
# LOCAL stack while the rest of this script checked a remote one.
BURST_ARGS=()
[[ "$BASE" != "http://localhost:8080" ]] && BURST_ARGS=(--url "$BASE")

if "$(dirname "$0")/../burst.sh" "${BURST_ARGS[@]}" "$STAMPEDE_SHOW" A12 500 2>&1 | tail -14; then
  green "stampede: one winner, no double-sell, invariant intact"
else
  red "stampede" "all checks pass" "see output above"
fi

# --- result ------------------------------------------------------------------
echo
echo "============================================================"
printf "  %d passed, %d failed\n" "$PASS" "$FAIL"
echo "============================================================"
echo
[[ $FAIL -eq 0 ]]
