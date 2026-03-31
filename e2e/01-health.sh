#!/bin/sh
# UC-0001: App starts and serves UI. Health endpoint returns OK.
#
# Starts the server in the background, checks /health, then shuts down.
set -e

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
PROJECT_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"

BINARY="$PROJECT_DIR/calendar-sync"
PORT=14004  # Use a high ephemeral port to avoid conflicts
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

# Start server in background with a temp database and no auth
export PORT="$PORT"
export STORE_TYPE=sqlite
export SQLITE_DB_PATH="$TEST_DATA/app.db"
"$BINARY" serve >"$TEST_DATA/server.log" 2>&1 &
PID=$!

# Wait for server to be ready
for i in 1 2 3 4 5; do
    if curl -sf "http://localhost:$PORT/health" >/dev/null 2>&1; then
        break
    fi
    sleep 1
done

# Test health endpoint
printf "${DIM}GET /health${NC}\n"
RESPONSE=$(curl -sf "http://localhost:$PORT/health" 2>&1)
echo "$RESPONSE"

if echo "$RESPONSE" | grep -q '"status"'; then
    printf "${GREEN}UC-0001 PASSED: health endpoint returns OK${NC}\n"
else
    printf "${RED}UC-0001 FAILED: unexpected health response${NC}\n"
    exit 1
fi
