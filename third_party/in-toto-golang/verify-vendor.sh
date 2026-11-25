#!/usr/bin/env bash
# Fails if third_party/in-toto-golang drifts from the patch documented
# in VENDOR.md: re-downloads the pinned upstream, keeps the same subset,
# re-applies the tag narrowing, re-generates the js stub, and diffs.
set -euo pipefail

HERE="$(dirname "$(readlink -f "$0")")"
PIN="v0.11.0"
PKGS=(
  in_toto
  in_toto/slsa_provenance/common
  in_toto/slsa_provenance/v0.1
  in_toto/slsa_provenance/v0.2
  in_toto/slsa_provenance/v1
)

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
cd "$TMP"
go mod init -q tmpvendorcheck 2>/dev/null || true
DIR="$(go mod download -json "github.com/in-toto/in-toto-golang@$PIN" | python3 -c 'import json,sys; print(json.load(sys.stdin)["Dir"])')"

EXP="$TMP/expected"
mkdir -p "$EXP"
cp "$DIR/go.mod" "$DIR/LICENSE" "$EXP/"
for pkg in "${PKGS[@]}"; do
  mkdir -p "$EXP/$pkg"
  for f in "$DIR/$pkg"/*.go; do
    case "$f" in
      *_test.go) continue ;;
      *) cp "$f" "$EXP/$pkg/" ;;
    esac
  done
done
# VENDOR.md change 1: narrow the unix build tag (js has no unix.Access).
sed -i -e 's#darwin || !windows#darwin#' -e 's#+build linux darwin !windows#+build linux darwin#' "$EXP/in_toto/util_unix.go"
# VENDOR.md change 2: the js stub.
cat > "$EXP/in_toto/util_js.go" <<'EOF'
//go:build js

package in_toto

func isWritable(path string) error { return nil }
EOF

fail=0
while IFS= read -r -d '' f; do
  rel="${f#"$EXP"/}"
  if ! cmp -s "$f" "$HERE/$rel"; then
    echo "drifted file: $rel" >&2
    fail=1
  fi
done < <(find "$EXP" -type f -print0)
while IFS= read -r -d '' f; do
  rel="${f#"$HERE"/}"
  case "$rel" in
    VENDOR.md|verify-vendor.sh) continue ;;
  esac
  if [ ! -e "$EXP/$rel" ]; then
    echo "extra file not from upstream: $rel" >&2
    fail=1
  fi
done < <(find "$HERE" -type f -print0)
if [ "$fail" -ne 0 ]; then
  echo "vendored in-toto-golang drifted from upstream $PIN + documented patch" >&2
  exit 1
fi
echo "vendored in-toto-golang matches upstream $PIN + documented patch"
