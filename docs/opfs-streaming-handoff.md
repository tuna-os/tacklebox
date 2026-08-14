# OPFS streaming and the wasm32 4 GiB ceiling (#156)

Status: **cause found, fixed, measured.** The earlier revision of this doc
said "nobody has identified what holds ~4 GiB" and listed four suspects
inside `WriteErofs`. All four were wrong, and the instrumentation it asked
for is what proved it. Kept as a record so the dead ends stay dead.

---

## The answer

`GraftLiveOverlay` unpacked the published live-overlay delta into the
**heap**, not OPFS.

`introspect` passes the `opfsArena` directly to `c.Unpack(...)`, so base
layers stream to origin-private storage exactly as designed. It then wraps
that arena in a `hybridStore` whose `Put` was backed by an `oci.MemStore`
— justified by a comment reading "writes after the unpack (tree surgery
like EnsureLiveUser — small files) land in memory."

That assumption does not hold. `GraftLiveOverlay` calls `UnpackOnto` with
the *same* store, so every file body in the overlay delta went into linear
memory. For `flounder:xfce`:

| | |
|---|---|
| extra layers over base | 1 |
| compressed | 941 MB |
| unpacked | **2.49 GiB** |
| files | 42,177 |
| location | effectively all `var/lib/` (installer flatpak payload) |
| dropped by `SkipBodies` | **none** |

2.49 GiB of bodies plus the tree and inode tables is the whole of the
reported `4252303360 in use`, and is why the allocation that failed was a
routine 4 MiB `sliceChunk` read buffer.

### Why the suspect list was wrong

Metadata was never the problem. Instrumented, on the edition that OOMed:

```
phase=unpack   heap=52MB   objects=259686  opfs(layers=2350MB post=0MB)
phase=overlay  heap=112MB  objects=662360  opfs(layers=2350MB post=2550MB)
```

The complete `oci.Node` tree for a 65-layer desktop image is **52 MB**.
`inodes`, `byPath`, retained `dirData`, per-node strings — all of it lives
inside that number. Design option C ("reduce the in-memory tree") would
have bought tens of megabytes against a 4 GiB problem.

Note also `objects` more than doubling across the overlay while heap moves
60 MB: that is the shape of metadata for 42k new files. Heap on its own
cannot tell "holds metadata" from "holds content" — which is exactly why
`reportMem` prints heap *and* per-arena bytes together.

### The fix

Post-unpack writes go to a second OPFS arena (`tbox-post.bin`). Three
parts that are not obvious:

1. **Refs are namespaced per arena** — `a:` layers, `o:` post-unpack,
   `s:` authored EROFS. Two arenas both minting `a:<off>:<len>` would
   resolve one ref against two different files at the same offsets: a
   silently corrupt EROFS, no error raised anywhere.
2. **Read-before-seal is handled, not avoided.** The overlay rewrites
   `/etc/{passwd,shadow,group,gshadow}` and `EnsureLiveUser` reads them
   straight back. OPFS does not commit an open `FileSystemWritableFileStream`,
   so the old lazy `getFile()` snapshot would have returned stale or short
   bytes silently. `opfsArena.Open` now seals and reopens-with-seek to
   resume appending.
3. **The post arena is sealed before `WriteErofs`**, which reads bodies
   from both arenas.

`oci.MemStore` is gone from the wasm store entirely rather than left as a
fallback. A heap-backed blob store is the one thing this engine cannot
afford, and keeping one wired up invites the bug back the next time
something unpacks through the post path.

---

## Still true, still worth not re-deriving

**Memory64 — do not pursue.** Browsers ship it (Chrome M133; Wasm 3.0),
but **Go cannot target it** — there is no memory64 backend, and
golang/go#63131 is about a *32-bit* `wasm32` for wasip1. For a Go engine
there is no "raise the limit" option. The ~4 GiB linear-memory ceiling is
a hard constraint to design under, not a tunable.

**Wasm linear memory only grows.** It is never returned to the host, so
transient peaks are permanent for the life of the module. Anything that
briefly holds content, even if promptly freed, raises the high-water mark
for good.

**Firefox is materially worse.** Go's js/wasm transport falls back to
`arrayBuffer()` when the browser lacks streaming response bodies — i.e.
Firefox — buffering each layer body whole into linear memory. Any
"largest buildable image" figure is Chromium-only.

**`await()` in `cmd/tbwasm/opfsstore.go` has no timeout.** It blocks on
`<-done` for a JS promise that may never settle — the same unbreakable-wait
class as the fetch bug fixed in #157, but in our own code. Deliberately
*not* bundled into this fix: mixing an unbreakable-wait change into an OOM
change means a red run cannot tell you which one caused it.

