#!/usr/bin/env bash
# Simulates an on-sale stampede and reconciles the result.
#
#   ./burst.sh <show-id> [seat] [count]
#   ./burst.sh --url https://seatlock.fly.dev <show-id> A12 500
#   ./burst.sh --quota <show-id>          # one user, many seats, tests the limit
#
# With no --url, the generator runs inside the docker network, which avoids the
# host port proxy refusing connections under a few hundred simultaneous dials.
set -euo pipefail

URL=""
MODE="seat"
while [[ $# -gt 0 ]]; do
  case "$1" in
    --url)   URL="$2"; shift 2 ;;
    --quota) MODE="quota"; shift ;;
    *) break ;;
  esac
done

SHOW="${1:-}"
SEAT="${2:-A12}"
COUNT="${3:-500}"

if [[ -z "$SHOW" ]]; then
  echo "usage: ./burst.sh [--url BASE] [--quota] <show-id> [seat] [count]" >&2
  exit 2
fi

ARGS=(-show "$SHOW" -seat "$SEAT" -n "$COUNT")
[[ "$MODE" == "quota" ]] && ARGS=(-show "$SHOW" -n "${3:-40}" -one-user -spread)

if [[ -n "$URL" ]]; then
  # Remote target: run the generator locally against the public URL.
  exec go run ./cmd/burst -url "$URL" "${ARGS[@]}"
fi

exec docker compose run --rm --quiet-pull burst -url http://api:8080 "${ARGS[@]}"
