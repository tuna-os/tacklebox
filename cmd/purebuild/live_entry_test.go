package main

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"
)

const pureTestDTB = "\xd0\x0d\xfe\xedSYNTH-PURE-DTB"

// buildPureISO runs the whole pure pipeline for tuna-os/x13s:gnome
// (env x13s-gnome) on platform with the synthetic image plus extraFiles,
// and returns the authored ISO.
func buildPureISO(t *testing.T, platform string, extraFiles map[string]string) []byte {
	t.Helper()
	files := map[string]string{
		"usr/lib/systemd/boot/efi/systemd-bootaa64.efi": "MZ-SYNTH-SDBOOT-AA64-EFI",
	}
	for k, v := range extraFiles {
		files[k] = v
	}
	srv, _, _, _, _ := startSyntheticRegistry(t, "tuna-os/x13s", "gnome", files)
	tmp := t.TempDir()
	initrd := filepath.Join(tmp, "initrd.img")
	writeTestFile(t, initrd, "SYNTH-TBOX-INITRD")
	var iso bytes.Buffer
	cfg := &pureBuildConfig{
		Image:    "tuna-os/x13s:gnome",
		Registry: srv.URL,
		Platform: platform,
		Label:    "X13S_LIVE",
		Initrd:   initrd,
		Workdir:  filepath.Join(tmp, "work"),
		Sink: NewSinkWriter(func(chunk []byte) error {
			_, err := iso.Write(chunk)
			return err
		}),
	}
	if err := runPureBuild(cfg); err != nil {
		t.Fatalf("runPureBuild: %v", err)
	}
	return iso.Bytes()
}

// liveEntry cuts the systemd-boot entry for env x13s-gnome out of the ISO
// (it sits verbatim in the FAT image).
func liveEntry(t *testing.T, iso []byte) string {
	t.Helper()
	start := bytes.Index(iso, []byte("title TunaOS x13s-gnome (live)\n"))
	if start < 0 {
		t.Fatal("no live BLS entry in the ISO")
	}
	rest := iso[start:]
	opts := bytes.Index(rest, []byte("\noptions "))
	if opts < 0 {
		t.Fatal("live BLS entry has no options line")
	}
	end := bytes.IndexByte(rest[opts+1:], '\n')
	return string(rest[:opts+1+end+1])
}

func TestPureLiveEntryCarriesDevicetree(t *testing.T) {
	iso := buildPureISO(t, "linux/arm64", map[string]string{
		"usr/lib/modules/6.12.0-tunaos/devicetree": pureTestDTB,
	})
	entry := liveEntry(t, iso)
	want := "initrd /images/pxeboot/x13s-gnome/initrd.img\ndevicetree /images/pxeboot/x13s-gnome/devicetree\noptions "
	if !strings.Contains(entry, want) {
		t.Errorf("entry should load the image's device tree after the initrd, want %q in:\n%s", want, entry)
	}
	// The live rootfs (EROFS, uncompressed) carries it once already; the
	// ESP and the ISO9660 tree beside the kernel add one each.
	if n := bytes.Count(iso, []byte(pureTestDTB)); n != 3 {
		t.Errorf("device tree bytes appear %d times in the ISO, want 3 (rootfs + ESP + ISO tree)", n)
	}
}

func TestPureLiveEntryWithoutDevicetreeIsUnchanged(t *testing.T) {
	iso := buildPureISO(t, "linux/amd64", nil)
	want := "title TunaOS x13s-gnome (live)\n" +
		"linux /images/pxeboot/x13s-gnome/vmlinuz\n" +
		"initrd /images/pxeboot/x13s-gnome/initrd.img\n" +
		"options root=tbox:CDLABEL=X13S_LIVE tacklebox.live.squashimg=x13s-gnome.rootfs.sfs" +
		" tacklebox.live.overlay.size=8192 enforcing=0 tacklebox.env=x13s-gnome console=ttyS0,115200n8\n"
	if got := liveEntry(t, iso); got != want {
		t.Errorf("entry for an image without device tree or kargs.d changed:\n got: %q\nwant: %q", got, want)
	}
}

func TestPureLiveEntryKargsDByPlatform(t *testing.T) {
	kargsD := map[string]string{
		"usr/lib/bootc/kargs.d/10-base.toml": "kargs = [\"quiet\"]\n",
		"usr/lib/bootc/kargs.d/20-x13s.toml": "kargs = [\"clk_ignore_unused\", \"arm64.nopauth\"]\nmatch-architectures = [\"aarch64\"]\n",
	}
	for _, tc := range []struct{ platform, want string }{
		{"linux/arm64", " console=ttyS0,115200n8 quiet clk_ignore_unused arm64.nopauth\n"},
		{"linux/amd64", " console=ttyS0,115200n8 quiet\n"},
	} {
		t.Run(tc.platform, func(t *testing.T) {
			entry := liveEntry(t, buildPureISO(t, tc.platform, kargsD))
			if !strings.HasSuffix(entry, tc.want) {
				t.Errorf("options should end with the image's kargs.d for %s (%q):\n%s", tc.platform, tc.want, entry)
			}
		})
	}
}
