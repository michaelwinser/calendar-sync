#!/bin/sh
# UC-0011 through UC-0016: Config CRUD — set hub, add/remove sources, validation.
#
# Tests the config API directly (no Google Calendar calls needed).
set -e

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
PROJECT_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"

BINARY="$PROJECT_DIR/calendar-sync"
PORT=14006
TEST_DATA=$(mktemp -d)
PID=""
FAILURES=0

# Colors
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

check() {
    desc="$1"; expected="$2"; actual="$3"
    if echo "$actual" | grep -q "$expected"; then
        printf "${GREEN}  PASS: %s${NC}\n" "$desc"
    else
        printf "${RED}  FAIL: %s (expected: %s)${NC}\n" "$desc" "$expected"
        printf "${DIM}  got: %s${NC}\n" "$actual"
        FAILURES=$((FAILURES + 1))
    fi
}

# Build
echo "Building calendar-sync..."
(cd "$PROJECT_DIR" && go build -o calendar-sync .)

# Start server with dev auth mode (no real OAuth needed)
export PORT="$PORT"
export STORE_TYPE=sqlite
export SQLITE_DB_PATH="$TEST_DATA/app.db"
export AUTH_MODE=dev
export DEV_USER_EMAIL="test@example.com"
"$BINARY" serve >"$TEST_DATA/server.log" 2>&1 &
PID=$!

# Wait for server
for i in 1 2 3 4 5; do
    if curl -sf "http://localhost:$PORT/health" >/dev/null 2>&1; then
        break
    fi
    sleep 1
done

BASE="http://localhost:$PORT"

echo ""
echo "=== Config API Tests ==="
echo ""

# --- UC-0015: Get config (empty) ---
printf "${DIM}GET /api/config (empty)${NC}\n"
RESP=$(curl -sf "$BASE/api/config")
echo "$RESP"
check "UC-0015: empty config returns syncWindowWeeks 8" '"syncWindowWeeks":8' "$RESP"
check "UC-0015: empty config has empty sources" '"sources":\[\]' "$RESP"
echo ""

# --- UC-0011: Set hub calendar ---
printf "${DIM}PUT /api/config (set hub)${NC}\n"
RESP=$(curl -sf -X PUT "$BASE/api/config" \
    -H "Content-Type: application/json" \
    -d '{"hubCalendarId":"hub@example.com","hubCalendarName":"Hub","syncWindowWeeks":8,"sources":[]}')
echo "$RESP"
check "UC-0011: hub calendar set" '"hubCalendarId":"hub@example.com"' "$RESP"
echo ""

# --- UC-0013: Add source calendars ---
printf "${DIM}PUT /api/config (add sources)${NC}\n"
RESP=$(curl -sf -X PUT "$BASE/api/config" \
    -H "Content-Type: application/json" \
    -d '{"hubCalendarId":"hub@example.com","hubCalendarName":"Hub","syncWindowWeeks":8,"sources":[{"calendarId":"work@example.com","calendarName":"Work"},{"calendarId":"personal@example.com","calendarName":"Personal"}]}')
echo "$RESP"
check "UC-0013: two sources added" '"calendarId":"work@example.com"' "$RESP"
check "UC-0013: personal added" '"calendarId":"personal@example.com"' "$RESP"
echo ""

# --- UC-0014: Remove a source ---
printf "${DIM}PUT /api/config (remove personal)${NC}\n"
RESP=$(curl -sf -X PUT "$BASE/api/config" \
    -H "Content-Type: application/json" \
    -d '{"hubCalendarId":"hub@example.com","hubCalendarName":"Hub","syncWindowWeeks":8,"sources":[{"calendarId":"work@example.com","calendarName":"Work"}]}')
echo "$RESP"
check "UC-0014: personal removed" '"sources":\[{"calendarId":"work@example.com"' "$RESP"
echo ""

# --- UC-0015: Verify config ---
printf "${DIM}GET /api/config (verify)${NC}\n"
RESP=$(curl -sf "$BASE/api/config")
echo "$RESP"
check "UC-0015: hub persisted" '"hubCalendarId":"hub@example.com"' "$RESP"
check "UC-0015: one source persisted" '"calendarId":"work@example.com"' "$RESP"
echo ""

# --- UC-0016: Hub cannot be source ---
printf "${DIM}PUT /api/config (hub as source — should fail)${NC}\n"
RESP=$(curl -s -w "\n%{http_code}" -X PUT "$BASE/api/config" \
    -H "Content-Type: application/json" \
    -d '{"hubCalendarId":"hub@example.com","hubCalendarName":"Hub","syncWindowWeeks":8,"sources":[{"calendarId":"hub@example.com","calendarName":"Hub"}]}')
HTTP_CODE=$(echo "$RESP" | tail -1)
BODY=$(echo "$RESP" | sed '$d')
echo "$BODY (HTTP $HTTP_CODE)"
check "UC-0016: hub-as-source rejected" "400" "$HTTP_CODE"
check "UC-0016: error message" "hub calendar cannot" "$BODY"
echo ""

# --- UC-0016: No duplicate sources ---
printf "${DIM}PUT /api/config (duplicate source — should fail)${NC}\n"
RESP=$(curl -s -w "\n%{http_code}" -X PUT "$BASE/api/config" \
    -H "Content-Type: application/json" \
    -d '{"hubCalendarId":"hub@example.com","hubCalendarName":"Hub","syncWindowWeeks":8,"sources":[{"calendarId":"work@example.com","calendarName":"Work"},{"calendarId":"work@example.com","calendarName":"Work Dup"}]}')
HTTP_CODE=$(echo "$RESP" | tail -1)
BODY=$(echo "$RESP" | sed '$d')
echo "$BODY (HTTP $HTTP_CODE)"
check "UC-0016: duplicate source rejected" "400" "$HTTP_CODE"
check "UC-0016: error message" "duplicate source" "$BODY"
echo ""

# --- UC-0012: Change hub ---
printf "${DIM}PUT /api/config (change hub)${NC}\n"
RESP=$(curl -sf -X PUT "$BASE/api/config" \
    -H "Content-Type: application/json" \
    -d '{"hubCalendarId":"newhub@example.com","hubCalendarName":"New Hub","syncWindowWeeks":4,"sources":[{"calendarId":"work@example.com","calendarName":"Work"}]}')
echo "$RESP"
check "UC-0012: hub changed" '"hubCalendarId":"newhub@example.com"' "$RESP"
check "UC-0012: sync window updated" '"syncWindowWeeks":4' "$RESP"
echo ""

# Results
echo "=== Results ==="
if [ "$FAILURES" -gt 0 ]; then
    printf "${RED}CONFIG TESTS FAILED: %d check(s) failed${NC}\n" "$FAILURES"
    exit 1
else
    printf "${GREEN}ALL CONFIG TESTS PASSED${NC}\n"
fi
