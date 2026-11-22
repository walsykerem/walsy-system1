#!/usr/bin/env bash
set -euo pipefail

cd "$(dirname "$0")/.."

echo "== 1/8 gofmt =="
unformatted="$(gofmt -l *.go scripts/*/*.go)"
if [ -n "$unformatted" ]; then
  echo "gofmt needed for:" >&2
  echo "$unformatted" >&2
  exit 1
fi

echo "== 2/8 go vet =="
CGO_ENABLED=0 go vet ./...

echo "== 3/8 go test -race =="
# The race detector needs cgo; do not inherit an ambient CGO_ENABLED=0.
CGO_ENABLED=1 go test -race -count=1 ./...

echo "== 4/8 go build =="
CGO_ENABLED=0 go build -o walsy-system1 .

echo "== 5/8 lrecon digest =="
LRECON=/Users/keremurhan/.config/opencode/skills/walsyrea/macos/lrecon
RECON=/private/var/folders/_h/qvftqp9j125dppqpdv8kyb8m0000gn/T/opencode/walsy_verify_lrecon.json
"$LRECON" --json ./walsy-system1 > "$RECON"
python3 - "$RECON" <<'PY'
import json
import sys

with open(sys.argv[1], encoding="utf-8") as fh:
    d = json.load(fh)
target = d.get("target") or {}
digest = d.get("digest") or target.get("file_sha256") or d.get("uuid") or ""
kind = d.get("kind") or ""
print("lrecon digest:", digest)
print("lrecon kind:", kind)
PY

echo "== 6/8 non-ascii scan =="
python3 - <<'PY'
import glob
import os
import sys

files = sorted(set(glob.glob("*.go") + glob.glob("web/*")))
bad = []
for path in files:
    if not os.path.isfile(path):
        continue
    with open(path, "rb") as fh:
        data = fh.read()
    for n, line in enumerate(data.decode("utf-8", "replace").splitlines(), 1):
        if any(ord(ch) > 127 for ch in line):
            bad.append((path, n))
            break
if bad:
    for path, n in bad:
        print("non-ascii: %s:%d" % (path, n), file=sys.stderr)
    sys.exit(1)
print("ascii clean:", len(files), "files")
PY

echo "== 7/8 subpackage vet =="
CGO_ENABLED=0 go vet ./scripts/...

echo "== 8/8 verdict =="
echo "PASS"
