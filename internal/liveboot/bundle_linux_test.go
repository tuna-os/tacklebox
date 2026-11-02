//go:build linux

package liveboot

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPrepareBytePreservingBundle(t *testing.T) {
	dir := t.TempDir()
	kernel := []byte("stock kernel")
	m := manifest(kernel)
	shim := syntheticPE(section{".text", []byte("shim")})
	grub := syntheticPE(section{".text", []byte("grub")})
	loader := syntheticPE(section{".linux", kernel}, section{".uname", []byte(m.KernelRelease)}, section{".dtbauto", dtb(Board)}, section{".hwids", hwids(Board)})
	receipt := []byte(`{"signature_verification":"caller supplied claim"}`)
	m.Shim = input("shim.efi", shim)
	m.GRUB = input("grub.efi", grub)
	m.Loader = input("loader.efi", loader)
	m.PreparationReceipt = input("receipt.json", receipt)
	for name, data := range map[string][]byte{m.Shim.Path: shim, m.GRUB.Path: grub, m.Loader.Path: loader, m.PreparationReceipt.Path: receipt} {
		if err := os.WriteFile(filepath.Join(dir, name), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	b, err := Prepare(dir, m, kernel, m.KernelRelease, fixtureScope("gnome-x13s"), []string{"root=tbox:CDLABEL=TUNA_X13S", "tacklebox.x13s-adsp=required"})
	if err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string][]byte{"EFI/BOOT/BOOTAA64.EFI": shim, "EFI/BOOT/grubaa64.efi": grub, "EFI/fedora/grubaa64.efi": grub, "images/pxeboot/gnome-x13s/vmlinuz": loader} {
		if !bytes.Equal(b.Files[name], want) {
			t.Fatalf("modified %s", name)
		}
	}
	if !bytes.Equal(b.PreparationReceipt, receipt) {
		t.Fatal("modified caller receipt")
	}
	cfg := string(b.Files["EFI/BOOT/grub.cfg"])
	if !strings.Contains(cfg, "linux /images/pxeboot/gnome-x13s/vmlinuz root=tbox:CDLABEL=TUNA_X13S") || !strings.Contains(cfg, "initrd /images/pxeboot/gnome-x13s/initrd.img") {
		t.Fatal(cfg)
	}
	for _, id := range []string{"../escape", "a/b", "", "${x}"} {
		if _, err := Prepare(dir, m, kernel, m.KernelRelease, fixtureScope(id), []string{"tacklebox.x13s-adsp=required"}); err == nil {
			t.Fatal("accepted unsafe ID")
		}
	}
	if _, err := Prepare(dir, m, kernel, m.KernelRelease, fixtureScope("gnome-x13s"), []string{"tacklebox.x13s-adsp=required", "x;reboot"}); err == nil {
		t.Fatal("accepted GRUB injection")
	}
	if err := os.WriteFile(filepath.Join(dir, m.Loader.Path), append(loader, 0), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Prepare(dir, m, kernel, m.KernelRelease, fixtureScope("gnome-x13s"), []string{"tacklebox.x13s-adsp=required"}); err == nil {
		t.Fatal("returned bundle after input changed")
	}
}

func fixtureScope(environment string) Scope {
	return Scope{ISO: true, Environments: 1, LiveOnly: true, Image: "ghcr.io/tuna-os/test@sha256:" + strings.Repeat("a", 64), Environment: environment}
}
