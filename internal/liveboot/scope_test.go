package liveboot

import (
	"strings"
	"testing"
)

func TestBoundedLiveOnlyScope(t *testing.T) {
	valid := Scope{ISO: true, Environments: 1, LiveOnly: true, Image: "ghcr.io/tuna-os/gnome-x13s@sha256:" + strings.Repeat("a", 64), Environment: "gnome-x13s"}
	if err := ValidateScope(valid); err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]func(*Scope){
		"block": func(s *Scope) { s.BlockTarget = true }, "raw": func(s *Scope) { s.ISO = false }, "multiple": func(s *Scope) { s.Environments = 2 }, "persistent": func(s *Scope) { s.LiveOnly = false }, "dedup": func(s *Scope) { s.Dedup = true }, "offline": func(s *Scope) { s.OfflinePayloads = 1 }, "stock initrd": func(s *Scope) { s.SkipInitramfsRebuild = true }, "mutable image": func(s *Scope) { s.Image = "ghcr.io/tuna-os/gnome-x13s:latest" }, "unsafe ID": func(s *Scope) { s.Environment = "../x" },
	} {
		t.Run(name, func(t *testing.T) {
			s := valid
			change(&s)
			if err := ValidateScope(s); err == nil {
				t.Fatal("accepted unsupported target")
			}
		})
	}
}

func TestMandatoryGateArguments(t *testing.T) {
	if err := ValidateArguments([]string{"root=tbox:CDLABEL=TEST", "tacklebox.x13s-adsp=required"}); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{nil, {"tacklebox.x13s-adsp=off"}, {"tacklebox.x13s-adsp=required", "tacklebox.x13s-adsp=required"}, {"tacklebox.x13s-adsp=required", "modprobe.blacklist=qcom_q6v5_pas"}, {"tacklebox.x13s-adsp=required", "rd.driver.blacklist=qcom-q6v5-pas"}, {"tacklebox.x13s-adsp=required", "module_blacklist=qcom_q6v5_pas"}} {
		if err := ValidateArguments(args); err == nil {
			t.Fatalf("accepted unsafe args: %v", args)
		}
	}
}

func TestRejectFirmwareOverridesAndBootconfig(t *testing.T) {
	for _, arg := range []string{
		"firmware_class.path=/alternate", "firmware-class.path=/alternate", "firmware_class.path=", "firmware_class.path", "FIRMWARE_CLASS.path=/alternate",
		"bootconfig", "bootconfig=/alternate", "bootconfig=0", "boot-config", "boot_config", "--", "quiet firmware_class.path=/alternate", "\"firmware_class.path=/alternate\"",
	} {
		t.Run(arg, func(t *testing.T) {
			for _, args := range [][]string{{"tacklebox.x13s-adsp=required", arg}, {arg, "tacklebox.x13s-adsp=required"}} {
				if err := ValidateArguments(args); err == nil {
					t.Fatalf("accepted unsafe arguments %q", args)
				}
			}
		})
	}
	if err := ValidateArguments([]string{"tacklebox.x13s-adsp=required", "quiet", "root=tbox:CDLABEL=TEST"}); err != nil {
		t.Fatal(err)
	}
}
