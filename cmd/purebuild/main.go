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
	"os/exec"
	"path/filepath"
	"strings"

	tacklebox "github.com/tuna-os/tacklebox"
	"github.com/tuna-os/tacklebox/internal/oci"
	"github.com/tuna-os/tacklebox/internal/purefs"
)

var initrdOnDisk string
var sdBootDisk string

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
	)
	flag.Parse()
	if *image == "" || !strings.Contains(*image, ":") {
		log.Fatal("--image <repo>:<tag> is required")
	}
	repo := (*image)[:strings.LastIndex(*image, ":")]
	tag := (*image)[strings.LastIndex(*image, ":")+1:]
	envID := filepath.Base(repo) + "-" + tag

	if err := os.MkdirAll(*workdir, 0o755); err != nil {
		log.Fatal(err)
	}

	store := &oci.DirStore{Dir: filepath.Join(*workdir, "blobs")}
	var root *oci.Node
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
	// systemd-boot: prefer the image's own copy; EL10 images don't ship it,
	// so fall back to the host binary exactly like production
	// ExtractEFIBinary does (CI installs systemd-boot-efi).
	var sdBoot func() (io.ReadCloser, error)
	if n := root.Lookup("usr/lib/systemd/boot/efi/systemd-bootx64.efi"); n != nil && n.Type == oci.TypeFile {
		sdBoot = func() (io.ReadCloser, error) { return store.Open(n.Ref) }
		sdBootDisk = n.Ref
	} else if _, err := os.Stat("/usr/lib/systemd/boot/efi/systemd-bootx64.efi"); err == nil {
		sdBootDisk = "/usr/lib/systemd/boot/efi/systemd-bootx64.efi"
		sdBoot = purefs.FileSource(sdBootDisk)
		log.Printf(">>> systemd-boot from host (image ships none)")
	} else {
		log.Fatal("no systemd-boot EFI binary in image or on host (install systemd-boot-efi)")
	}

	// Blobs still needed after the EROFS pass; everything else is deleted
	// the moment the EROFS writer has consumed it (rolling disk peak).
	keepRefs := map[string]bool{}
	for _, kp := range []string{
		"usr/lib/modules/" + kver + "/vmlinuz",
		"usr/lib/modules/" + kver + "/initramfs.img",
		"usr/lib/systemd/boot/efi/systemd-bootx64.efi",
	} {
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

	// BLS entry — same template as cmd/tacklebox's liveKernelCmdline.
	kargs := fmt.Sprintf(
		"root=tbox:CDLABEL=%s tacklebox.live.squashimg=%s"+
			" tacklebox.live.overlay.size=8192 enforcing=0"+
			" tacklebox.env=%s console=ttyS0,115200n8",
		*label, sfsName, envID,
	)
	entry := fmt.Sprintf("title TunaOS %s (live)\nlinux /images/pxeboot/%s/vmlinuz\ninitrd /images/pxeboot/%s/initrd.img\noptions %s\n",
		envID, envID, envID, kargs)
	loaderConf := "timeout 3\n"

	kernelPath := "/images/pxeboot/" + envID + "/vmlinuz"
	initrdPath := "/images/pxeboot/" + envID + "/initrd.img"

	log.Printf(">>> authoring ESP")
	espPath := filepath.Join(*workdir, "efi.img")
	if err := purefs.WriteEsp(espPath, []purefs.EspFile{
		{Path: "/EFI/BOOT/BOOTX64.EFI", Source: sdBoot},
		{Path: "/loader/loader.conf", Source: purefs.StringSource(loaderConf)},
		{Path: "/loader/entries/" + envID + ".conf", Source: purefs.StringSource(entry)},
		{Path: kernelPath, Source: blob("usr/lib/modules/" + kver + "/vmlinuz")},
		{Path: initrdPath, Source: initrdSource},
	}); err != nil {
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
		espSt, _ := os.Stat(espPath)
		sfsSt, _ := os.Stat(sfsPath)
		kn := root.Lookup("usr/lib/modules/" + kver + "/vmlinuz")
		var initrdSize int64
		if *initrd != "" {
			ist, _ := os.Stat(*initrd)
			initrdSize = ist.Size()
		} else {
			initrdSize = root.Lookup("usr/lib/modules/" + kver + "/initramfs.img").Size
		}
		sdbSt, _ := os.Stat(sdBootDisk)
		inputs := []purefs.IsoInput{
			{Path: "/EFI/efi.img", Size: espSt.Size(), Source: purefs.FileSource(espPath)},
			{Path: "/EFI/BOOT/BOOTX64.EFI", Size: sdbSt.Size(), Source: purefs.FileSource(sdBootDisk)},
			{Path: kernelPath, Size: kn.Size, Source: blob("usr/lib/modules/" + kver + "/vmlinuz")},
			{Path: initrdPath, Size: initrdSize, Source: initrdSource},
			{Path: "/LiveOS/" + sfsName, Size: sfsSt.Size(), Source: purefs.FileSource(sfsPath)},
		}
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
	if err := purefs.WriteIso(*out, *label, []purefs.IsoFile{
		{Path: "/EFI/efi.img", Source: purefs.FileSource(espPath)},
		{Path: "/EFI/BOOT/BOOTX64.EFI", Source: sdBoot},
		{Path: kernelPath, Source: blob("usr/lib/modules/" + kver + "/vmlinuz")},
		{Path: initrdPath, Source: initrdSource},
		{Path: "/LiveOS/" + sfsName, Source: purefs.FileSource(sfsPath)},
	}, "/EFI/efi.img"); err != nil {
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
	if err := place(filepath.Join(isoRoot, "EFI", "BOOT", "BOOTX64.EFI"), sdBootDisk); err != nil {
		return err
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
	cmd := exec.Command("xorriso",
		"-dev", "stdio:"+out,
		"-volid", label,
		"-rockridge", "on",
		"-joliet", "on",
		"-map", isoRoot, "/",
		"-boot_image", "any", "platform_id=0xef",
		"-boot_image", "any", "efi_path=EFI/efi.img",
		"-boot_image", "any", "part_like_isohybrid=on",
		"-commit",
	)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("xorriso: %w", err)
	}
	_ = os.RemoveAll(isoRoot)
	return nil
}
