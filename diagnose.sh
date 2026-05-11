#!/bin/bash
# Triage script for `cant run go run . start --web=:8080`.
# Prints everything goon would, plus the obvious environment culprits.
set -u

cd "$(dirname "$0")"

echo "──────── 1. go toolchain ─────────"
go version || { echo "  ✗ go not in PATH — install go 1.21+"; exit 0; }

echo
echo "──────── 2. compile only ─────────"
if ! go build ./... 2>&1 | tee /tmp/goon-build.err; then
  echo "  ✗ compile errors above — paste this entire output back to Claude"
  exit 0
fi
echo "  ✓ builds clean"

echo
echo "──────── 3. is anything already running? ─────────"
if [ -f storage/goon.pid ]; then
  PID=$(cat storage/goon.pid)
  echo "  found storage/goon.pid → pid=$PID"
  if kill -0 "$PID" 2>/dev/null; then
    echo "  ✗ goon is still running. Run: goon stop"
  else
    echo "  pid is dead — removing stale pidfile"
    rm -f storage/goon.pid
  fi
fi

echo
echo "──────── 4. is port :8080 free? ─────────"
if command -v lsof >/dev/null 2>&1; then
  if lsof -iTCP:8080 -sTCP:LISTEN >/dev/null 2>&1; then
    echo "  ✗ :8080 already bound:"
    lsof -iTCP:8080 -sTCP:LISTEN
  else
    echo "  ✓ :8080 is free"
  fi
fi

echo
echo "──────── 5. memory.json health ─────────"
if [ -f storage/memory.json ]; then
  if python3 -c "import json; json.load(open('storage/memory.json'))" 2>/tmp/mem.err; then
    echo "  ✓ memory.json parses"
  else
    echo "  ✗ memory.json corrupt:"
    cat /tmp/mem.err
    echo "  → mv storage/memory.json storage/memory.json.bak  # then retry"
  fi
fi

echo
echo "──────── 6. actually start it ─────────"
echo "  running: go run . start --web=:8080"
echo "  (Ctrl-C when you've seen enough)"
echo "──────────────────────────────────"
go run . start --web=:8080
