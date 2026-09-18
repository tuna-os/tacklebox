package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/ulikunitz/xz"

	tacklebox "github.com/tuna-os/tacklebox"
	"github.com/tuna-os/tacklebox/internal/purefs"
)

// buildFromDdi is the DDI input mode (tacklebox#172): instead of pulling
// and unpacking an OCI image, consume a systemd-sysupdate v1 artifact set
// (UKI + EROFS root partition) published by mkosi SplitArtifacts — e.g.
// frostyard/snosi's native A/B channels. The root partition is already
// EROFS, so the whole unpack+author pipeline is skipped: extract the
// UKI's kernel/initrd, prepend the tbox live scripts, and wrap.
//
// src is either an https URL of the artifact directory (containing
// SHA256SUMS) or a local directory laid out the same way — the local
// form is what CI uses to e2e this path offline.
func buildFromDdi(src, stem, label, workdir, out string) {
	if err := os.MkdirAll(workdir, 0o755); err != nil {
		log.Fatal(err)
	}
	fetch := newDdiFetcher(src)

	manifest, err := fetch.bytes("SHA256SUMS")
	if err != nil {
		log.Fatalf("SHA256SUMS: %v", err)
	}
	rel, err := purefs.ResolveDdiRelease(string(manifest), stem)
	if err != nil {
		log.Fatal(err)
	}
	log.Printf(">>> DDI release %s: uki=%s root=%s", rel.Version, rel.UKI, rel.Root)

	ukiBytes, err := fetch.bytes(rel.UKI)
	if err != nil {
		log.Fatal(err)
	}
	verifySha(rel.UKI, ukiBytes, rel.UKISHA)
	sections, err := purefs.ExtractUKI(ukiBytes)
	if err != nil {
		log.Fatal(err)
	}
	log.Printf(">>> UKI: kernel %d bytes, initrd %d bytes (baked cmdline: %q — replaced for live boot)",
		len(sections.Linux), len(sections.Initrd), sections.Cmdline)

	overlay, err := purefs.BuildInitrdOverlayScriptsOnly(tacklebox.DracutModules)
	if err != nil {
		log.Fatal(err)
	}
	initrd := append(overlay, sections.Initrd...)

	// Root partition: hash the artifact as published (compressed), then
	// land it decompressed — it is the live EROFS, verbatim.
	envID := "ddi-live"
	sfsName := envID + ".rootfs.sfs"
	sfsPath := filepath.Join(workdir, sfsName)
	if err := fetch.toFile(rel.Root, rel.RootSHA, sfsPath); err != nil {
		log.Fatal(err)
	}
	log.Printf(">>> root EROFS: %d bytes", mustStatSize(sfsPath))

	// No image tree, so no image-shipped loader: the host's systemd-boot
	// drives the extracted kernel/initrd via a BLS entry with live kargs.
	sdBoot := "/usr/lib/systemd/boot/efi/systemd-bootx64.efi"
	if _, err := os.Stat(sdBoot); err != nil {
		log.Fatal("DDI mode needs the host systemd-boot (install systemd-boot-efi): the artifact set ships only a UKI, whose baked cmdline cannot mount a live ISO root")
	}

	kargs := fmt.Sprintf(
		"root=tbox:CDLABEL=%s tacklebox.live.squashimg=%s"+
			" tacklebox.live.overlay.size=8192 enforcing=0"+
			" tacklebox.env=%s console=ttyS0,115200n8",
		label, sfsName, envID)
	kernelPath := "/images/pxeboot/" + envID + "/vmlinuz"
	initrdPath := "/images/pxeboot/" + envID + "/initrd.img"
	entry := fmt.Sprintf("title %s %s (live)\nlinux %s\ninitrd %s\noptions %s\n",
		label, rel.Version, kernelPath, initrdPath, kargs)

	kernelSrc := bytesFileSource(filepath.Join(workdir, "vmlinuz"), sections.Linux)
	initrdSrc := bytesFileSource(filepath.Join(workdir, "initrd-ddi.img"), initrd)

	log.Printf(">>> authoring ESP")
	espPath := filepath.Join(workdir, "efi.img")
	if err := purefs.WriteEsp(espPath, []purefs.EspFile{
		{Path: "/EFI/BOOT/BOOTX64.EFI", Source: purefs.FileSource(sdBoot)},
		{Path: "/loader/loader.conf", Source: purefs.StringSource("timeout 3\n")},
		{Path: "/loader/entries/" + envID + ".conf", Source: purefs.StringSource(entry)},
		{Path: kernelPath, Source: kernelSrc},
		{Path: initrdPath, Source: initrdSrc},
	}); err != nil {
		log.Fatal(err)
	}

	log.Printf(">>> authoring ISO")
	f, err := os.Create(out)
	if err != nil {
		log.Fatal(err)
	}
	inputs := []purefs.IsoInput{
		{Path: "/EFI/efi.img", Size: mustStatSize(espPath), Source: purefs.FileSource(espPath)},
		{Path: "/EFI/BOOT/BOOTX64.EFI", Size: mustStatSize(sdBoot), Source: purefs.FileSource(sdBoot)},
		{Path: kernelPath, Size: int64(len(sections.Linux)), Source: kernelSrc},
		{Path: initrdPath, Size: int64(len(initrd)), Source: initrdSrc},
		{Path: "/LiveOS/" + sfsName, Size: mustStatSize(sfsPath), Source: purefs.FileSource(sfsPath)},
	}
	if err := purefs.WriteIso9660(f, label, inputs, "/EFI/efi.img"); err != nil {
		log.Fatal(err)
	}
	if err := f.Close(); err != nil {
		log.Fatal(err)
	}
	st, _ := os.Stat(out)
	log.Printf(">>> done: %s (%.1f GB)", out, float64(st.Size())/1e9)
}

