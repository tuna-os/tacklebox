# Vendored in-toto-golang

Upstream: `github.com/in-toto/in-toto-golang` at tag `v0.11.0`
(commit `36d782ffb2ca3adbffcdce1fd971c23319dd4469`,
2026-05-04). The repo is archived, so no upstream fix is coming.

## Why this copy exists

`github.com/sigstore/sigstore-go` (used by `internal/medium` for
installer-image signature verification) pulls `in-toto-golang` through
`rekor/pkg/types`. Upstream `in_toto/util_unix.go` carries the build
tag `linux || darwin || !windows`, which also matches `js/wasm`, but
calls `unix.Access`, which `golang.org/x/sys` does not implement for
js/wasm. That single file breaks `GOOS=js GOARCH=wasm go build
./cmd/tbwasm`. Wired in via `replace` in the root `go.mod`, so host
and wasm share the one sigstore-go verification implementation.

## What changed from upstream (the whole patch)

1. `in_toto/util_unix.go`: build tag narrowed from
   `linux || darwin || !windows` to `linux || darwin`. The file only
   implements `isWritable` via `unix.Access`; js/wasm never uses it.
2. `in_toto/util_js.go`: added, `//go:build js`, a stub
   `func isWritable(path string) error { return nil }`. Wasm callers
   never check path writability; the stub only satisfies the linker.
3. Omitted: upstream `_test.go` files, and every package outside the
   tacklebox import graph (`cmd`, `internal`, `test`, `test-infra`,
   `doc`, `certs`, `scripts`). Only `in_toto` and
   `in_toto/slsa_provenance/{common,v0.1,v0.2,v1}` plus `go.mod` and
   `LICENSE` are vendored.

`LICENSE` is the upstream Apache-2.0 text, unchanged.

## Drift check

`verify-vendor.sh` re-downloads the pinned upstream version, keeps the
same subset, re-applies change 1, re-generates change 2, and diffs the
result against this directory. It fails on any other difference. It
runs as part of `just check`.
