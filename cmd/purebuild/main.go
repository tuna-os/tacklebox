// purebuild assembles a live TunaOS ISO entirely in Go — no podman, no
// mksquashfs, no xorriso, no sudo. It is the native proving harness for
// the pure-Go core (tunaOS ADR 0002); the WASM browser builder wires the
// same packages behind a UI.
//
// Layout and kernel cmdline mirror internal/target.IsoTarget exactly, so
// the ISO boots through the same tbox-live dracut path as production
// media. The initramfs must contain the tbox modules: pass --initrd with
// a rebuilt initramfs (dracut --add "tbox-live tbox-root" inside the
// image), or the boot will stop in the stock initrd.
package main

import (
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"

	tacklebox "github.com/tuna-os/tacklebox"
	"github.com/tuna-os/tacklebox/internal/oci"
	"github.com/tuna-os/tacklebox/internal/purefs"
	"github.com/tuna-os/tacklebox/internal/runner"
)

var initrdOnDisk string
var sdBootDisk string

// bootDiskFile is one chain-staged boot file: its path inside the ISO/ESP
// and its on-disk source (a DirStore ref, the workdir grub.cfg, or the
// host systemd-boot fallback).
type bootDiskFile struct {
	path string
	disk string
}

var bootDiskFiles []bootDiskFile

// mustStatSize is the declared size of an ISO input. It exits rather than
// returning, because every caller is building the layout and there is no
// useful way to continue with an unknown size — the previous code wrote
// `st, _ := os.Stat(p)` and would have nil-dereferenced on a missing file
// instead of saying which one.
func mustStatSize(path string) int64 {
	st, err := os.Stat(path)
	if err != nil {
		log.Fatalf("iso input: %v", err)
	}
	return st.Size()
}

// parseImageRef validates and splits an "--image" flag value of the form
// <repo>:<tag> into its repo/tag halves, and derives the per-build env ID
// (used for the ISO volume label's kernel cmdline, the pxeboot path, and the
// live-root squashfs name). LastIndex splits on the *last* colon so a
// registry host with an explicit port (e.g. "localhost:5000/ns/img:tag")
// resolves correctly.
func parseImageRef(image string) (repo, tag, envID string, err error) {
	if image == "" || !strings.Contains(image, ":") {
		return "", "", "", fmt.Errorf("--image <repo>:<tag> is required")
	}
	repo = image[:strings.LastIndex(image, ":")]
	tag = image[strings.LastIndex(image, ":")+1:]
	envID = filepath.Base(repo) + "-" + tag
	return repo, tag, envID, nil
}

