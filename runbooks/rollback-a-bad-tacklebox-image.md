# Runbook: Rollback a Bad `ghcr.io/tuna-os/tacklebox` Image

## Overview

`.github/workflows/release-image.yml` builds and pushes the tacklebox
container image on every push to `main` that touches `Containerfile`,
`cmd/**`, `internal/**`, `src/**`, `go.mod`, or `go.sum` — and on every
`v*` tag. The job is **not gated** on `ci.yml`'s `lint-test` job (compare
`ci.yml`'s `needs: lint-test` jobs with `release-image.yml`, which has no
`needs:` at all and no `workflow_run` trigger) and it has no build- or
boot-verification step of its own. A push that compiles but is
functionally broken — a bad recipe default, a bootc/mkfs regression, a
dracut overlay bug — is published as `:latest` the moment the image build
finishes.

This matters beyond this repo: `tuna-os/tunaOS`'s
`scripts/lib/tacklebox.sh` defaults every ISO build to
`TACKLEBOX_IMAGE:-ghcr.io/tuna-os/tacklebox:latest` (see
`build_tacklebox_image_ref`), so a bad `:latest` here silently breaks the
org's primary ISO build pipeline on its very next run, with no version
pin protecting it by default.

## What Rollback Can and Cannot Do

- **Can**: repoint `:latest` (and any consumer that reads it) at a
  previously published image. Every successful `release-image.yml` run
  on `main` also tags the same digest `sha-<short-sha>` in addition to
  `latest` (confirmed via
  `gh api orgs/tuna-os/packages/container/tacklebox/versions` — recent
  versions carry both `['sha-<short-sha>', 'latest']` on the same
  digest). Untagged predecessor digests are not pruned by any workflow in
  this repo, so the last-known-good `sha-` tag reliably survives as a
  rollback anchor — the same mechanism `tuna-os/tunaOS` documents for its
  own image promotions.
- **Cannot**: stop a host or CI job that already pulled the bad `:latest`
  digest before rollback — this is a forward-pointer fix, not a
  revocation. Any build that already ran against the bad image needs a
  manual re-run after `:latest` is repointed.

## Rollback Steps

1. **Confirm the bad digest and find the last-known-good `sha-` tag:**
   ```bash
   gh api orgs/tuna-os/packages/container/tacklebox/versions --paginate \
     | jq -r '.[] | select(.metadata.container.tags | length > 0) |
              "\(.updated_at) \(.metadata.container.tags)"' | head -20
   ```
   Identify the digest currently tagged `latest`, then the digest tagged
   `sha-<short-sha>` from the last commit known to build a working image
   (cross-check against `git log --oneline` on `main` and, if the
   suspect commit is known, `ci.yml`'s run history for that SHA).

2. **Immediate stopgap for the ISO pipeline** — pin `tunaOS` builds off
   `:latest` without waiting on a registry retag:
   ```bash
   export TACKLEBOX_IMAGE="ghcr.io/tuna-os/tacklebox:sha-<good-short-sha>"
   ```
   (`scripts/lib/tacklebox.sh` reads this env var before falling back to
   `:latest` — no code change needed.)

3. **Repoint `:latest` at the good digest** so every consumer that has
   *not* set an explicit pin also recovers. This requires registry write
   access this bot does not have (`gh auth token` is blocked for this
   agent and there is no `skopeo`/`crane` credential here) — a
   maintainer with GHCR push rights runs:
   ```bash
   skopeo copy \
     docker://ghcr.io/tuna-os/tacklebox:sha-<good-short-sha> \
     docker://ghcr.io/tuna-os/tacklebox:latest
   ```
   or re-triggers `release-image.yml` via `workflow_dispatch` from the
   last-known-good commit (the workflow always re-tags `:latest` on the
   checked-out commit — see the `Resolve image tags` step).

4. **Revert or fix forward** on `main` so the next automatic push doesn't
   immediately re-publish the same bad `:latest`.

5. **Verify**: re-run `tunaOS`'s ISO build against the repointed
   `:latest` (or confirm the `TACKLEBOX_IMAGE` pin from step 2 is still
   in place) and confirm a successful boot before removing the stopgap
   pin.

## Longer-Term Fix (not applied here — requires a `workflows`-scoped push)

Gate `release-image.yml` on `ci.yml`'s `lint-test` job so a push that
fails lint/tests can't reach `:latest` at all:

```diff
--- a/.github/workflows/release-image.yml
+++ b/.github/workflows/release-image.yml
@@
-jobs:
-  build-and-push:
-    runs-on: ubuntu-24.04
+jobs:
+  wait-for-ci:
+    runs-on: ubuntu-24.04
+    steps:
+      - name: Wait for lint-test to succeed on this SHA
+        uses: lewagon/wait-on-check-action@v1
+        with:
+          ref: ${{ github.sha }}
+          check-name: lint-test
+          repo-token: ${{ secrets.GITHUB_TOKEN }}
+          wait-interval: 15
+  build-and-push:
+    needs: wait-for-ci
+    runs-on: ubuntu-24.04
```

This does not close the gap on its own — `lint-test` compiles and unit
tests the code, it does not boot the produced image — but it stops the
clearly-broken (non-compiling, failing-test) case from ever reaching
`:latest`, matching the bar `ci.yml` already enforces for PRs.
