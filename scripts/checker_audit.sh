#!/usr/bin/env bash
set -euo pipefail

cd "$(dirname "$0")/.."

# audit.jsonl is exercised only through tests that set HOME to their own TempDir.
# This script never reads or writes the real audit file; HOME is redirected.
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

export HOME="$tmp"
export CGO_ENABLED=0
export GOCACHE="${GOCACHE:-/private/var/folders/_h/qvftqp9j125dppqpdv8kyb8m0000gn/T/opencode/gocache}"

go test -count=1 ./internal/app -run 'TestRelayLogRecordsOutcomeWithoutPayload|TestBridgeLogsBothDirections|TestAuditRotatesAtCap|TestItem2APILogsFromAuditJSONL|TestLogCallCapsRingAndCounts|TestUsageLineFlattensNumbers|TestLogsSheetRendersRecordedCall|TestLogTimestampsUseLocalClock' ./internal/app

echo AUDIT-OK
