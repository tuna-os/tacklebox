//go:build !(js && wasm)

package main

// tbwasm is browser-only (see main.go): syscall/js does not exist for any
// other GOOS/GOARCH. This stub exists only so `go build ./...` and
// `go test ./cmd/tbwasm/...` link on the host toolchain — it lets the
// pure-logic helpers in this package (refs.go) be unit-tested without a
// JS engine, per tacklebox#265/#267. Never invoked: nothing calls main()
// under `go test`, and no non-wasm binary of this package is shipped.
func main() {}
