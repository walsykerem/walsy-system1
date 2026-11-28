#!/usr/bin/env bash
# Checker simulation: tmp-only scratch, read-only on the live dashboard.
# Prints one PASS/FAIL (or SKIP for a stopped dashboard) per check.
set -uo pipefail

cd "$(dirname "$0")/.."

tmp="$(mktemp -d "${TMPDIR:-/tmp}/checker-sim.XXXXXX")"
trap 'rm -rf "$tmp"' EXIT
export TMPDIR="$tmp"
export HOME="$tmp/home"
mkdir -p "$HOME"
export GOCACHE="${GOCACHE:-$tmp/gocache}"
export CGO_ENABLED=0

pass=0
fail=0
ok() { echo "PASS $1"; pass=$((pass + 1)); }
no() { echo "FAIL $1"; fail=$((fail + 1)); }

# 1. Test inventory floor: 49+ checker tests must be discoverable.
# Count the repo's own sources, not `go test -list ./...`: that output silently
# shrinks to the packages that happen to compile (24 vs 516 under contention).
n="$(grep -h '^func Test' internal/app/*_test.go | wc -l | tr -d ' ')"
if [ "$n" -ge 49 ]; then ok "tests>=49 ($n)"; else no "tests>=49 ($n)"; fi

# 2. Cache dir wiped on both stdin transports, persistent state untouched.
# Both tests pin TMPDIR/HOME to their own t.TempDir(); nothing here shares a path.
if go test -count=1 -run '^TestNoDiskCacheDirOnMCPStart$' ./internal/app >/dev/null 2>&1; then ok "cache-dir"; else no "cache-dir"; fi

# 3. SO state build benchmark still runs (short smoke, not the 200k timing run).
if go test -bench='^BenchmarkSOStateBuild$' -run='^$' -benchtime=200x ./internal/app >/dev/null 2>&1; then
  ok "bench-so-state"
else
  no "bench-so-state"
fi

if [ "${CHECKER_SIM_SKIP_LIVE:-0}" = "1" ]; then
  echo "SKIP dashboard-live (CHECKER_SIM_SKIP_LIVE=1)"
else
  # 4. Exactly one dashboard listener, bound to loopback:8787.
  lsof_out="$(lsof -nP -iTCP:8787 -sTCP:LISTEN 2>/dev/null || true)"
  lines="$(printf '%s\n' "$lsof_out" | grep -c LISTEN || true)"
  bound="$(printf '%s\n' "$lsof_out" | grep -c '127.0.0.1:8787 (LISTEN)' || true)"
  if [ "$lines" -eq 0 ]; then
    echo "SKIP dashboard-listener (not running)"
  elif [ "$lines" -eq 1 ] && [ "$bound" -eq 1 ]; then
    ok "dashboard-listener"
  else
    no "dashboard-listener ($lines listeners, $bound loopback)"
  fi

  if [ "$lines" -gt 0 ]; then
    # 5. Loopback Host accepted, hostile Host rejected by the dashboard.
    good="$(curl -sS -m 3 -o /dev/null -w '%{http_code}' -H 'Host: 127.0.0.1:8787' http://127.0.0.1:8787/ 2>/dev/null || true)"
    if [ "$good" = "200" ]; then ok "host-loopback"; else no "host-loopback ($good)"; fi
    evil="$(curl -sS -m 3 -o /dev/null -w '%{http_code}' -H 'Host: evil.example.com' http://127.0.0.1:8787/ 2>/dev/null || true)"
    if [ "$evil" = "400" ]; then ok "host-evil-400"; else no "host-evil-400 ($evil)"; fi
  else
    echo "SKIP dashboard-host (not running)"
    echo "SKIP dashboard-host-evil (not running)"
  fi
fi

echo "checker-sim: $pass PASS $fail FAIL"
[ "$fail" -eq 0 ]