func main() {
	var (
		image      = flag.String("image", "", "image as <repo>:<tag>, e.g. tuna-os/sailfin:kde")
		registry   = flag.String("registry", "https://ghcr.io", "registry base URL (or CORS shim)")
		out        = flag.String("out", "tunaos-pure.iso", "output ISO path")
		label      = flag.String("label", "TUNAOS", "ISO volume label (CDLABEL)")
		initrd     = flag.String("initrd", "", "path to a tbox-enabled initramfs (overrides the image's stock one)")
		workdir    = flag.String("workdir", ".purebuild", "scratch directory")
		rootTar    = flag.String("rootfs-tar", "", "build from this rootfs tar (podman export of a customized container) instead of pulling; the tar is DELETED after ingest to bound disk use")
		useXorriso = flag.Bool("xorriso", false, "assemble the final ISO with xorriso (production args; native only) instead of the go-diskfs writer — avoids its staging copy")
		trim       = flag.String("trim", "var/cache,var/log,var/tmp,tmp,run", "comma-separated rootfs paths emptied before authoring (boot-irrelevant caches)")
		ddi        = flag.String("ddi", "", "build from a systemd-sysupdate v1 artifact directory (URL or local path containing SHA256SUMS + UKI + root.raw[.xz]) instead of an OCI image — tacklebox#172")
		ddiStem    = flag.String("ddi-stem", "", "artifact stem to select in the DDI manifest (e.g. snow-ab); required when the manifest lists several")
		liveMarker = flag.String("live-marker", "", "readiness string the baked tbox-live-ready.service prints to the serial console (default "+purefs.DefaultLiveMarker+")")
	)
	flag.Parse()
	if *ddi != "" {
		buildFromDdi(*ddi, *ddiStem, *label, *workdir, *out)
		return
	}
	repo, tag, envID, err := parseImageRef(*image)
	if err != nil {
		log.Fatal(err)
	}

	if err := os.MkdirAll(*workdir, 0o755); err != nil {
		log.Fatal(err)
	}

	store := &oci.DirStore{Dir: filepath.Join(*workdir, "blobs")}
	var root *oci.Node
	// Kept beyond the pull branch so the live-overlay graft below can reuse
	// them. Both stay nil on the --rootfs-tar path, which is already a
	// customized tree and must not have an overlay grafted on top.
	var client *oci.Client
	var manifest *oci.Manifest
	if *rootTar != "" {
		log.Printf(">>> ingesting rootfs tar %s", *rootTar)
		tf, err := os.Open(*rootTar)
		if err != nil {
			log.Fatal(err)
		}
		pr := &punchReader{f: tf}
		root, err = oci.ApplyTar(pr, store)
		pr.Close()
		if err != nil {
			log.Fatal(err)
		}
		// The punch-reader already released the tar's blocks; the empty
		// husk is left for the caller (deleting early would lose the
		// input if a later stage fails).
	} else {
		c := oci.NewClient(*registry)
		ref := oci.Ref{Repo: repo, Tag: tag}
		log.Printf(">>> resolving %s", *image)
		m, err := c.ResolveManifest(ref, "amd64")
		if err != nil {
			log.Fatal(err)
		}
		client, manifest = c, m
		log.Printf(">>> unpacking %d layers", len(m.Layers))
		root, err = c.Unpack(ref, m, store, func(i, n int) {
			fmt.Printf("\r    layer %d/%d", i+1, n)
		})
		fmt.Println()
		if err != nil {
			log.Fatal(err)
		}
	}

	// Empty boot-irrelevant cache/log dirs (their blobs are deleted too).
	for _, t := range strings.Split(*trim, ",") {
		t = strings.TrimSpace(t)
		if t == "" {
			continue
		}
		if n := root.Lookup(t); n != nil && n.Type == oci.TypeDir {
			n.Walk(func(_ string, c *oci.Node) error {
				if c.Type == oci.TypeFile && c.Ref != "" {
					_ = os.Remove(c.Ref)
				}
				return nil
			})
			n.Children = map[string]*oci.Node{}
		}
	}

	// Live-overlay parity artifact (tunaOS#673): apply the published
	// customize delta for this variant, exactly as the browser build does.
	// Until now only cmd/tbwasm grafted overlays, so nothing native ever
	// exercised them and the two paths could drift unobserved — this shared
	// call is what makes a native build byte-comparable with a browser one.
	//
	// Best-effort by design. The overlay is produced *by* the live customize
	// step, so requiring one would deadlock every new variant; absence just
	// means the plain baseline below.
	if applied, err := purefs.GraftLiveOverlay(root, store, client, *image, manifest,
		func(i, n int) { fmt.Printf("\r    overlay layer %d/%d", i+1, n) }); err != nil {
		log.Fatal(err)
	} else if applied {
		fmt.Println()
		log.Printf(">>> live overlay grafted")
	} else {
		log.Printf(">>> no live overlay for %s — plain baseline", *image)
	}

	// Distro-agnostic live baseline: bake the passwordless live user into
	// the squash so the desktop adapter's autologin works on first boot
	// (livesys-scripts is absent on EL10/openSUSE images).
	if err := purefs.EnsureLiveUser(root, store, "liveuser", 1000); err != nil {
		log.Fatal(err)
	}
	log.Printf(">>> liveuser baked into rootfs")

	// Autologin + display-manager setup (baseline.sh's pure-Go sibling) so
	// the live session actually reaches the desktop instead of a greeter that
	// rejects the passwordless user's blank password.
	if err := purefs.EnsureAutologin(root, store, purefs.DetectDesktop(root), "liveuser"); err != nil {
		log.Fatal(err)
	}
	log.Printf(">>> autologin + display-manager configured")

	// Readiness marker for serial-polling e2e harnesses; images shipping
	// their own readiness unit are left untouched.
	if err := purefs.EnsureLiveReadyMarker(root, store, *liveMarker); err != nil {
		log.Fatal(err)
	}

	// Kernel + stock initramfs + systemd-boot out of the image tree.
	modDir := root.Lookup("usr/lib/modules")
	if modDir == nil {
		log.Fatal("no /usr/lib/modules in image")
	}
	var kver string
	for name := range modDir.Children {
		if modDir.Lookup(name+"/vmlinuz") != nil {
			kver = name
			break
		}
	}
	if kver == "" {
		log.Fatal("no kernel found under /usr/lib/modules")
	}
	log.Printf(">>> kernel %s", kver)

	blob := func(p string) func() (io.ReadCloser, error) {
		n := root.Lookup(p)
		if n == nil || n.Type != oci.TypeFile {
			log.Fatalf("missing in image: %s", p)
		}
		return func() (io.ReadCloser, error) { return store.Open(n.Ref) }
	}
	initrdSource := blob("usr/lib/modules/" + kver + "/initramfs.img")
	initrdOnDisk = root.Lookup("usr/lib/modules/" + kver + "/initramfs.img").Ref
	if *initrd != "" {
		initrdSource = purefs.FileSource(*initrd)
		initrdOnDisk = *initrd
		log.Printf(">>> using tbox initramfs %s", *initrd)
	} else {
		// Auto: stock initramfs + tbox overlay segment — no dracut, no
		// container. The overlay carries the embedded module scripts and
		// the image's own fs/device kernel modules (insmod fallback in
		// tbox-live-root loads them without modules.dep).
		log.Printf(">>> appending tbox initrd overlay (no --initrd supplied)")
		overlay, err := purefs.BuildInitrdOverlay(root, store, kver, tacklebox.DracutModules)
		if err != nil {
			log.Fatal(err)
		}
		rc, err := initrdSource()
		if err != nil {
			log.Fatal(err)
		}
		stock, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			log.Fatal(err)
		}
		combined := filepath.Join(*workdir, "initrd-tbox.img")
		if err := os.WriteFile(combined, append(overlay, stock...), 0o644); err != nil {
			log.Fatal(err)
		}
		initrdSource = purefs.FileSource(combined)
		initrdOnDisk = combined
	}
	// Boot chain: two code paths resolved from the tree (see
	// purefs.DetectBootChain) — the image's own systemd-boot, or the
	// signed shim+GRUB pair from its bootupd payload (aurora/bluefin-
	// family images ship no systemd-boot). When the image carries
	// neither, fall back to the host's systemd-boot exactly like
	// production ExtractEFIBinary does (CI installs systemd-boot-efi).
	// DirStore refs are file paths, so every chain file has an on-disk
	// source the ISO writers can stat and stream.
	chain, chainErr := purefs.DetectBootChain(root)
	if chainErr == nil && chain.Kind == "sdboot" {
		sdBootDisk = root.Lookup(chain.SdBoot).Ref
	} else if chainErr == nil && chain.Kind == "grub2" {
		log.Printf(">>> boot chain: bootupd shim+GRUB (vendor %s) — image ships no systemd-boot", chain.Vendor)
	} else if _, err := os.Stat("/usr/lib/systemd/boot/efi/systemd-bootx64.efi"); err == nil {
		chain = &purefs.BootChain{Kind: "sdboot"}
		sdBootDisk = "/usr/lib/systemd/boot/efi/systemd-bootx64.efi"
		log.Printf(">>> systemd-boot from host (image ships none)")
	} else {
		log.Fatalf("no bootable EFI loader in image or on host (install systemd-boot-efi): %v", chainErr)
	}

	// Blobs still needed after the EROFS pass; everything else is deleted
	// the moment the EROFS writer has consumed it (rolling disk peak).
	keepRefs := map[string]bool{}
	for _, kp := range []string{
		"usr/lib/modules/" + kver + "/vmlinuz",
		"usr/lib/modules/" + kver + "/initramfs.img",
		chain.SdBoot, chain.Shim, chain.Grub, chain.MokMgr,
	} {
		if kp == "" {
			continue
		}
		if n := root.Lookup(kp); n != nil {
			keepRefs[n.Ref] = true
		}
	}
	// Blob refcounts: skel copies (and any future tree surgery) may point
	// several nodes at one blob — it may only be deleted after its LAST
	// consumer, not its first.
	refCount := map[string]int{}
	root.Walk(func(_ string, n *oci.Node) error {
		if n.Type == oci.TypeFile && n.Ref != "" {
			refCount[n.Ref]++
		}
		return nil
	})
	erofsStore := &consumingStore{inner: store, keep: keepRefs, refs: refCount}

	// Live rootfs (EROFS; tbox-live mounts with -t auto).
	sfsName := envID + ".rootfs.sfs"
	sfsPath := filepath.Join(*workdir, sfsName)
	log.Printf(">>> authoring EROFS live root %s", sfsName)
	sf, err := os.Create(sfsPath)
	if err != nil {
		log.Fatal(err)
	}
	if err := purefs.WriteErofs(root, erofsStore, sf, 0); err != nil {
		log.Fatal(err)
	}
	if err := sf.Close(); err != nil {
		log.Fatal(err)
	}

	// (blobs were consumed during the EROFS pass; only keepRefs remain)

	// Kernel cmdline — same template as cmd/tacklebox's liveKernelCmdline.
	kargs := fmt.Sprintf(
		"root=tbox:CDLABEL=%s tacklebox.live.squashimg=%s"+
			" tacklebox.live.overlay.size=8192 enforcing=0"+
			" tacklebox.env=%s console=ttyS0,115200n8",
		*label, sfsName, envID,
	)
	kernelPath := "/images/pxeboot/" + envID + "/vmlinuz"
	initrdPath := "/images/pxeboot/" + envID + "/initrd.img"

	// Chain-specific boot files. bootDiskFiles is mirrored into the ISO
	// tree by every writer (streaming, WriteIso, xorriso); espExtras are
	// config files that live only inside efi.img.
	var espExtras []purefs.EspFile
	switch chain.Kind {
	case "sdboot":
		entry := fmt.Sprintf("title TunaOS %s (live)\nlinux %s\ninitrd %s\noptions %s\n",
			envID, kernelPath, initrdPath, kargs)
		bootDiskFiles = []bootDiskFile{{"/EFI/BOOT/BOOTX64.EFI", sdBootDisk}}
		espExtras = []purefs.EspFile{
			{Path: "/loader/loader.conf", Source: purefs.StringSource("timeout 3\n")},
			{Path: "/loader/entries/" + envID + ".conf", Source: purefs.StringSource(entry)},
		}
	case "grub2":
		cfg := purefs.LiveGrubCfg("TunaOS "+envID+" (live)", kernelPath, initrdPath, kargs)
		cfgPath := filepath.Join(*workdir, "grub.cfg")
		if err := os.WriteFile(cfgPath, []byte(cfg), 0o644); err != nil {
			log.Fatal(err)
		}
		// shim boots as BOOTX64.EFI and loads grubx64.efi from its own
		// directory; grub.cfg goes to every prefix a signed GRUB has been
		// observed to search (its own dir and the vendor dir).
		bootDiskFiles = []bootDiskFile{
			{"/EFI/BOOT/BOOTX64.EFI", root.Lookup(chain.Shim).Ref},
			{"/EFI/BOOT/grubx64.efi", root.Lookup(chain.Grub).Ref},
			{"/EFI/BOOT/grub.cfg", cfgPath},
		}
		if chain.MokMgr != "" {
			bootDiskFiles = append(bootDiskFiles, bootDiskFile{"/EFI/BOOT/mmx64.efi", root.Lookup(chain.MokMgr).Ref})
		}
		espExtras = []purefs.EspFile{
			{Path: "/EFI/" + chain.Vendor + "/grub.cfg", Source: purefs.FileSource(cfgPath)},
		}
	}

	log.Printf(">>> authoring ESP")
	espPath := filepath.Join(*workdir, "efi.img")
	espFiles := make([]purefs.EspFile, 0, len(bootDiskFiles)+len(espExtras)+2)
	for _, bf := range bootDiskFiles {
		espFiles = append(espFiles, purefs.EspFile{Path: bf.path, Source: purefs.FileSource(bf.disk)})
	}
	espFiles = append(espFiles, espExtras...)
	espFiles = append(espFiles,
		purefs.EspFile{Path: kernelPath, Source: blob("usr/lib/modules/" + kver + "/vmlinuz")},
		purefs.EspFile{Path: initrdPath, Source: initrdSource},
	)
	if err := purefs.WriteEsp(espPath, espFiles); err != nil {
		log.Fatal(err)
	}

	log.Printf(">>> authoring ISO")
	if !*useXorriso {
		// Pure streaming writer: no workspace, no staging copy, each
		// source read once — and the same code path the WASM builder uses.
		f, err := os.Create(*out)
		if err != nil {
			log.Fatal(err)
		}
		kn := root.Lookup("usr/lib/modules/" + kver + "/vmlinuz")
		// Every size here must come from the thing that is actually
		// streamed. WriteIso9660 computes the whole layout from declared
		// sizes up front and then streams each source once, so a size that
		// disagrees with its source is not caught until gigabytes are
		// already on disk:
		//
		//   initrd.img: declared 53383528 bytes, source yielded 54859624
		//
		// which is what this used to do. initrdSource is the overlay+stock
		// concatenation whenever --initrd is absent, but the size was
		// re-derived from the stock initramfs node — so it under-declared by
		// exactly the overlay segment (1,476,096 bytes). Two expressions for
		// one fact, free to drift, and the xz module fix drifted them.
		//
		// initrdOnDisk is by construction the file initrdSource reads in
		// both branches, so stat that and there is only one expression left.
		inputs := []purefs.IsoInput{
			{Path: "/EFI/efi.img", Size: mustStatSize(espPath), Source: purefs.FileSource(espPath)},
		}
		for _, bf := range bootDiskFiles {
			inputs = append(inputs, purefs.IsoInput{Path: bf.path, Size: mustStatSize(bf.disk), Source: purefs.FileSource(bf.disk)})
		}
		inputs = append(inputs,
			purefs.IsoInput{Path: kernelPath, Size: kn.Size, Source: blob("usr/lib/modules/" + kver + "/vmlinuz")},
			purefs.IsoInput{Path: initrdPath, Size: mustStatSize(initrdOnDisk), Source: initrdSource},
			purefs.IsoInput{Path: "/LiveOS/" + sfsName, Size: mustStatSize(sfsPath), Source: purefs.FileSource(sfsPath)},
		)
		if err := purefs.WriteIso9660(f, *label, inputs, "/EFI/efi.img"); err != nil {
			log.Fatal(err)
		}
		if err := f.Close(); err != nil {
			log.Fatal(err)
		}
		st, _ := os.Stat(*out)
		log.Printf(">>> done: %s (%.1f GB)", *out, float64(st.Size())/1e9)
		return
	}
	if *useXorriso {
		if err := assembleWithXorriso(*workdir, *out, *label, envID, espPath, sfsPath, sfsName, root, kver); err != nil {
			log.Fatal(err)
		}
		st, _ := os.Stat(*out)
		log.Printf(">>> done: %s (%.1f GB)", *out, float64(st.Size())/1e9)
		return
	}
	isoFiles := []purefs.IsoFile{
		{Path: "/EFI/efi.img", Source: purefs.FileSource(espPath)},
	}
	for _, bf := range bootDiskFiles {
		isoFiles = append(isoFiles, purefs.IsoFile{Path: bf.path, Source: purefs.FileSource(bf.disk)})
	}
	isoFiles = append(isoFiles,
		purefs.IsoFile{Path: kernelPath, Source: blob("usr/lib/modules/" + kver + "/vmlinuz")},
		purefs.IsoFile{Path: initrdPath, Source: initrdSource},
		purefs.IsoFile{Path: "/LiveOS/" + sfsName, Source: purefs.FileSource(sfsPath)},
	)
	if err := purefs.WriteIso(*out, *label, isoFiles, "/EFI/efi.img"); err != nil {
		log.Fatal(err)
	}
	st, _ := os.Stat(*out)
	log.Printf(">>> done: %s (%.1f GB)", *out, float64(st.Size())/1e9)
}

