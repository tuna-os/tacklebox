//go:build js && wasm

// tbwasm is the browser entry for the tacklebox pure-Go core: the same
// packages purebuild proves natively (registry pull → overlay unpack →
// introspection → liveuser → EROFS → FAT ESP → ISO9660/El Torito),
// compiled to WASM and exposed as three JS calls:
//
//	tboxIntrospect(image, registry) → Promise<factsJSON>
//	tboxBuildIso(opts, onChunk)     → Promise<bytesWritten>
//	tboxReset()
//
// opts: { label, initrd: Uint8Array|null }. onChunk receives Uint8Array
// pieces of the ISO as they stream — the caller appends them to an OPFS
// file or a download stream. State (tree + blob store) lives between the
// two calls so the GUI can show facts before committing to a build.
//
// Memory model: nothing the size of the image is held in linear memory.
// Layer bodies stream to an OPFS arena during unpack, post-unpack tree
// surgery (live overlay, liveuser, autologin) streams to a second arena,
// the authored EROFS streams to a third, and reads slice back out of
// OPFS 4 MiB at a time. The wasm heap holds the oci.Node tree, the EROFS
// inode table and chunk buffers only — metadata scale, not content
// scale. That distinction is load-bearing: wasm32 gives a single 32-bit
// linear memory, so ~4 GiB is a hard ceiling with no host tunable and no
// Memory64 escape (Go cannot target it) — tacklebox#156.
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"runtime"
	"strings"
	"syscall/js"

	tacklebox "github.com/tuna-os/tacklebox"
	"github.com/tuna-os/tacklebox/internal/oci"
	"github.com/tuna-os/tacklebox/internal/purefs"
)

var (
	gRoot     *oci.Node
	gStore    *hybridStore
	gFacts    purefs.ImageFacts
	gClient   *oci.Client
	gManifest *oci.Manifest
	gImage    string
)

func main() {
	js.Global().Set("tboxIntrospect", js.FuncOf(introspect))
	js.Global().Set("tboxBuildIso", js.FuncOf(buildIso))
	js.Global().Set("tboxBuildDdiIso", js.FuncOf(buildDdiIso))
	js.Global().Set("tboxReset", js.FuncOf(func(js.Value, []js.Value) any {
		if gStore != nil {
			gStore.destroy()
		}
		gRoot, gStore = nil, nil
		return nil
	}))
	select {}
}

// promise runs fn on a goroutine and resolves/rejects a JS Promise.
func promise(fn func() (any, error)) js.Value {
	handler := js.FuncOf(func(_ js.Value, args []js.Value) any {
		resolve, reject := args[0], args[1]
		go func() {
			defer func() {
				if r := recover(); r != nil {
					// Console, not just the rejection: app.js routes the
					// rejection to log(), which writes to the DOM, so a
					// headless run's only record of WHY a build died was a
					// screenshot. The e2e greps console for engine death.
					fmt.Println("!!! tbox panic:", r)
					reject.Invoke(fmt.Sprint(r))
				}
			}()
			v, err := fn()
			if err != nil {
				fmt.Println("!!! tbox failed:", err)
				reject.Invoke(err.Error())
				return
			}
			resolve.Invoke(v)
		}()
		return nil
	})
	return js.Global().Get("Promise").New(handler)
}

func emitProgress(stage string, i, n int) {
	if cb := js.Global().Get("tboxOnProgress"); cb.Type() == js.TypeFunction {
		cb.Invoke(stage, i, n)
	}
}

// reportMem prints the two numbers that distinguish "the heap holds
// metadata" from "the heap holds image content" — the distinction the
// whole wasm32 ceiling turns on (tacklebox#156). Heap alone is ambiguous;
// heap next to bytes-parked-in-OPFS is not. Cheap enough to leave in: a
// ReadMemStats per phase boundary, not per file.
func reportMem(phase string) {
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	var layerMB, postMB int64
	if gStore != nil {
		if gStore.arena != nil {
			layerMB = gStore.arena.off >> 20
		}
		if gStore.post != nil {
			postMB = gStore.post.off >> 20
		}
	}
	fmt.Printf("tbox: phase=%s heap=%dMB sys=%dMB objects=%d opfs(layers=%dMB post=%dMB)\n",
		phase, ms.HeapAlloc>>20, ms.Sys>>20, ms.HeapObjects, layerMB, postMB)
}

