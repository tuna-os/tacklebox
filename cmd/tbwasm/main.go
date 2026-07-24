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
// Memory model (MVP): MemStore holds unpacked file bodies and the EROFS
// is buffered before streaming — peak ≈ 2× unpacked rootfs. Fine for
// base images; desktop images need the OPFS-backed store (tracked in
// tunaOS#667).
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
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
	js.Global().Set("tboxReset", js.FuncOf(func(js.Value, []js.Value) any {
		if gStore != nil && gStore.arena != nil {
			gStore.arena.Destroy()
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
					reject.Invoke(fmt.Sprint(r))
				}
			}()
			v, err := fn()
			if err != nil {
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
		arena, err := newOpfsArena("tbox-arena.bin")
		if err != nil {
			return nil, err
		}
		gStore = &hybridStore{arena: arena, mem: &oci.MemStore{}}
		root, err := c.Unpack(ref, m, arena, func(i, n int) {
			emitProgress("unpack", i+1, n)
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
	return promise(func() (any, error) {
		if gRoot == nil {
			return nil, fmt.Errorf("introspect an image first")
		}
		root, store := gRoot, gStore

		// Live-overlay parity artifact (tunaOS#673): tuna-os/<variant>:<flavor>
		// images have a published customize delta at
		// tuna-os/live-overlay:<variant>-<flavor> — apply its non-base
		// layers so the browser ISO carries the identical live payload
		// (installer flatpak, autologin, polkit) CI media do. Best-effort:
		// absence just means the plain baseline.
		if strings.HasPrefix(gImage, "tuna-os/") && gClient != nil {
			parts := strings.SplitN(strings.TrimPrefix(gImage, "tuna-os/"), ":", 2)
			if len(parts) == 2 {
				ovRef := oci.Ref{Repo: "tuna-os/live-overlay", Tag: parts[0] + "-" + parts[1]}
				if ovm, err := gClient.ResolveManifest(ovRef, "amd64"); err == nil {
					skip := map[string]bool{}
					for _, l := range gManifest.Layers {
						skip[l.Digest] = true
					}
					emitProgress("overlay", 0, 1)
					if err := gClient.UnpackOnto(root, ovRef, ovm, store, skip, nil); err != nil {
						fmt.Println("!!! live overlay skipped:", err)
					}
					emitProgress("overlay", 1, 1)
				}
			}
		}

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

		kver := gFacts.KernelVer
		if kver == "" {
			return nil, fmt.Errorf("no kernel in image")
		}
		envID := "browser-live"
		sfsName := envID + ".rootfs.sfs"

		emitProgress("erofs", 0, 1)
		sfsArena, err := newOpfsArena("tbox-erofs.img")
		if err != nil {
			return nil, err
		}
		defer sfsArena.Destroy()
		if err := purefs.WriteErofs(root, store, arenaWriter{sfsArena}, 0); err != nil {
			return nil, err
		}
		sfsSize := sfsArena.off
		if err := sfsArena.Seal(); err != nil {
			return nil, err
		}
		sfsSource := func() (io.ReadCloser, error) {
			return sfsArena.Open(fmt.Sprintf("a:0:%d", sfsSize))
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
		sdSrc, _, err := blob("usr/lib/systemd/boot/efi/systemd-bootx64.efi")
		if err != nil {
			return nil, fmt.Errorf("image ships no systemd-boot (EL10-style images need a supplied one): %w", err)
		}

		kargs := fmt.Sprintf(
			"root=tbox:CDLABEL=%s tacklebox.live.squashimg=%s"+
				" tacklebox.live.overlay.size=8192 enforcing=0"+
				" tacklebox.env=%s console=ttyS0,115200n8",
			label, sfsName, envID)
		entry := fmt.Sprintf("title TunaOS %s (live)\nlinux /images/pxeboot/%s/vmlinuz\ninitrd /images/pxeboot/%s/initrd.img\noptions %s\n",
			envID, envID, envID, kargs)
		kernelPath := "/images/pxeboot/" + envID + "/vmlinuz"
		initrdPath := "/images/pxeboot/" + envID + "/initrd.img"

		emitProgress("esp", 0, 1)
		esp, err := purefs.BuildEspBytes([]purefs.EspFile{
			{Path: "/EFI/BOOT/BOOTX64.EFI", Source: sdSrc},
			{Path: "/loader/loader.conf", Source: purefs.StringSource("timeout 3\n")},
			{Path: "/loader/entries/" + envID + ".conf", Source: purefs.StringSource(entry)},
			{Path: kernelPath, Source: kernelSrc},
			{Path: initrdPath, Source: initrdSrc},
		})
		if err != nil {
			return nil, err
		}
		emitProgress("esp", 1, 1)

		emitProgress("iso", 0, 1)
		jw := &jsChunkWriter{cb: onChunk}
		inputs := []purefs.IsoInput{
			{Path: "/EFI/efi.img", Size: int64(len(esp)), Source: bytesSource(esp)},
			{Path: "/EFI/BOOT/BOOTX64.EFI", Size: mustSize(sdSrc), Source: sdSrc},
			{Path: kernelPath, Size: kernelSize, Source: kernelSrc},
			{Path: initrdPath, Size: initrdSize, Source: initrdSrc},
			{Path: "/LiveOS/" + sfsName, Size: sfsSize, Source: sfsSource},
		}
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

func splitRef(s string) (repo, tag string, ok bool) {
	i := -1
	for j := len(s) - 1; j >= 0; j-- {
		if s[j] == ':' {
			i = j
			break
		}
		if s[j] == '/' {
			break
		}
	}
	if i <= 0 || i == len(s)-1 {
		return "", "", false
	}
	return s[:i], s[i+1:], true
}
