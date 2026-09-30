//go:build js && wasm

package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"strings"
	"syscall/js"

	"github.com/ulikunitz/xz"

	tacklebox "github.com/tuna-os/tacklebox"
	"github.com/tuna-os/tacklebox/internal/purefs"
)

// buildDdiIso is the DDI input path (tacklebox#172), exposed as
// tboxBuildDdiIso(opts, onChunk) → Promise<bytesWritten> with opts:
//
//	{ base: string,            // artifact directory URL (SHA256SUMS inside)
//	  stem: string|undefined,  // e.g. "snow-ab"; required if several published
//	  label: string,
//	  sdboot: Uint8Array }     // a systemd-boot PE the app supplies
//
// Unlike the OCI path there is no unpack and no EROFS authoring — the
// published root partition IS the live EROFS — so none of the wasm32
// ceiling pressure of tacklebox#156 applies: peak linear memory is the
// UKI plus chunk buffers. The root artifact streams through xz into an
// OPFS arena and out into the ISO.
//
// sdboot must be supplied because the artifact set ships only a UKI,
// and a UKI cannot boot live media verbatim: its baked cmdline mounts
// the verity root by GPT partition UUID, which an ISO9660 does not
// have. The UKI's kernel/initrd are extracted and driven through a BLS
// entry with tbox live kargs instead.
func buildDdiIso(_ js.Value, args []js.Value) any {
	opts := args[0]
	onChunk := args[1]
	base := strings.TrimRight(opts.Get("base").String(), "/")
	label := opts.Get("label").String()
	stem := ""
	if v := opts.Get("stem"); v.Type() == js.TypeString {
		stem = v.String()
	}
	var sdboot []byte
	if v := opts.Get("sdboot"); v.Type() == js.TypeObject {
		sdboot = make([]byte, v.Get("length").Int())
		js.CopyBytesToGo(sdboot, v)
	}
	return promise(func() (any, error) {
		if len(sdboot) == 0 {
			return nil, fmt.Errorf("DDI build needs opts.sdboot (a systemd-boot PE): the artifact set ships only a UKI, whose baked cmdline cannot mount a live ISO root")
		}

		emitProgress("resolve", 0, 1)
		manifest, err := httpGetAll(base + "/SHA256SUMS")
		if err != nil {
			return nil, fmt.Errorf("SHA256SUMS: %w", err)
		}
		rel, err := purefs.ResolveDdiRelease(string(manifest), stem)
		if err != nil {
			return nil, err
		}
		fmt.Printf("tbox: ddi release %s (uki=%s root=%s)\n", rel.Version, rel.UKI, rel.Root)
		emitProgress("resolve", 1, 1)

		ukiBytes, err := httpGetAll(base + "/" + rel.UKI)
		if err != nil {
			return nil, err
		}
		if err := checkSha(rel.UKI, ukiBytes, rel.UKISHA); err != nil {
			return nil, err
		}
		sections, err := purefs.ExtractUKI(ukiBytes)
		if err != nil {
			return nil, err
		}
		emitProgress("initrd", 0, 1)
		overlay, err := purefs.BuildInitrdOverlayScriptsOnly(tacklebox.DracutModules)
		if err != nil {
			return nil, err
		}
		initrd := append(overlay, sections.Initrd...)
		emitProgress("initrd", 1, 1)
		reportMem("ddi-uki")

		// Root artifact → OPFS, xz-decoded in flight; sha over the bytes
		// as published (compressed).
		envID := "ddi-live"
		sfsName := envID + ".rootfs.sfs"
		rootArena, err := newOpfsArena("tbox-ddi-root.img", "d")
		if err != nil {
			return nil, err
		}
		defer rootArena.Destroy()
		if err := fetchRootToArena(base+"/"+rel.Root, rel.Root, rel.RootSHA, rootArena); err != nil {
			return nil, err
		}
		sfsSize := rootArena.off
		if err := rootArena.Seal(); err != nil {
			return nil, err
		}
		sfsSource := func() (io.ReadCloser, error) {
			return rootArena.Open(formatArenaRef("d", 0, sfsSize))
		}
		reportMem("ddi-root")

		kargs := fmt.Sprintf(
			"root=tbox:CDLABEL=%s tacklebox.live.squashimg=%s"+
				" tacklebox.live.overlay.size=8192 enforcing=0"+
				" tacklebox.env=%s console=ttyS0,115200n8",
			label, sfsName, envID)
		kernelPath := "/images/pxeboot/" + envID + "/vmlinuz"
		initrdPath := "/images/pxeboot/" + envID + "/initrd.img"
		entry := fmt.Sprintf("title %s %s (live)\nlinux %s\ninitrd %s\noptions %s\n",
			label, rel.Version, kernelPath, initrdPath, kargs)

		emitProgress("esp", 0, 1)
		esp, err := purefs.BuildEspBytes([]purefs.EspFile{
			{Path: "/EFI/BOOT/BOOTX64.EFI", Source: bytesSource(sdboot)},
			{Path: "/loader/loader.conf", Source: purefs.StringSource("timeout 3\n")},
			{Path: "/loader/entries/" + envID + ".conf", Source: purefs.StringSource(entry)},
			{Path: kernelPath, Source: bytesSource(sections.Linux)},
			{Path: initrdPath, Source: bytesSource(initrd)},
		})
		if err != nil {
			return nil, err
		}
		emitProgress("esp", 1, 1)

		emitProgress("iso", 0, 1)
		jw := &jsChunkWriter{cb: onChunk}
		inputs := []purefs.IsoInput{
			{Path: "/EFI/efi.img", Size: int64(len(esp)), Source: bytesSource(esp)},
			{Path: "/EFI/BOOT/BOOTX64.EFI", Size: int64(len(sdboot)), Source: bytesSource(sdboot)},
			{Path: kernelPath, Size: int64(len(sections.Linux)), Source: bytesSource(sections.Linux)},
			{Path: initrdPath, Size: int64(len(initrd)), Source: bytesSource(initrd)},
			{Path: "/LiveOS/" + sfsName, Size: sfsSize, Source: sfsSource},
		}
		if err := purefs.WriteIso9660(jw, label, inputs, "/EFI/efi.img"); err != nil {
			return nil, err
		}
		emitProgress("iso", 1, 1)
		reportMem("ddi-iso")
		return jw.written, nil
	})
}

func httpGetAll(url string) ([]byte, error) {
	resp, err := http.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	return io.ReadAll(resp.Body)
}

func fetchRootToArena(url, name, wantSha string, arena *opfsArena) error {
	resp, err := http.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	h := sha256.New()
	var body io.Reader = io.TeeReader(resp.Body, h)
	if strings.HasSuffix(name, ".xz") {
		xr, err := xz.NewReader(body)
		if err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		body = xr
	}
	if _, err := io.Copy(arenaWriter{arena}, body); err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	if wantSha != "" {
		if got := hex.EncodeToString(h.Sum(nil)); got != strings.ToLower(wantSha) {
			return fmt.Errorf("%s: sha256 mismatch (manifest %s, artifact %s)", name, wantSha, got)
		}
	}
	return nil
}

func checkSha(name string, b []byte, want string) error {
	if want == "" {
		return nil
	}
	got := sha256.Sum256(b)
	if hex.EncodeToString(got[:]) != strings.ToLower(want) {
		return fmt.Errorf("%s: sha256 mismatch (manifest %s, artifact %s)", name, want, hex.EncodeToString(got[:]))
	}
	return nil
}