func introspect(_ js.Value, args []js.Value) any {
	image := args[0].String()
	registry := "https://ghcr.io"
	if len(args) > 1 && args[1].Type() == js.TypeString && args[1].String() != "" {
		registry = args[1].String()
	}
	return promise(func() (any, error) {
		repo, tag, ok := splitRef(image)
		if !ok {
			return nil, fmt.Errorf("image must be <repo>:<tag>")
		}
		c := oci.NewClient(registry)
		// Boot-irrelevant junk never hits origin storage (quota!).
		c.SkipBodies = func(p string) bool {
			for _, pre := range []string{"tmp/", "var/tmp/", "var/cache/", "var/log/", "run/"} {
				if strings.HasPrefix(p, pre) {
					return true
				}
			}
			return false
		}
		ref := oci.Ref{Repo: repo, Tag: tag}
		emitProgress("resolve", 0, 1)
		m, err := c.ResolveManifest(ref, "amd64")
		if err != nil {
			return nil, err
		}
		arena, err := newOpfsArena("tbox-arena.bin", "a")
		if err != nil {
			return nil, err
		}
		gStore = &hybridStore{arena: arena}
		// Sample inside the layer loop, not just at the end. marlin:niri
		// died here at 4.28 GB having reported 71 MB nine layers earlier,
		// and a single end-of-phase sample cannot tell "grew steadily"
		// from "one layer did it" — which is the difference between a
		// leak and a single pathological entry. Every 8 layers keeps the
		// log short while still bracketing any jump to one layer.
		root, err := c.Unpack(ref, m, arena, func(i, n int) {
			emitProgress("unpack", i+1, n)
			if i%8 == 0 {
				reportMem(fmt.Sprintf("unpack-layer-%d", i))
			}
		})
		if err != nil {
			return nil, err
		}
		if err := arena.Seal(); err != nil {
			return nil, err
		}
		gRoot = root
		gClient, gManifest, gImage = c, m, image
		gFacts = purefs.Introspect(root)
		reportMem("unpack")
		b, _ := json.Marshal(gFacts)
		return string(b), nil
	})
}