**Multi-extent ISO9660 (#160) is an OPEN PR, and it is not independent —
it is the next hard blocker.** An earlier revision of this doc described
it as done, which reads as merged; it is not. Fixing the ceiling is
exactly what lets a build reach ISO assembly, where a desktop edition's
~4.9 GB EROFS rootfs immediately trips
`exceeds the single-extent ISO9660 limit (4 GiB)`. #156 and #160 are
therefore sequential: neither alone produces an ISO for any desktop
edition. Merging #160 makes the full chain write a 5.36 GB image
(`TestIsoBrowserShapeOver4GiB`).

**The two-bug split in `iso-builder`'s `ci.yml` header still stands.** The
layer-stall hang (#157) killed large editions *before* they reached
authoring, so there was only ever **one** OOM datapoint — `flounder:xfce`,
an edition that does have a live-overlay tag. "8 of 9 red" was never 8
instances of this bug.

---

## Next walls (distinct from the ceiling — do not conflate)

Neither is a wasm32 problem, and neither shows up in `tboxWasmMB()`.

**OPFS quota.** Peak origin-storage usage is now base + post + authored
EROFS simultaneously — roughly 2.4 + 2.6 + 5 GB for `flounder:xfce`
against the ~10.7 GB the app reports free. Larger editions will exceed it.
The layer and post arenas cannot simply be dropped after authoring: the
ISO write still reads kernel and initramfs bodies out of them.

**The headless ISO sink.** `app.js` uses `showSaveFilePicker` when it can,
but with `?autodl` (and in any browser lacking the File System Access
API) it accumulates the whole ISO into `chunks[]` and then a `Blob` — in
**JS** heap. A multi-GB ISO there will present as an unexplained tab death
with no Go panic and no memory-guard trip, because the memory guard only
samples wasm linear memory.

**Uncompressed EROFS.** `erofs.go` writes `FLAT_PLAIN`, so a 2.17 GB
`marlin:gnome` becomes a 7.4 GB ISO. Z_EROFS lz4/zstd clusters would cut
both the ISO size and the OPFS peak above. Real work, but it now attacks
the *remaining* problems rather than a misdiagnosed one.

**Publishing pre-authored artifacts from CI** (tunaOS#673) remains the
strategic option: have CI publish the authored EROFS as an OCI artifact so
the browser assembles rather than builds. It sidesteps quota and sink
alike, at the cost of the browser no longer being a builder for those
editions.

---

## How to reproduce without a GitHub runner

Runners are frequently saturated; do not debug this through CI.

```bash
GOOS=js GOARCH=wasm go build -o iso-builder/app/public/tbox.wasm ./cmd/tbwasm

rsync -az --exclude node_modules --exclude .git app e2e <build-host>:/var/tmp/isob/

ssh <build-host> 'mkdir -p /var/tmp/isob/home && cd /var/tmp/isob && podman run --rm --shm-size=2g \
  -v /var/tmp/isob:/w:z -w /w/e2e \
  -e TBOX_E2E_FULL=1 -e TBOX_E2E_IMAGE=tuna-os/flounder:xfce \
  -e HOME=/w/home \
  mcr.microsoft.com/playwright:v1.62.0-noble \
  bash -lc "npm ci && npx playwright test --grep @full --timeout=3900000"'
```

`flounder:xfce` is the right test case: at 1.04 GB compressed it is the
*smallest* edition, it still OOMed, and it is one of the 39 editions with
a `live-overlay` tag — so it exercises the bug and fails fast.

`--shm-size=2g` matters; a small `/dev/shm` makes chromium fail for
unrelated reasons. `HOME` must be on the mounted volume because Chrome
derives the OPFS quota from that partition's free space.

The `@full` spec forwards page console to the run log, so `reportMem`'s
`tbox: phase=...` lines land there next to the 30s
`[stage] <phase> wasm=<N> MB` heartbeat.

Interactive alternative: `corral`'s `tuna-lab iso <ref>` builds an ISO and
boots it with a console. Note corral's qemu unit has **no `-serial`**, so
guest serial is not captured there — fine for looking at a desktop,
useless for a boot that prints nothing.

---

## Definition of done

`flounder:xfce` and `marlin:kde-cachyos` both build an ISO in the browser
and pass `@full` in `iso-builder`'s `full-matrix`, with peak
`tboxWasmMB()` reported and comfortably under 4096.

`marlin:kde-cachyos` has no `live-overlay` tag, so it never hit this bug;
it was blocked by the layer stall (#157). It is the check that nothing
*else* holds gigabytes.
