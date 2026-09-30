# AGENTS.md — agent guide for tuna-os/tacklebox

Tacklebox builds **multi-boot media** — a loop `.img`, a real block device, or
a UEFI ISO — from one or more bootc images, on a shared store with a single
`systemd-boot` ESP.

Read [`ARCHITECTURE.md`](ARCHITECTURE.md) first: it has the code layout, the
target/install abstractions and the on-media layout. [`README.md`](README.md)
covers features and usage, [`TODO.md`](TODO.md) known bugs,
[`CONTRIBUTING.md`](CONTRIBUTING.md) the local validation commands. This file
is about the things that are only learnable by getting them wrong.

## Root context decides what the code does

`rootContext()` in `internal/install` is the switch under a lot of behaviour:
when EUID is 0 (or `TACKLEBOX_CONTEXT=root`), container work runs directly
against root's store; otherwise, and with `SUDO_USER` set, commands drop back
to that user with `sudo -u … -H --preserve-env=…`. `TACKLEBOX_CONTEXT=user`
forces the legacy drop.

That matters for testing: **`go test` behaves differently depending on who
runs it.** Any test asserting the drop must pin `TACKLEBOX_CONTEXT`
explicitly, or it silently tests the other branch. Running as root is normal
here — tacklebox itself is invoked under `sudo` — so do not assume a green
suite on CI means a green suite locally.

## What CI actually gates, and what it costs

| Job | Trigger | Cost | What it proves |
|---|---|---|---|
| `lint-test` | every PR | ~2 min | `go vet`, `go test`, `go build`, integration `Help` tests, every `examples/`+`fixtures/` recipe parses, shellcheck on the dracut modules |
| `verify-smoke` | after `lint-test` | ≤25 min | 2-env block image → `verify` → `update` → `add`/`remove` → QEMU boot |
| `iso-smoke` | after `lint-test` | ≤60 min | per-env, dedup, 6-env dedup, delta and offline-store ISOs, each QEMU-booted |

`ci.yml` also runs on a daily schedule (04:20 UTC), deliberately: base images
drift under this repo (dracut, systemd, bootc), and a broken live boot should
page here rather than a downstream.

Two structural notes:

- **There is no `required-checks` aggregator job.** Most tuna-os repos have
  one (`if: always()` + `test "${{ needs.X.result }}" = success`) so a
  skipped or cancelled job reports failure instead of an absent check. If
  branch protection or agent auto-merge is ever pointed at this repo, that
  gap needs closing first.
- **`iso-smoke` runs on a self-hosted RunsOn runner**
  (`runs-on=…/runner=build-amd64`), not a GitHub runner, because the GitHub
  image's podman wedges the customize commit (tunaOS#1893). A fork without
  that runner configured cannot run this job at all.

The `concurrency` group is load-bearing, not tidiness: without it, 26 stale
runs piled up on 2026-07-20 (mostly Renovate rebases) and saturated the shared
macOS/Windows pool for hours.

## Podman is the recurring source of CI breakage

Three separate incidents are pinned open in `ci.yml`, and all three look like
tacklebox bugs from the log:

- **podman ≥ 5 + crun ≤ 1.14**: podman stamps `ociVersion: 1.2.0`, crun
  rejects anything that is not `1.0.x`, every `podman run` dies with
  "unknown version specified" (exit 126). Worked around by pointing podman at
  `runc` for exactly that pairing.
- **podman 5.8.4**: the customize commit wedges and `podman build` fails with
  "cannot re-exec process". The ISO job installs podman 6.x from Kubic
  unstable and removes the image's non-apt podman shadow from
  `/usr/local/bin` so the pinned one wins on `PATH`.
- **conmon 2.1.10 without journald**: dies on the customize run. Forced to the
  `k8s-file` log driver.

Before assuming a red `iso-smoke` is your change, check `podman --version` /
`crun --version` in the log against those notes.

## Checks

```bash
just build && just test          # go build -o tacklebox ./cmd/tacklebox; go test ./...
go vet ./...
go test -tags=integration -run='Help' ./...
for r in examples/*.json fixtures/*.json; do python3 -m json.tool "$r" >/dev/null; done
```

> **Nothing checks formatting or lints.** `.golangci.yml` is present but no
> workflow runs `golangci-lint`, and neither CI nor the justfile runs `gofmt`.
> `gofmt -l ./cmd ./internal` currently reports three files
> (`internal/install/live_orchestration_test.go`, `internal/install/remora.go`,
> `internal/purefs/uki_test.go`). Reformatting them is a separate, mechanical
> change — do not sweep it into an unrelated PR.

Two tests are environment-gated rather than skipped, so a local `go test ./...`
can be red for reasons that are not your change: `purefs`
`TestIso9660ExternalTools` loop-mounts the built ISO when EUID is 0 (it needs
real mount privileges — and note it never runs on CI, where the runner user is
not root), and the `internal/install` SUDO_USER tests depend on the root
context described above.

## Don'ts

- **Don't commit built binaries.** `go build ./cmd/<name>` drops output at the
  repo root; a 10 MB `purebuild` binary landed in `d860815` and had to be
  removed in `b0f18a1`, and the blob is still in every clone's history.
  `/purebuild`, `/tacklebox` and `/tbwasm` are ignored for that reason.
- **Don't hand-prepare initramfs images.** Tacklebox probes each image and
  rebuilds with the image's own dracut inside a privileged container,
  injecting its embedded `tbox-root` / `tbox-live` modules — that is what
  makes live ISOs work from any distro's bootc image without
  `dracut-live`-style packages. Set `skip_initramfs_rebuild` per environment
  only when the image genuinely already ships the modules.
- **Don't delete the build caches to "clean up" CI.** `initramfs-cache/` and
  `squashfs-cache/` are keyed by image ID, so a stale restore is harmless
  (changed images simply miss) and a warm cache saves ~2-3 min per env.
- **Don't relax the ISO size assertions** in `iso-smoke` — the dedup and
  delta bounds are deliberately loose so they catch "dedup silently did
  nothing" without being flaky about compression noise. A failure there means
  the layout regressed, not that the bound is wrong.
