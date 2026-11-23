#!/usr/bin/env bash
set -euo pipefail

cd "$(dirname "$0")/.."

echo "== 1/10 gofmt =="
unformatted="$(gofmt -l *.go scripts/*/*.go)"
if [ -n "$unformatted" ]; then
  echo "gofmt needed for:" >&2
  echo "$unformatted" >&2
  exit 1
fi

echo "== 2/10 go vet =="
CGO_ENABLED=0 go vet ./...

echo "== 3/10 go build =="
CGO_ENABLED=0 go build -trimpath -buildvcs=false -o walsy-system1 .

echo "== 4/10 go test -race =="
# The race detector needs cgo; do not inherit an ambient CGO_ENABLED=0.
CGO_ENABLED=1 go test -race -count=1 ./...

echo "== 5/10 lrecon digest =="
LRECON="${LRECON:-/Users/keremurhan/.config/opencode/skills/walsyrea/macos/lrecon}"
RECON=/private/var/folders/_h/qvftqp9j125dppqpdv8kyb8m0000gn/T/opencode/walsy_verify_lrecon.json
"$LRECON" --json ./walsy-system1 > "$RECON"
go run ./scripts/recondigest "$RECON"

echo "== 6/10 non-ascii scan =="
files=()
for f in *.go web/*; do
  [ -e "$f" ] || continue
  files+=("$f")
done
go run ./scripts/asciiscan "${files[@]}"

echo "== 7/10 subpackage vet =="
CGO_ENABLED=0 go vet ./scripts/...

echo "== 8/10 decision_items signature =="
shasum -a 256 -c decision_items.json.sha256.sig

echo "== 9/10 release gate =="
go run ./scripts/releasegate -so-final-complete 0.8

echo "== 10/10 verdict =="
echo "PASS"
