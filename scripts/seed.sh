#!/usr/bin/env bash
# Creates a show and prints its id. Usage:
#   ./scripts/seed.sh [seat_count] [base_url]
set -euo pipefail

COUNT="${1:-50}"
BASE="${2:-http://localhost:8080}"

admin_token=$(curl -fsS -X POST "$BASE/dev/token?user_id=admin&admin=1" \
  | sed -E 's/.*"token":"([^"]*)".*/\1/')

seats=""
for i in $(seq 1 "$COUNT"); do
  seats="${seats}\"A${i}\","
done
seats="${seats%,}"

curl -fsS -X POST "$BASE/shows" \
  -H "Authorization: Bearer $admin_token" \
  -H 'Content-Type: application/json' \
  -d "{\"name\":\"friday-night\",\"seats\":[$seats],\"price_paise\":25000}" \
  | sed -E 's/.*"show_id":"([^"]*)".*/\1/'