// consumingStore deletes DirStore blobs as they are read (Close), except
// the keep set — a rolling disk profile for the EROFS pass.
type consumingStore struct {
	inner *oci.DirStore
	keep  map[string]bool
	refs  map[string]int
}

func (c *consumingStore) Put(r io.Reader) (string, int64, error) { return c.inner.Put(r) }
func (c *consumingStore) Open(ref string) (io.ReadCloser, error) {
	rc, err := c.inner.Open(ref)
	if err != nil {
		return nil, err
	}
	if c.keep[ref] {
		return rc, nil
	}
	return &deleteOnClose{ReadCloser: rc, path: ref, store: c}, nil
}

type deleteOnClose struct {
	io.ReadCloser
	path  string
	store *consumingStore
}

func (d *deleteOnClose) Close() error {
	err := d.ReadCloser.Close()
	d.store.refs[d.path]--
	if d.store.refs[d.path] <= 0 {
		_ = os.Remove(d.path)
	}
	return err
}

// assembleWithXorriso lays out iso-root with hardlinks (zero copies) and
// runs xorriso with the exact production argument set from
// internal/target.IsoTarget.assembleIso. Native-only convenience; the
// browser path uses the pure writer.
func assembleWithXorriso(workdir, out, label, envID, espPath, sfsPath, sfsName string, root *oci.Node, kver string) error {
	isoRoot := filepath.Join(workdir, "iso-root")
	_ = os.RemoveAll(isoRoot)
	px := filepath.Join(isoRoot, "images", "pxeboot", envID)
	for _, d := range []string{filepath.Join(isoRoot, "EFI", "BOOT"), px, filepath.Join(isoRoot, "LiveOS")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return err
		}
	}
	place := func(dst string, srcRef string) error {
		if err := os.Link(srcRef, dst); err == nil {
			return nil
		}
		in, err := os.Open(srcRef)
		if err != nil {
			return err
		}
		defer in.Close()
		o, err := os.Create(dst)
		if err != nil {
			return err
		}
		defer o.Close()
		_, err = io.Copy(o, in)
		return err
	}
	if err := place(filepath.Join(isoRoot, "EFI", "efi.img"), espPath); err != nil {
		return err
	}
	for _, bf := range bootDiskFiles {
		if err := place(filepath.Join(isoRoot, filepath.FromSlash(strings.TrimPrefix(bf.path, "/"))), bf.disk); err != nil {
			return err
		}
	}
	if err := place(filepath.Join(px, "vmlinuz"), root.Lookup("usr/lib/modules/"+kver+"/vmlinuz").Ref); err != nil {
		return err
	}
	// initrd was placed into the ESP already; reuse the ESP staging source
	// is gone, so link from the workdir copy the caller made — the ESP file
	// itself is inside efi.img. Callers pass --initrd; find it next to the
	// workdir if present.
	if err := place(filepath.Join(px, "initrd.img"), initrdOnDisk); err != nil {
		return err
	}
	if err := place(filepath.Join(isoRoot, "LiveOS", sfsName), sfsPath); err != nil {
		return err
	}

	_ = os.Remove(out)
	// Same argument set as internal/target.IsoTarget.assembleIso (the
	// production xorriso call this fallback mirrors); routed through the
	// runner seam like the rest of the codebase, instead of exec.Command
	// directly, so it picks up flatpak-spawn wrapping and is unit-testable.
	args := []string{
		"-dev", "stdio:" + out,
		"-volid", label,
		"-rockridge", "on",
		"-joliet", "on",
		"-map", isoRoot, "/",
		"-boot_image", "any", "platform_id=0xef",
		"-boot_image", "any", "efi_path=EFI/efi.img",
		"-boot_image", "any", "part_like_isohybrid=on",
		"-commit",
	}
	if err := runner.Run("xorriso", args...); err != nil {
		return fmt.Errorf("xorriso: %w", err)
	}
	_ = os.RemoveAll(isoRoot)
	return nil
}