// bytesFileSource persists b at path once and serves it from disk — the
// ISO writer opens each source more than it streams it, and holding the
// kernel+initrd in memory per open would double peak usage for nothing.
func bytesFileSource(path string, b []byte) func() (io.ReadCloser, error) {
	if err := os.WriteFile(path, b, 0o644); err != nil {
		log.Fatal(err)
	}
	return purefs.FileSource(path)
}

func verifySha(name string, b []byte, want string) {
	if want == "" {
		return
	}
	got := sha256.Sum256(b)
	if hex.EncodeToString(got[:]) != strings.ToLower(want) {
		log.Fatalf("%s: sha256 mismatch (manifest %s, artifact %s)", name, want, hex.EncodeToString(got[:]))
	}
}

// ddiFetcher reads artifacts from an https base URL or a local directory.
type ddiFetcher struct{ base string }

func newDdiFetcher(base string) *ddiFetcher {
	return &ddiFetcher{base: strings.TrimRight(base, "/")}
}

func (f *ddiFetcher) open(name string) (io.ReadCloser, error) {
	// Plaintext is refused, not downgraded to a warning. SHA256SUMS travels
	// the same channel as the artifacts it covers and carries no signature,
	// so on http:// an attacker who can rewrite the artifacts rewrites the
	// digests to match and the check in ResolveDdiRelease passes on their
	// own numbers — for a kernel and root filesystem that go straight into
	// a bootable ISO.
	if strings.HasPrefix(f.base, "http://") {
		return nil, fmt.Errorf("DDI source %s is plaintext http:// — use https:// or a local directory path (the SHA256SUMS manifest is unsigned, so a plaintext fetch verifies nothing)", f.base)
	}
	if strings.HasPrefix(f.base, "https://") {
		resp, err := http.Get(f.base + "/" + name)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			return nil, fmt.Errorf("GET %s/%s: %s", f.base, name, resp.Status)
		}
		return resp.Body, nil
	}
	return os.Open(filepath.Join(f.base, name))
}

func (f *ddiFetcher) bytes(name string) ([]byte, error) {
	rc, err := f.open(name)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return io.ReadAll(rc)
}

// toFile streams an artifact to disk, verifying the manifest sha256 over
// the bytes as published (i.e. before decompression) and transparently
// xz-decompressing *.xz names.
func (f *ddiFetcher) toFile(name, wantSha, dst string) error {
	rc, err := f.open(name)
	if err != nil {
		return err
	}
	defer rc.Close()

	h := sha256.New()
	var body io.Reader = io.TeeReader(rc, h)
	if strings.HasSuffix(name, ".xz") {
		xr, err := xz.NewReader(body)
		if err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		body = xr
	}
	o, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(o, body); err != nil {
		o.Close()
		return fmt.Errorf("%s: %w", name, err)
	}
	if err := o.Close(); err != nil {
		return err
	}
	if wantSha != "" {
		if got := hex.EncodeToString(h.Sum(nil)); got != strings.ToLower(wantSha) {
			return fmt.Errorf("%s: sha256 mismatch (manifest %s, artifact %s)", name, wantSha, got)
		}
	}
	return nil
}
