# Tacklebox task runner

output_base := "/var/mnt/data/tacklebox-build"
image_path := output_base / "tacklebox.img"
recipe_path := "examples/multi-test.json"

# Build the tacklebox binary
build:
    go build -o tacklebox ./cmd/tacklebox

# Run unit tests
test:
    go test ./...

# Provision a real disk (CAUTION: wipes the disk!)
provision-usb device="/dev/sda" recipe=recipe_path: build
    sudo ./tacklebox build {{recipe}} {{device}} -b {{output_base}}-usb

# Refresh an existing USB/image without wiping the PERSIST partition
update device="/dev/sda" recipe=recipe_path: build
    sudo ./tacklebox update {{recipe}} {{device}} -b {{output_base}}-usb

# Verify the generated image (partitions and boot entries)
verify-test image=image_path:
    #!/usr/bin/env bash
    set -euo pipefail
    echo ">>> Verifying image: {{image}}"
    LOOP_DEV=$(sudo losetup --find --show --partscan {{image}})
    trap 'sudo umount {{output_base}}/mount-esp 2>/dev/null || true; sudo losetup -d "$LOOP_DEV"' EXIT
    echo ">>> Mounted at $LOOP_DEV"
    sudo mount "${LOOP_DEV}p1" {{output_base}}/mount-esp
    ls -R {{output_base}}/mount-esp/loader/entries
    cat {{output_base}}/mount-esp/loader/entries/*.conf

# Boot the generated image in QEMU (nographic)
boot image=image_path:
    sudo qemu-system-x86_64 \
        -m 4G -accel kvm -cpu host \
        -drive if=pflash,format=raw,readonly=on,file=/usr/share/edk2/ovmf/OVMF_CODE.fd \
        -drive file={{image}},format=raw,if=virtio \
        -net none -display none -serial stdio

# Build and compress a multi-boot image for redistribution
build-xz recipe=recipe_path: build
    sudo ./tacklebox build {{recipe}} --xz -b {{output_base}}-dist

# Clean up build artifacts
clean:
    sudo rm -rf {{output_base}}*
    rm -f tacklebox

# ═══ CI ══════════════════════════════════════════════════════════════════════
# Recipes the GitHub workflows call. Run them locally with the same names.
#
# Ownership model (scripts/ci-lib.sh): a job removes ONLY what it created.
# Directories are claimed with a marker (`ci-claim`), podman objects carry the
# CI_OWNER label (`ci-fixture-images`) or sit in the pulled-images ledger
# (`ci-pull-images`), loop devices/mounts are owned by living under a claimed
# directory. `ci-cleanup` honours exactly those and nothing else.

# The job's tacklebox output base; every ci-* recipe and ci-cleanup agree on it.
export TBX_CI_OUT := env("TBX_CI_OUT", "/mnt/tbx")
# Job-scoped owner id: labels podman objects, marks claimed directories.
export CI_OWNER := env("GITHUB_RUN_ID", "local") + "." + env("GITHUB_RUN_ATTEMPT", "1") + "." + env("GITHUB_JOB", "job")

# Claim directories for this job (create them user-owned, write the marker)
ci-claim *dirs:
    scripts/ci-claim.sh {{dirs}}

# Claim the job's output base
ci-prepare-out:
    scripts/ci-claim.sh "{{TBX_CI_OUT}}"

# Always-run, job-scoped teardown: QEMU, labelled podman objects, loop devices
# and mounts under the claimed directories, scratch contents (caches kept)

[doc("Always-run, job-scoped teardown: QEMU, labelled podman objects, loop devices")]
ci-cleanup *scratch_dirs:
    scripts/ci-cleanup.sh {{scratch_dirs}}

# Cancel stale queued runs, delete old runs and expired artifacts
# (STALE_QUEUED_HOURS=6 KEEP_RUN_DAYS=14 DRY_RUN=1 to preview)

[doc("Cancel stale queued runs, delete old runs and expired artifacts")]
ci-reap-stale:
    scripts/ci-reap-stale.sh

# Local gate: what the lint-test job runs
check: vet test check-recipes

# go vet
vet:
    go vet ./...

# go test with a coverage summary
test-cover:
    go test -cover -covermode=set ./...

# CLI help surface (integration tag, help-only)
test-integration-help:
    go test -tags=integration -run='Help' ./...

# Loop-device unmount smoke (needs passwordless sudo, sgdisk, mkfs.vfat)
test-unmount-smoke:
    go test -tags=integration -v -run=TestUnmountDevice_LoopSmoke ./internal/blockdev/...

# Native darwin tests of pkg/blockdev/darwin
test-darwin:
    go test -v ./pkg/blockdev/darwin/...

# Compile everything
build-all:
    go build ./...

# Browser WASM compile and API contract
wasm:
    #!/usr/bin/env bash
    set -e
    GOOS=js GOARCH=wasm go build -o /tmp/tbwasm.wasm ./cmd/tbwasm
    GOOS=js GOARCH=wasm go test -timeout=3m -exec="$(go env GOROOT)/lib/wasm/go_js_wasm_exec" ./cmd/tbwasm

# Every example/fixture recipe must be valid JSON
check-recipes: build
    #!/usr/bin/env bash
    fail=0
    for r in examples/*.json fixtures/*.json; do
      [ -f "$r" ] || continue
      if ! python3 -c "import json,sys; json.load(open('$r'))"; then
        echo "::error file=$r::JSON parse failed"
        fail=1
      fi
    done
    exit $fail

# Point podman at runc when podman >= 5 would stamp an OCI version crun <= 1.14
# rejects (ubuntu24/20260726: podman 5.8.4 + crun 1.14.1, "unknown version
# specified", exit 126). crun 1.15 dropped the check; a no-op elsewhere.

[doc("Point podman at runc when podman >= 5 would stamp an OCI version crun <= 1.14")]
ci-podman-runtime:
    #!/usr/bin/env bash
    set -e
    podman_ver=$(podman --version | awk '{print $3}')
    crun_ver=$(crun --version 2>/dev/null | awk 'NR==1{print $3}')
    echo ">>> podman=$podman_ver crun=${crun_ver:-absent}"
    if dpkg --compare-versions "$podman_ver" ge 5 &&
       [ -n "$crun_ver" ] && dpkg --compare-versions "$crun_ver" lt 1.15; then
      command -v runc >/dev/null || {
        echo "::error::crun $crun_ver cannot run podman $podman_ver's spec and there is no runc to fall back to"
        exit 1
      }
      sudo install -d /etc/containers/containers.conf.d
      printf '[engine]\nruntime = "runc"\n' |
        sudo tee /etc/containers/containers.conf.d/10-tbox-runc.conf
      podman info --format '>>> OCI runtime: {{{{.Host.OCIRuntime.Name}} {{{{.Host.OCIRuntime.Version}}'
    fi

# Install the Kubic unstable podman (6.x) and drop the image's podman 5.8.4
# shadow from /usr/local/bin. 5.8.4 wedges the customize commit and fails
# `podman build` with "cannot re-exec process"; the k8s-file log driver keeps
# conmon 2.1.10 (no journald) alive.

[doc("Install the Kubic unstable podman (6.x) and drop the image's podman 5.8.4")]
ci-podman-kubic:
    #!/usr/bin/env bash
    set -euo pipefail
    sudo install -d /etc/apt/keyrings
    curl -fsSL https://download.opensuse.org/repositories/devel:/kubic:/libcontainers:/unstable/xUbuntu_24.04/Release.key \
      | gpg --dearmor | sudo tee /etc/apt/keyrings/kubic.gpg >/dev/null
    echo 'deb [signed-by=/etc/apt/keyrings/kubic.gpg] https://download.opensuse.org/repositories/devel:/kubic:/libcontainers:/unstable/xUbuntu_24.04/ /' \
      | sudo tee /etc/apt/sources.list.d/kubic.list
    sudo apt-get update -qq
    # Force the Kubic stack: apt kept the image's 4.9.3+conmon2.1.10
    # (conmon journald failure). Remove the old podman/conmon so the
    # Kubic podman 6.x + matching conmon/crun come in fresh.
    sudo apt-get remove -y podman conmon crun || true
    sudo apt-get install -y podman
    # conmon 2.1.10 on this image lacks journald support and dies on the
    # customize run ("[conmon:e]: Include journald in compilation path").
    # Force a file log driver so conmon never touches journald.
    sudo install -d /etc/containers/containers.conf.d
    printf '[engine]\nlog_driver = "k8s-file"\n' | \
      sudo tee /etc/containers/containers.conf.d/10-tbox-log.conf
    for p in /usr/local/bin/podman /usr/local/sbin/podman; do
      if [ -e "$p" ] && ! dpkg -S "$p" >/dev/null 2>&1; then
        echo ">>> removing non-apt podman shadow: $p"
        sudo rm -f "$p"
      fi
    done
    hash -r
    echo ">>> podman now: $(podman --version) / conmon $(conmon --version 2>/dev/null | head -1 || echo none)"

# Pull images into root's podman store with retries (Quay's CDN drops blobs
# mid-transfer). Images that were absent before are recorded in the ledger so
# ci-cleanup removes exactly those. attempts=3 par=1 are the defaults.

[doc("Pull images into root's podman store with retries (Quay's CDN drops blobs")]
ci-pull-images +images:
    scripts/ci-pull.sh {{images}}

# Pull every image a recipe boots, in parallel, no retry (bazzite workflows)
ci-pull-recipe-images recipe par="6":
    jq -r '.bootable_environments[].image' {{recipe}} | PULL_ATTEMPTS=1 PULL_PARALLEL={{par}} xargs -r scripts/ci-pull.sh

# Build the fixture live images, labelled with CI_OWNER so ci-cleanup can find
# them. podman_cmd picks the store: "sudo podman" (root) or "podman" (user).

[doc("Build the fixture live images, labelled with CI_OWNER so ci-cleanup can find")]
ci-fixture-images podman_cmd +markers:
    #!/usr/bin/env bash
    set -e
    for m in {{markers}}; do
      {{podman_cmd}} build -f fixtures/iso-smoke.Containerfile \
        --label "io.tunaos.tacklebox.ci-owner=$CI_OWNER" \
        --build-arg MARKER="$m" -t "localhost/tbox-iso-$m:latest" fixtures
    done

# ── verify-smoke: 2-env block image ──────────────────────────────────────────

# Build the 2-env block image
ci-smoke-build: ci-prepare-out
    sudo ./tacklebox build fixtures/smoke-2env.json -b {{TBX_CI_OUT}}

# tacklebox verify on the block image
ci-smoke-verify:
    sudo ./tacklebox verify {{TBX_CI_OUT}}/tacklebox.img

# tacklebox status on the block image
ci-smoke-status:
    sudo ./tacklebox status {{TBX_CI_OUT}}/tacklebox.img

# Re-install both envs without reformatting. Shares -b with the build on
# purpose: the update must hit the initramfs cache the build populated.

[doc("Re-install both envs without reformatting. Shares -b with the build on")]
ci-smoke-update:
    sudo ./tacklebox update --yes fixtures/smoke-2env.json {{TBX_CI_OUT}}/tacklebox.img -b {{TBX_CI_OUT}}

# Add the 3rd env
ci-smoke-add:
    sudo ./tacklebox add fixtures/smoke-add.json {{TBX_CI_OUT}}/tacklebox.img --yes -b {{TBX_CI_OUT}}

# Remove the added env
ci-smoke-remove:
    sudo ./tacklebox remove centos-add {{TBX_CI_OUT}}/tacklebox.img --yes -b {{TBX_CI_OUT}}

# Boot the block image under QEMU (TCG on free runners, ~3 min to login)
ci-smoke-boot:
    sudo ./scripts/test-boot.sh {{TBX_CI_OUT}}/tacklebox.img 600

# Step summary for verify-smoke
ci-summary-smoke:
    #!/usr/bin/env bash
    echo '## tacklebox build artefacts' >> "$GITHUB_STEP_SUMMARY"
    ls -lh {{TBX_CI_OUT}}/ 2>/dev/null | awk 'NR>1 {print "- `" $NF "` (" $5 ")"}' >> "$GITHUB_STEP_SUMMARY"
    df -h / /mnt 2>/dev/null >> "$GITHUB_STEP_SUMMARY"
    if [ -f qemu-boot.log ]; then
      echo '### QEMU Boot Log (tail)' >> "$GITHUB_STEP_SUMMARY"
      echo '```' >> "$GITHUB_STEP_SUMMARY"
      tail -n 50 qemu-boot.log >> "$GITHUB_STEP_SUMMARY"
      echo '```' >> "$GITHUB_STEP_SUMMARY"
    fi

# ── iso-smoke: per-env, dedup, 6-env dedup, delta, offline-store ISOs ────────

# Build one fixture ISO: recipe is a fixtures/<recipe>.json stem
ci-iso-build recipe iso: ci-prepare-out
    sudo ./tacklebox build fixtures/{{recipe}}.json --iso {{TBX_CI_OUT}}/{{iso}}.iso -b {{TBX_CI_OUT}}

# tacklebox verify on one ISO
ci-iso-verify iso:
    ./tacklebox verify {{TBX_CI_OUT}}/{{iso}}.iso

# QEMU/OVMF live boot gate on the per-env ISO
ci-iso-live-boot:
    sudo ./scripts/test-live-boot.sh {{TBX_CI_OUT}}/smoke.iso 900

# alpha carries live_customize (fixtures/live-customize-smoke.sh); beta does
# not. The marker must be inside alpha's squashed rootfs and absent from beta's.

[doc("alpha carries live_customize (fixtures/live-customize-smoke.sh); beta does")]
ci-iso-assert-live-customize:
    #!/usr/bin/env bash
    set -e
    work=$(mktemp -d)
    trap 'rm -rf "$work"' EXIT
    xorriso -osirrox on -indev {{TBX_CI_OUT}}/smoke.iso \
      -extract /LiveOS/alpha.rootfs.sfs "$work/alpha.sfs" \
      -extract /LiveOS/beta.rootfs.sfs "$work/beta.sfs"
    unsquashfs -l "$work/alpha.sfs" 'usr/share/tbox-live-customize-marker' \
      | grep -q tbox-live-customize-marker \
      || { echo "::error::alpha rootfs missing live_customize marker"; exit 1; }
    if unsquashfs -l "$work/beta.sfs" 'usr/share/tbox-live-customize-marker' 2>/dev/null \
      | grep -q tbox-live-customize-marker; then
      echo "::error::beta (no live_customize) unexpectedly contains the marker"
      exit 1
    fi
    echo "live_customize marker present in alpha, absent in beta"

# The fixture envs differ by one marker file, so the combined squashfs should
# be roughly half the per-env size. The 90% bound is deliberately loose.

[doc("The fixture envs differ by one marker file, so the combined squashfs should")]
ci-iso-assert-dedup-shrinks:
    #!/usr/bin/env bash
    set -e
    per_env=$(stat -c%s {{TBX_CI_OUT}}/smoke.iso)
    dedup=$(stat -c%s {{TBX_CI_OUT}}/dedup.iso)
    echo "per-env ISO: ${per_env} bytes; dedup ISO: ${dedup} bytes"
    if [ "$dedup" -ge $((per_env * 9 / 10)) ]; then
      echo "::error::dedup ISO (${dedup}) is not meaningfully smaller than per-env ISO (${per_env})"
      exit 1
    fi

# 6 nearly-identical envs must be < 2x the 2-env dedup ISO
ci-iso-assert-6env-scales:
    #!/usr/bin/env bash
    set -e
    dedup2=$(stat -c%s {{TBX_CI_OUT}}/dedup.iso)
    dedup6=$(stat -c%s {{TBX_CI_OUT}}/dedup-6env.iso)
    echo "2-env dedup ISO: ${dedup2} bytes; 6-env dedup ISO: ${dedup6} bytes"
    if [ "$dedup6" -ge $((dedup2 * 2)) ]; then
      echo "::error::6-env ISO (${dedup6}) is too large — expected ≤ 2× 2-env (${dedup2})"
      exit 1
    fi
    echo "6-env/dedup2 ratio: $(echo "scale=2; $dedup6 / $dedup2" | bc)"

# dedup_layout=delta: the delta must be tiny and carry the divergent marker
ci-iso-assert-delta:
    #!/usr/bin/env bash
    set -e
    work=$(mktemp -d)
    trap 'rm -rf "$work"' EXIT
    xorriso -osirrox on -indev {{TBX_CI_OUT}}/delta.iso \
      -extract /LiveOS/beta.delta.sfs "$work/beta.delta.sfs"
    size=$(stat -c%s "$work/beta.delta.sfs")
    echo "beta.delta.sfs: ${size} bytes"
    # The fixtures differ by one marker file (plus initramfs-adjacent
    # noise); anything over 64 MiB means the diff degenerated into a
    # near-full copy.
    if [ "$size" -ge $((64 * 1024 * 1024)) ]; then
      echo "::error::beta.delta.sfs (${size}B) is far too large for a one-file diff"
      exit 1
    fi
    unsquashfs -l "$work/beta.delta.sfs" usr/share/tbox-env-marker \
      | grep -q tbox-env-marker \
      || { echo "::error::delta squashfs missing the env marker diff"; exit 1; }

# Boot all four ISOs in parallel to keep CI time under the 60-minute budget.
# Each QEMU VM gets 2G RAM; output is tee'd to the step log and a per-ISO file.

[doc("Boot all four ISOs in parallel to keep CI time under the 60-minute budget")]
ci-iso-boot-all:
    #!/usr/bin/env bash
    set -e
    fail=0

    # 6-env dedup ISO: gamma env with tbox-root pivot
    ( QEMU_LOG=qemu-iso-6env.log ./scripts/test-boot.sh {{TBX_CI_OUT}}/dedup-6env.iso 600 \
        "tacklebox.env=gamma" "Tacklebox: rebased OK" \
        2>&1 | tee -a qemu-iso-6env.out ) &
    pid1=$!

    # per-env ISO: beta env (non-dedup path)
    ( QEMU_LOG=qemu-iso-perenv.log ./scripts/test-boot.sh {{TBX_CI_OUT}}/smoke.iso 600 \
        "tacklebox.env=beta" \
        2>&1 | tee -a qemu-iso-perenv.out ) &
    pid2=$!

    # dedup ISO: alpha env with tbox-root pivot
    ( QEMU_LOG=qemu-iso-dedup.log ./scripts/test-boot.sh {{TBX_CI_OUT}}/dedup.iso 600 \
        "tacklebox.env=alpha" "Tacklebox: rebased OK" \
        2>&1 | tee -a qemu-iso-dedup.out ) &
    pid3=$!

    # delta ISO: beta env boots base + beta.delta.sfs lowerdir stack
    ( QEMU_LOG=qemu-iso-delta.log ./scripts/test-boot.sh {{TBX_CI_OUT}}/delta.iso 600 \
        "tacklebox.env=beta" "tacklebox.live.delta=beta.delta.sfs" \
        2>&1 | tee -a qemu-iso-delta.out ) &
    pid4=$!

    wait $pid1 || { echo "::error::6-env dedup ISO boot failed"; fail=1; }
    wait $pid2 || { echo "::error::per-env ISO boot failed"; fail=1; }
    wait $pid3 || { echo "::error::dedup ISO boot failed"; fail=1; }
    wait $pid4 || { echo "::error::delta ISO boot failed"; fail=1; }
    exit $fail

# Step summary for iso-smoke
ci-summary-iso-smoke:
    #!/usr/bin/env bash
    echo '## iso-smoke artefacts' >> "$GITHUB_STEP_SUMMARY"
    ls -lh {{TBX_CI_OUT}}/*.iso 2>/dev/null | awk '{print "- `" $NF "` (" $5 ")"}' >> "$GITHUB_STEP_SUMMARY"
    for log in qemu-iso-perenv.log qemu-iso-dedup.log qemu-iso-6env.log; do
      if [ -f "$log" ]; then
        echo "### $log (tail)" >> "$GITHUB_STEP_SUMMARY"
        echo '```' >> "$GITHUB_STEP_SUMMARY"
        tail -n 30 "$log" >> "$GITHUB_STEP_SUMMARY"
        echo '```' >> "$GITHUB_STEP_SUMMARY"
      fi
    done

# ── poc-artifacts / bazzite-*: ISO build, verify, checksum, summary ──────────

# Build an examples/<recipe>.json ISO into iso_dir (build base: TBX_CI_OUT)
ci-example-iso-build recipe iso_dir="/mnt/iso-out":
    sudo ./tacklebox build examples/{{recipe}}.json --iso {{iso_dir}}/{{recipe}}.iso -b {{TBX_CI_OUT}}

# tacklebox verify (as root) on iso_dir/<recipe>.iso
ci-example-iso-verify recipe iso_dir="/mnt/iso-out":
    sudo ./tacklebox verify {{iso_dir}}/{{recipe}}.iso

# Verify every ISO in dir (unprivileged)
ci-isos-verify dir:
    #!/usr/bin/env bash
    set -e
    for iso in {{dir}}/*.iso; do
      ./tacklebox verify "$iso"
    done

# Write dir/SHA256SUMS for the given glob (relative to dir) and print it
ci-isos-checksum dir pattern="./*.iso":
    #!/usr/bin/env bash
    set -e
    cd {{dir}}
    sha256sum {{pattern}} > SHA256SUMS
    cat SHA256SUMS

# Step summary: ISOs in dir plus SHA256SUMS
ci-summary-isos title dir:
    #!/usr/bin/env bash
    echo '## {{title}}' >> "$GITHUB_STEP_SUMMARY"
    ls -lh {{dir}}/*.iso 2>/dev/null | awk '{print "- `" $NF "` (" $5 ")"}' >> "$GITHUB_STEP_SUMMARY"
    if [ -f {{dir}}/SHA256SUMS ]; then
      echo '```' >> "$GITHUB_STEP_SUMMARY"
      cat {{dir}}/SHA256SUMS >> "$GITHUB_STEP_SUMMARY"
      echo '```' >> "$GITHUB_STEP_SUMMARY"
    fi

# ── pure-iso ─────────────────────────────────────────────────────────────────

# Which copy of the parse hook actually boots. Non-fatal evidence for
# tacklebox#166: prints the combined initrd's sections, hooks and the stock
# initramfs listing.

[doc("Which copy of the parse hook actually boots. Non-fatal evidence for")]
ci-pure-inspect-initrd workdir:
    #!/usr/bin/env bash
    img="{{workdir}}/initrd-tbox.img"
    [ -f "$img" ] || { echo "no combined initrd (built with --initrd?)"; exit 0; }
    # Print the whole listing, per section, with errors: a listing that failed
    # outright is otherwise indistinguishable from one that contained nothing.
    ls_out=$(mktemp) ls_err=$(mktemp) stock_out=$(mktemp)
    trap 'rm -f "$ls_out" "$ls_err" "$stock_out"' EXIT
    lsinitrd "$img" > "$ls_out" 2> "$ls_err" || true
    echo "### lsinitrd: $(wc -l < "$ls_out") lines, stderr:"
    cat "$ls_err"
    echo "### section headers"
    grep -n -i "image:\|early cpio\|^Version" "$ls_out" || true
    echo "### total entries vs dracut cmdline hooks seen"
    echo "entries: $(grep -c '^[-dl]' "$ls_out" || true)"
    grep -n "dracut/hooks/cmdline" "$ls_out" || echo "(no cmdline hooks listed at all — listing is incomplete)"
    echo "### every copy of the parse hook, in listing order"
    grep -n "parse-tbox-live" "$ls_out" || echo "(none)"
    echo "### stock initramfs from the image, listed on its own"
    stock=$(find "{{workdir}}/blobs" -type f -size +10M -size -400M 2>/dev/null | head -20)
    for f in $stock; do
      if lsinitrd "$f" > "$stock_out" 2>/dev/null && grep -q "dracut" "$stock_out"; then
        echo "--- $f ($(wc -l < "$stock_out") entries)"
        grep "parse-tbox-live\|hooks/cmdline" "$stock_out" || echo "  (image bakes no tbox hook)"
        break
      fi
    done

# Format-only ISO check: size floor and CD001 at sector 16
ci-pure-verify-iso iso:
    #!/usr/bin/env bash
    set -e
    ISO="{{iso}}"
    ls -l "$ISO"
    size=$(stat -c%s "$ISO")
    [ "$size" -gt 104857600 ] || { echo "::error::ISO is only ${size} bytes"; exit 1; }
    magic=$(dd if="$ISO" bs=2048 skip=16 count=1 status=none | dd bs=1 skip=1 count=5 status=none)
    [ "$magic" = "CD001" ] || { echo "::error::no CD001 at sector 16, got '${magic}'"; exit 1; }
    echo ">>> ${ISO} ${size} bytes, CD001 present"

# ── cross-platform-smoke (macOS) ─────────────────────────────────────────────

# hdiutil create + attach a virtual disk; the image is detached and removed on
# every exit path, including a failed diskutil assertion

[doc("hdiutil create + attach a virtual disk; the image is detached and removed on")]
ci-macos-disk-smoke:
    #!/usr/bin/env bash
    set -e
    dmg="${RUNNER_TEMP:-/tmp}/tboxci.dmg"
    dev=""
    cleanup() {
      if [ -n "$dev" ]; then hdiutil detach "$dev" >/dev/null 2>&1 || hdiutil detach -force "$dev" >/dev/null 2>&1 || true; fi
      rm -f "$dmg"
    }
    trap cleanup EXIT
    hdiutil create -size 64m -layout GPTSPUD -fs "MS-DOS FAT32" -volname TBOXCI "$dmg"
    attach_out=$(hdiutil attach -nomount "$dmg")
    echo "$attach_out"
    dev=$(echo "$attach_out" | head -1 | awk '{print $1}')
    echo "attached as $dev"
    diskutil info "$dev" | grep -E "Virtual|Protocol|Removable Media"
    hdiutil detach "$dev"
    dev=""
