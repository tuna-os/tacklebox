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

## CI runs through the justfile, and cleans up after itself

Workflow steps with real logic call `just ci-*` recipes (`just --list`); the
YAML keeps only thin wrappers (checkout, setup, upload, publish). Two sets of
recipes keep the runners and the Actions queue from filling up:

- **`just ci-cleanup`** — the last step of every job that creates containers,
  images, loop devices, QEMU or large scratch (`if: always()`). It removes
  only what *that job* created, proven by ownership rather than guessed:
  directories carry a `.ci-owner` marker written by `just ci-claim` /
  `ci-prepare-out`; podman images and containers carry the
  `io.tunaos.tacklebox.ci-owner=$CI_OWNER` label (`ci-fixture-images`) or sit
  in the pulled-images ledger (`ci-pull-images`); loop devices and mounts are
  owned by living under a claimed directory. It refuses (exit 1) a directory
  claimed by another owner. It never runs `podman system prune`, and it keeps
  `initramfs-cache/` and `squashfs-cache/` because `actions/cache` saves them
  in its post step.
- **`just ci-reap-stale`** — run daily by `ci-reaper.yml`: cancels runs queued
  or in progress longer than `STALE_QUEUED_HOURS` (6), deletes completed runs
  and expired artifacts older than `KEEP_RUN_DAYS` (14). It never touches the
  newest run of a workflow+branch, a default-branch push younger than the
  bound, or itself. `DRY_RUN=1` prints only; manual dispatch defaults to it.

Both are tested in `internal/cirecipes` against fake `podman`/`losetup`/
`umount`/`gh` binaries, so `go test ./...` covers them without privileges.
`just ci-claim` refuses a non-empty unmarked directory: if a job's output base
already holds content it did not create, cleanup could not prove it owned it.

Deliberately not routed through the justfile: the R2 upload steps
(`poc-artifacts`, `pure-iso`, `bazzite-*`) and `release*.yml` publishing/
signing/attestation (behaviour cannot be exercised locally), the PowerShell
steps of `cross-platform-smoke` (no `just` on the Windows leg), and the
`apt-get install` steps (a few lines, different per job).

## Checks

```bash
just check                       # go vet, go test, every example/fixture recipe parses
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