func buildIso(_ js.Value, args []js.Value) any {
	opts := args[0]
	onChunk := args[1]
	label := opts.Get("label").String()
	var initrd []byte
	if v := opts.Get("initrd"); v.Type() == js.TypeObject {
		initrd = make([]byte, v.Get("length").Int())
		js.CopyBytesToGo(initrd, v)
	}
	var flatpaks []string
	if v := opts.Get("flatpaks"); v.Type() == js.TypeObject {
		for i := 0; i < v.Get("length").Int(); i++ {
			flatpaks = append(flatpaks, v.Index(i).String())
		}
	}
	strSlice := func(key string) []string {
		var out []string
		if v := opts.Get(key); v.Type() == js.TypeObject {
			for i := 0; i < v.Get("length").Int(); i++ {
				out = append(out, v.Index(i).String())
			}
		}
		return out
	}
	packages := strSlice("packages")
	extraRun := strSlice("extraRun")
	liveMarker := ""
	if v := opts.Get("liveMarker"); v.Type() == js.TypeString {
		liveMarker = v.String()
	}
	return promise(func() (any, error) {
		if gRoot == nil {
			return nil, fmt.Errorf("introspect an image first")
		}
		root, store := gRoot, gStore

		// Live-overlay parity artifact (tunaOS#673) — see
		// purefs.GraftLiveOverlay. Shared with cmd/purebuild so the native
		// build produces the byte-identical artifact this one does; that
		// shared call is the only thing making the two paths comparable.
		// Best-effort: absence just means the plain baseline.
		emitProgress("overlay", 0, 1)
		if _, err := purefs.GraftLiveOverlay(root, store, gClient, gImage, gManifest, nil); err != nil {
			fmt.Println("!!! live overlay skipped:", err)
		}
		emitProgress("overlay", 1, 1)
		reportMem("overlay")

		if err := purefs.EnsureLiveUser(root, store, "liveuser", 1000); err != nil {
			return nil, err
		}
		// Autologin + display-manager (baseline.sh's pure-Go sibling): the
		// live-overlay above only exists for a few variants, so without this
		// every other browser ISO boots to a greeter no blank password
		// satisfies. Idempotent, so re-applying over an overlay is harmless.
		if err := purefs.EnsureAutologin(root, store, purefs.DetectDesktop(root), "liveuser"); err != nil {
			return nil, err
		}
		// Readiness marker for serial-polling e2e harnesses; images
		// shipping their own readiness unit are left untouched.
		// opts.liveMarker overrides the neutral default.
		if err := purefs.EnsureLiveReadyMarker(root, store, liveMarker); err != nil {
			return nil, err
		}

		kver := gFacts.KernelVer
		if kver == "" {
			return nil, fmt.Errorf("no kernel in image")
		}
		envID := "browser-live"
		sfsName := envID + ".rootfs.sfs"

		emitProgress("erofs", 0, 1)
		// WriteErofs reads bodies from both arenas, and the post arena is
		// still open for writes at this point — OPFS does not commit an
		// open writable stream, so authoring before this seal would read
		// short.
		if err := store.seal(); err != nil {
			return nil, fmt.Errorf("seal post-unpack arena: %w", err)
		}
		reportMem("pre-erofs")
		sfsArena, err := newOpfsArena("tbox-erofs.img", "s")
		if err != nil {
			return nil, err
		}
		defer sfsArena.Destroy()
		if err := purefs.WriteErofs(root, store, arenaWriter{sfsArena}, 0); err != nil {
			return nil, err
		}
		reportMem("erofs")
		sfsSize := sfsArena.off
		if err := sfsArena.Seal(); err != nil {
			return nil, err
		}
		sfsSource := func() (io.ReadCloser, error) {
			return sfsArena.Open(formatArenaRef("s", 0, sfsSize))
		}
		emitProgress("erofs", 1, 1)

		blob := func(p string) (func() (io.ReadCloser, error), int64, error) {
			n := root.Lookup(p)
			if n == nil || n.Type != oci.TypeFile {
				return nil, 0, fmt.Errorf("missing in image: %s", p)
			}
			return func() (io.ReadCloser, error) { return store.Open(n.Ref) }, n.Size, nil
		}
		kernelSrc, kernelSize, err := blob("usr/lib/modules/" + kver + "/vmlinuz")
		if err != nil {
			return nil, err
		}
		initrdSrc, initrdSize, err := blob("usr/lib/modules/" + kver + "/initramfs.img")
		if len(initrd) > 0 {
			// explicit override wins
			initrdSrc = func() (io.ReadCloser, error) {
				return io.NopCloser(bytes.NewReader(initrd)), nil
			}
			initrdSize = int64(len(initrd))
			err = nil
		} else if err == nil {
			// Auto: append the tbox overlay segment (embedded dracut
			// module scripts + the image's own fs/device kernel modules)
			// to the stock initramfs — live-bootable with no supplied
			// artifacts (the tacklebox engine "tackles the initramfs").
			emitProgress("initrd", 0, 1)
			overlay, oerr := purefs.BuildInitrdOverlay(root, store, kver, tacklebox.DracutModules)
			if oerr != nil {
				return nil, fmt.Errorf("initrd overlay: %w", oerr)
			}
			stock, oerr := blobBytes(initrdSrc)
			if oerr != nil {
				return nil, oerr
			}
			combined := append(overlay, stock...)
			initrdSrc = bytesSource(combined)
			initrdSize = int64(len(combined))
			emitProgress("initrd", 1, 1)
		}
		if err != nil {
			return nil, fmt.Errorf("no initramfs in image: %w", err)
		}
		// Two boot chains, resolved wootc-style from the tree (see
		// purefs.DetectBootChain): systemd-boot when the image ships it
		// (Debian ships only the .signed name — an sbat-signed PE that
		// boots unchanged), else the signed shim+GRUB pair from the
		// image's bootupd payload. aurora/bluefin-family images carry no
		// systemd-boot at all — this used to hard-error here with
		// "EL10-style images need a supplied one".
		chain, err := purefs.DetectBootChain(root)
		if err != nil {
			return nil, err
		}

		kargs := fmt.Sprintf(
			"root=tbox:CDLABEL=%s tacklebox.live.squashimg=%s"+
				" tacklebox.live.overlay.size=8192 enforcing=0"+
				" tacklebox.env=%s console=ttyS0,115200n8",
			label, sfsName, envID)
		kernelPath := "/images/pxeboot/" + envID + "/vmlinuz"
		initrdPath := "/images/pxeboot/" + envID + "/initrd.img"

		var espFiles []purefs.EspFile
		// The ISO tree carries the same boot files outside efi.img as
		// inside it (some firmware reads the ISO filesystem directly);
		// whatever the chain stages on the ESP is mirrored here.
		var bootInputs []purefs.IsoInput
		switch chain.Kind {
		case "sdboot":
			sdSrc, sdSize, berr := blob(chain.SdBoot)
			if berr != nil {
				return nil, berr
			}
			entry := fmt.Sprintf("title TunaOS %s (live)\nlinux %s\ninitrd %s\noptions %s\n",
				envID, kernelPath, initrdPath, kargs)
			espFiles = []purefs.EspFile{
				{Path: "/EFI/BOOT/BOOTX64.EFI", Source: sdSrc},
				{Path: "/loader/loader.conf", Source: purefs.StringSource("timeout 3\n")},
				{Path: "/loader/entries/" + envID + ".conf", Source: purefs.StringSource(entry)},
			}
			bootInputs = []purefs.IsoInput{
				{Path: "/EFI/BOOT/BOOTX64.EFI", Size: sdSize, Source: sdSrc},
			}
		case "grub2":
			shimSrc, shimSize, berr := blob(chain.Shim)
			if berr != nil {
				return nil, berr
			}
			grubSrc, grubSize, berr := blob(chain.Grub)
			if berr != nil {
				return nil, berr
			}
			cfg := purefs.LiveGrubCfg("TunaOS "+envID+" (live)", kernelPath, initrdPath, kargs)
			// shim boots as BOOTX64.EFI and loads grubx64.efi from its own
			// directory; grub.cfg goes to every prefix a signed GRUB has
			// been observed to search (its own dir and the vendor dir).
			espFiles = []purefs.EspFile{
				{Path: "/EFI/BOOT/BOOTX64.EFI", Source: shimSrc},
				{Path: "/EFI/BOOT/grubx64.efi", Source: grubSrc},
				{Path: "/EFI/BOOT/grub.cfg", Source: purefs.StringSource(cfg)},
				{Path: "/EFI/" + chain.Vendor + "/grub.cfg", Source: purefs.StringSource(cfg)},
			}
			bootInputs = []purefs.IsoInput{
				{Path: "/EFI/BOOT/BOOTX64.EFI", Size: shimSize, Source: shimSrc},
				{Path: "/EFI/BOOT/grubx64.efi", Size: grubSize, Source: grubSrc},
				{Path: "/EFI/BOOT/grub.cfg", Size: int64(len(cfg)), Source: bytesSource([]byte(cfg))},
			}
			if chain.MokMgr != "" {
				mmSrc, mmSize, merr := blob(chain.MokMgr)
				if merr == nil {
					espFiles = append(espFiles, purefs.EspFile{Path: "/EFI/BOOT/mmx64.efi", Source: mmSrc})
					bootInputs = append(bootInputs, purefs.IsoInput{Path: "/EFI/BOOT/mmx64.efi", Size: mmSize, Source: mmSrc})
				}
			}
		}
		espFiles = append(espFiles,
			purefs.EspFile{Path: kernelPath, Source: kernelSrc},
			purefs.EspFile{Path: initrdPath, Source: initrdSrc},
		)

		emitProgress("esp", 0, 1)
		esp, err := purefs.BuildEspBytes(espFiles)
		if err != nil {
			return nil, err
		}
		emitProgress("esp", 1, 1)

		emitProgress("iso", 0, 1)
		jw := &jsChunkWriter{cb: onChunk}
		inputs := append([]purefs.IsoInput{
			{Path: "/EFI/efi.img", Size: int64(len(esp)), Source: bytesSource(esp)},
		}, bootInputs...)
		inputs = append(inputs,
			purefs.IsoInput{Path: kernelPath, Size: kernelSize, Source: kernelSrc},
			purefs.IsoInput{Path: initrdPath, Size: initrdSize, Source: initrdSrc},
			purefs.IsoInput{Path: "/LiveOS/" + sfsName, Size: sfsSize, Source: sfsSource},
		)
		if len(flatpaks) > 0 {
			manifest, _ := json.Marshal(map[string]any{"preload": flatpaks})
			inputs = append(inputs, purefs.IsoInput{
				Path: "/LiveOS/flatpak-preload.json", Size: int64(len(manifest)),
				Source: bytesSource(manifest),
			})
		}
		// remora manifest (tacklebox#99): packages + custom repos/config
		// (extra_run) ride into the installed system, where remora rebuilds
		// the layers on the upstream base and bootc-switches — customization
		// that persists AND keeps updating.
		if len(packages) > 0 || len(extraRun) > 0 {
			ry := purefs.RemoraManifest(gFacts.PkgManager, packages, extraRun)
			inputs = append(inputs, purefs.IsoInput{
				Path: "/LiveOS/remora/remora.yaml", Size: int64(len(ry)),
				Source: bytesSource([]byte(ry)),
			})
		}
		if err := purefs.WriteIso9660(jw, label, inputs, "/EFI/efi.img"); err != nil {
			return nil, err
		}
		emitProgress("iso", 1, 1)
		reportMem("iso")
		return jw.written, nil
	})
}

func blobBytes(src func() (io.ReadCloser, error)) ([]byte, error) {
	rc, err := src()
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return io.ReadAll(rc)
}

func bytesSource(b []byte) func() (io.ReadCloser, error) {
	return func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(b)), nil
	}
}

func mustSize(src func() (io.ReadCloser, error)) int64 {
	rc, err := src()
	if err != nil {
		return 0
	}
	defer rc.Close()
	n, _ := io.Copy(io.Discard, rc)
	return n
}

type jsChunkWriter struct {
	cb      js.Value
	written int
}

func (w *jsChunkWriter) Write(p []byte) (int, error) {
	u8 := js.Global().Get("Uint8Array").New(len(p))
	js.CopyBytesToJS(u8, p)
	w.cb.Invoke(u8)
	w.written += len(p)
	return len(p), nil
}
