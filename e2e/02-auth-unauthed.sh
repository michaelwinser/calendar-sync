#!/bin/sh
# UC-0002, UC-0004: Unauthenticated user sees login page. Auth status returns loggedIn: false.
#
# Starts the server, checks auth status without a session, verifies login page is served.
set -e

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
PROJECT_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"

BINARY="$PROJECT_DIR/calendar-sync"
PORT=14005
TEST_DATA=$(mktemp -d)
PID=""

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

# Build
echo "Building calendar-sync..."
(cd "$PROJECT_DIR" && go build -o calendar-sync .)

# Start server
export PORT="$PORT"
export STORE_TYPE=sqlite
export SQLITE_DB_PATH="$TEST_DATA/app.db"
"$BINARY" serve >"$TEST_DATA/server.log" 2>&1 &
PID=$!

# Wait for server
for i in 1 2 3 4 5; do
    if curl -sf "http://localhost:$PORT/health" >/dev/null 2>&1; then
        break
    fi
    sleep 1
done

FAILURES=0

# Test 1: Auth status returns loggedIn: false
printf "${DIM}GET /api/auth/status (no session)${NC}\n"
RESPONSE=$(curl -sf "http://localhost:$PORT/api/auth/status" 2>&1)
echo "$RESPONSE"

if echo "$RESPONSE" | grep -q '"loggedIn":false'; then
    printf "${GREEN}UC-0004 PASSED: auth status returns loggedIn: false${NC}\n\n"
else
    printf "${RED}UC-0004 FAILED: expected loggedIn: false${NC}\n\n"
    FAILURES=$((FAILURES + 1))
fi

# Test 2: Root page returns login content for unauthenticated user
printf "${DIM}GET / (no session)${NC}\n"
RESPONSE=$(curl -sf "http://localhost:$PORT/" 2>&1)

if echo "$RESPONSE" | grep -qi "sign in\|login\|google"; then
    printf "${GREEN}UC-0002 PASSED: unauthenticated user sees login page${NC}\n\n"
else
    printf "${RED}UC-0002 FAILED: expected login page content${NC}\n\n"
    FAILURES=$((FAILURES + 1))
fi

# Results
if [ "$FAILURES" -gt 0 ]; then
    printf "${RED}E2E FAILED: %d check(s) failed${NC}\n" "$FAILURES"
    exit 1
else
    printf "${GREEN}ALL PASSED${NC}\n"
fi
