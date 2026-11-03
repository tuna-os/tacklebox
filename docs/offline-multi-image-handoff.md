# Offline installer handoff from multi-image media

Evaluation for [#421](https://github.com/tuna-os/tacklebox/issues/421).
The existing recipe can describe the experiment without a schema change.
An environment's live root and its installable image are separate payloads:
deduplicating the live roots does not make them an OCI image store.

**Status: source evaluation complete; runtime compatibility unverified.**
No media size, successful installation, or installed digest is claimed here.
The evaluation host had Go but no Podman, skopeo, squashfs tools, or QEMU.
The runtime steps below remain acceptance work for #421.

## Inspected implementations and existing coverage

The Tacklebox baseline is `34a04b9`; Fisherman was inspected at
[`6fffcaa`](https://github.com/tuna-os/fisherman/tree/6fffcaa0bdf5fbca6addc5882c035e07ed6b18fc).
The issue's Fisherman `internal/recipe/source.go` link concerns recipe input
(file/stdin), not image transport. The relevant image contracts now live in
`internal/recipe/recipe.go`, `cmd/fisherman/main.go`, and
`internal/install/{bootc,mediainput}.go` under `fisherman/`.

| Boundary | Existing behavior | Limit of the evidence |
| --- | --- | --- |
| `cmd/tacklebox/recipe_gen.go` | Multiple environments default to shared-store dedup; full recipe fields pass through. | It does not infer `offline_payloads` or bind an environment ID to an installer selection. |
| `cmd/tacklebox/build.go`, `build_install.go` | Live roots use per-environment, combined, or delta squashfs; offline payloads are assembled separately. | A boot menu selection alone does not choose Fisherman's source. |
| `internal/install/offline_store.go` | `OfflinePayload.Source` is copied from the builder store and exposed under `Ref`; overlay layers can be shared. Digest destinations use a tag alias followed by a digest-resolution check. | Retagging and successful lookup alone do not prove that the cached image matches an expected selected digest. |
| Fisherman `CheckImage` and CLI preflight | An explicit `containers-storage:` source is inspected locally; a missing or malformed source is refused before partitioning. `targetImgref` is separate from `image`. | A mutable tag that points at a different valid image is still a valid local source. Selection must bind identity before this check. |
| Fisherman `resolveMediaInput` | A required host media receipt binds source, target, platform, and read-only OCI content. | A single-image required receipt cannot be repurposed as a multi-image selector. Preserve its refusal of a different source. |
| `fixtures/iso-e2e.json` | Two related images, combined live dedup, both offline payloads, live SSH customization. | `scripts/test-install-e2e.sh` hardcodes alpha, calls bootc directly, permits guest networking, and checks login rather than digest identity. It is not the #421 acceptance test. |

Searches covered the recipe generator and tests, all offline-payload callers,
live install strategies, pure-Go OCI media generation, and the install script's
callers. The pure-Go `internal/purefs` OCI-layout producer is a separate path;
do not replace it with the CLI's containers-storage handoff.

Related issues establish useful baselines:
[#40](https://github.com/tuna-os/tacklebox/issues/40) covers larger live dedup
sets, [#92](https://github.com/tuna-os/tacklebox/issues/92) VFS embedding,
[#93](https://github.com/tuna-os/tacklebox/issues/93) storage-driver mismatch,
and [#98](https://github.com/tuna-os/tacklebox/issues/98) the bootc install E2E.
None of those tests establishes selected-image Fisherman installation.

The pinned [Firn reference](https://github.com/frostyard/firn/blob/9968f2b32e5aa28d8b168cf1b903d83a30e57759/internal/bootcimg/verify.go)
binds labels to its selected source and prefers an available local image.
It also performs a remote inspection and may require remote cosign verification.
That is useful identity handling to compare, but it is not proof of a completely
offline path and is not adopted here.

## Selected identity and ownership

For each environment, retain a build-time record of its ID, platform, original
image reference, platform manifest digest, local runtime image ID, offline
payload `source`/`ref`, and intended day-2 registry reference. Do not confuse a
multi-architecture index digest, platform manifest digest, config/image ID,
or ostree commit checksum. Keep the raw manifest used to establish each digest.

Use the existing Fisherman fields: `image` is an explicit local transport and
`targetImgref` is the selected image's update reference. For the overlay case,
resolve and compare the selected digest in the live store, then use its
immutable runtime ID as `containers-storage:<id>` for the install. Keep metadata
from that same image. Refuse an absent image, a digest mismatch, or an unknown
environment before invoking the destructive installer. Do not fall back to a
tag or to the running live environment. A digest establishes identity; it does
not replace signature verification required by an image's trust policy.

The builder owns its staging graphroot and runroot. The media owns the packed
store and live squashfs files. The live session mounts media content read-only
and owns a separate writable graphroot/runroot; it must not write Podman state
into `/var/lib/superiso-store`. Fisherman owns install scratch and the single
disposable target. Never pass the media disk as `disk`.

For overlay payloads, mount `LiveOS/store.squashfs.img` read-only at
`/var/lib/superiso-store`, configure the live consumer's primary driver as
`overlay`, and register that path as an additional image store. Supplying
Fisherman's `additionalImageStores` alone does not configure the host skopeo
preflight. Prove visibility with host skopeo as well as with the install process.
An explicit `CONTAINERS_STORAGE_CONF` takes precedence over Fisherman's store
configuration; record it with the evidence.

Do not claim composefs dedup support from the overlay result. The all-composefs
branch builds a squashed VFS store and sets `SetVFSStorePath`, but embedding is
implemented by `InstallLiveWithStore`, reached via per-environment `InstallLive`.
Combined assembly bypasses that embedding hook. Delta assembly uses `InstallLive`
for the base, but builds other environments as diffs against the source images;
the inherited VFS store and its visibility need a separate boot test. This source
audit leaves the combined/delta VFS handoff unproven. VFS squashing also changes
image identity; record the resulting manifest rather than claiming the original
digest survived. Test these paths separately before listing them as supported.

## Disposable runtime evaluation

Use a Linux media-build host with the tools in [the requirements](../README.md#-requirements),
OVMF, QEMU, and a recorded Fisherman binary plus its required guest tools.
Use only newly created VM disk files. Keep the default single-image recipes
and current CI size bounds unchanged.

1. Build alpha and beta with `fixtures/iso-smoke.Containerfile`, using `MARKER=alpha`
   and `MARKER=beta`. Record the resolved base digest, both raw manifests, image
   IDs, platform, and shared layer digests. Use the same root/rootless builder
   context for fixture creation and Tacklebox assembly.
2. Start from `fixtures/iso-e2e.json`. Stage Fisherman and its required tools
   through live customization for both environments; retain the original
   `offline_payloads` as the install sources. Live SSH/tool customization must
   not become the installed image. Resolve customization paths relative to the
   recipe, or make them absolute when generating recipes elsewhere.
3. Build two otherwise identical recipes: one with `shared_store.dedup=false`,
   one with `dedup=true` and `dedup_layout="combined"`. Keep both offline
   payloads in both builds. Run `tacklebox verify` on each ISO. Retain build
   logs, recipe JSON, tool versions, source commits, and ISO hashes.
4. Record actual ISO byte counts with `stat -c%s`. Extract `LiveOS` from each
   ISO with xorriso and record each squashfs byte count. Report separately:
   live-root saving (sum of per-env squashfs minus combined squashfs), offline
   store size, and total ISO saving. Compute percentages against the measured
   baseline. Recipe `size` and store-usage warnings are estimates, not measured
   savings. Offline-store layer sharing is separate from live file dedup.
5. Boot the combined media with alpha selected and a fresh blank target.
   Disable guest network access for the entire test. A serial console with no
   NIC works; an SSH harness can use QEMU SLIRP `restrict=on` plus host port
   forwarding. Retain the QEMU argv and prove registry/DNS access fails while
   control access works. Do not reuse the unrestricted E2E script as proof.
6. In the guest, verify the read-only store mount, inspect the selected payload
   through the configured live storage, compare its digest with the build
   record, and bind its runtime ID. Pass that explicit local source to
   Fisherman with the existing recipe fields, for example:

   ```json
   {
     "disk": "/dev/vda",
     "filesystem": "ext4",
     "hostname": "offline-alpha",
     "image": "containers-storage:<verified-runtime-image-id>",
     "targetImgref": "<selected-image-registry-reference>",
     "additionalImageStores": ["/var/lib/superiso-store"],
     "encryption": {"type": "none"}
   }
   ```

   This is a template for a disposable guest, not a host install command.
   Inspect any host media receipt first: a required receipt for another source
   must fail, and must not be removed to make the experiment pass.
7. Detach the ISO and boot only the installed target with networking disabled.
   Capture `bootc status --json`, deployment identity, the installed
   `/usr/share/tbox-env-marker`, and the update reference. Require both the
   selected digest (interpreted using the recorded manifest/ostree identities)
   and the alpha marker; a login prompt alone is insufficient.
8. Repeat selection, install, and installed boot for beta on a separate fresh
   target. Do not count two installations of alpha as the two-image test.
9. On additional disposable targets, test a missing source and beta substituted
   for alpha's expected identity. The selection/preflight boundary must reject
   both before disk writes. Capture target hashes before and after rejection,
   exit status, source diagnostics, and process logs showing no partition or
   filesystem command. Also test a malformed local manifest. A valid but wrong
   tag must fail the identity comparison even if Fisherman's local check passes.
10. Repeat the existing single-image media build/install control. Record
    failures separately for visibility, selection, preflight, installation,
    and installed boot. Mark a backend supported only after all gates pass.

## Verification record

Run the complete affected modules, not just tests whose names match offline:

```bash
# Tacklebox
go test ./cmd/tacklebox ./internal/recipe ./internal/install
# From tuna-os/fisherman/fisherman
go test ./internal/recipe ./internal/install ./cmd/fisherman
```

Fisherman's `offline_local_source_test.go` and `offline_preflight_test.go`
exercise local transport and refusal with controlled process boundaries.
Both complete module commands above passed on the evaluation host. Go's build
cache was placed in writable `/tmp`; Fisherman's storage-driver test required
an unsandboxed run to create its `/var/tmp` fixture. These tests do not prove a
real store mount or OS installation. Keep their results separate from the
runtime matrix:

| Case | Measured bytes | Offline Fisherman install | Installed digest/marker | Refusal before writes |
| --- | --- | --- | --- | --- |
| Alpha, combined overlay | Not run | Not run | Not run | Not run |
| Beta, combined overlay | Not run | Not run | Not run | Not run |
| Single-image control | Not run | Not run | Not run | Not run |
| Composefs combined/delta | Not run | Not run | Not run | Not run |

Keep #421 open until measured space savings, both selected-image installations,
installed digest evidence, and refusal evidence are attached. No Frostyard
change, multi-root install, or new dedup API follows from this source audit.
