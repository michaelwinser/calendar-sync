#!/bin/sh
# UC-0029: POST /api/sync returns 400 when no config exists.
set -e

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
PROJECT_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"

BINARY="$PROJECT_DIR/calendar-sync"
PORT=14007
TEST_DATA=$(mktemp -d)
PID=""
FAILURES=0

if [ -t 1 ]; then
    GREEN='\033[0;32m'
    RED='\033[0;31m'
    DIM='\033[2m'
    NC='\033[0m'
else
    GREEN='' RED='' DIM='' NC=''
fi

cleanup() {
    if [ -n "$PID" ] && kill -0 "$PID" 2>/dev/null; then
        kill "$PID" 2>/dev/null || true
        wait "$PID" 2>/dev/null || true
    fi
    rm -rf "$TEST_DATA"
}
trap cleanup EXIT

echo "Building calendar-sync..."
(cd "$PROJECT_DIR" && go build -o calendar-sync .)

export PORT="$PORT" STORE_TYPE=sqlite SQLITE_DB_PATH="$TEST_DATA/app.db" AUTH_MODE=dev DEV_USER_EMAIL="test@example.com"
"$BINARY" serve >"$TEST_DATA/server.log" 2>&1 &
PID=$!

for i in 1 2 3 4 5; do
    if curl -sf "http://localhost:$PORT/health" >/dev/null 2>&1; then break; fi
    sleep 1
done

BASE="http://localhost:$PORT"

echo ""
echo "=== Sync API Tests ==="
echo ""

# POST /api/sync with no config → 400
printf "${DIM}POST /api/sync (no config)${NC}\n"
RESP=$(curl -s -w "\n%{http_code}" -X POST "$BASE/api/sync")
HTTP_CODE=$(echo "$RESP" | tail -1)
BODY=$(echo "$RESP" | sed '$d')
echo "$BODY (HTTP $HTTP_CODE)"

if [ "$HTTP_CODE" = "400" ]; then
    printf "${GREEN}  PASS: sync returns 400 with no config${NC}\n"
else
    printf "${RED}  FAIL: expected 400, got $HTTP_CODE${NC}\n"
    FAILURES=$((FAILURES + 1))
fi

echo ""

# GET /api/sync/logs → empty list
printf "${DIM}GET /api/sync/logs${NC}\n"
RESP=$(curl -sf "$BASE/api/sync/logs")
echo "$RESP"

if echo "$RESP" | grep -q '^\[\]$'; then
    printf "${GREEN}  PASS: sync logs empty${NC}\n"
else
    printf "${RED}  FAIL: expected empty array${NC}\n"
    FAILURES=$((FAILURES + 1))
fi

echo ""

# GET /api/sync/events → empty list
printf "${DIM}GET /api/sync/events${NC}\n"
RESP=$(curl -sf "$BASE/api/sync/events")
echo "$RESP"

if echo "$RESP" | grep -q '^\[\]$'; then
    printf "${GREEN}  PASS: synced events empty${NC}\n"
else
    printf "${RED}  FAIL: expected empty array${NC}\n"
    FAILURES=$((FAILURES + 1))
fi

echo ""
echo "=== Results ==="
if [ "$FAILURES" -gt 0 ]; then
    printf "${RED}SYNC TESTS FAILED: %d check(s) failed${NC}\n" "$FAILURES"
    exit 1
else
    printf "${GREEN}ALL SYNC TESTS PASSED${NC}\n"
fi
