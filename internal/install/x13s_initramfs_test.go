package install

// Tests for the X13s live initrd rebuild and the live safety script
// (tacklebox#388).
//
// Falsification: compress the initrd, drop --kver, accept a missing source
// observation, or let the safety rules match USB disks, and a test fails.

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestX13sInitramfsScript(t *testing.T) {
	s := x13sInitramfsScript("7.3.5-200.fc44.aarch64")
	for _, want := range []string{
		"kver='7.3.5-200.fc44.aarch64'",
		"--no-compress --no-early-microcode --nohardlink --nostrip --no-uefi",
		"--no-hostonly --no-hostonly-cmdline --reproducible",
		`--add "tbox-live tbox-root tbox-x13s-adsp" --kver "$kver"`,
		`SOURCE_DATE_EPOCH="$epoch"`,
		"DRACUT_ARCH=aarch64",
		"squash squash-lib squash-squashfs squash-erofs",
		"modinfo -k \"$kver\" -n qcom_q6v5_pas",
		"%{SOURCERPM}",
		"stat -c %s",
		"for name in qcadsp8280.mbn qccdsp8280.mbn qcslpi8280.mbn; do",
		"printf '%s\\n' $omit",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("script missing %q:\n%s", want, s)
		}
	}
	if strings.Contains(s, "%!") {
		t.Errorf("format verb leaked into the script:\n%s", s)
	}
	cmd := exec.Command("sh", "-n")
	cmd.Stdin = strings.NewReader(s)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("script does not parse: %v\n%s", err, out)
	}
}

func TestX13sADSPFirmwareMatchesDracutModule(t *testing.T) {
	data, err := os.ReadFile("../../src/dracut/70tbox-x13s-adsp/module-setup.sh")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "for name in "+strings.Join(x13sADSPFirmware, " ")+"; do") {
		t.Errorf("x13sADSPFirmware %v differs from the firmware 70tbox-x13s-adsp installs", x13sADSPFirmware)
	}
}

func TestPrepareX13sInitramfsRejectsUnsafeRelease(t *testing.T) {
	for _, kver := range []string{"", "7.3.5'; rm -rf /", "../7.3.5", "7.3 5"} {
		if _, err := PrepareX13sInitramfs("img", kver); err == nil {
			t.Errorf("kernel release %q accepted", kver)
		}
	}
}

const x13sTestKver = "7.3.5-200.fc44.aarch64"

func x13sTestOutput() string {
	sum := strings.Repeat("ab", 32)
	return strings.Join([]string{
		"noise from dracut",
		"TBOX_X13S_KCONFIG=" + sum,
		"TBOX_X13S_KPKG=kernel-7.3.5-200.fc44.src.rpm",
		"TBOX_X13S_EPOCH=1794930078",
		"TBOX_X13S_PIN=1000 " + sum + " usr/lib/modules/" + x13sTestKver + "/kernel/drivers/remoteproc/qcom_q6v5_pas.ko.xz",
		"TBOX_X13S_PIN=10 " + sum + " usr/lib/firmware/qcom/sc8280xp/LENOVO/21BX/qcadsp8280.mbn.xz",
		"TBOX_X13S_PIN=11 " + sum + " usr/lib/firmware/qcom/sc8280xp/LENOVO/21BX/qccdsp8280.mbn.xz",
		"TBOX_X13S_PIN=12 " + sum + " usr/lib/firmware/qcom/sc8280xp/LENOVO/21BX/qcslpi8280.mbn.xz",
		"TBOX_INITRAMFS=X13s profile",
	}, "\n")
}

func TestParseX13sInitramfsOutput(t *testing.T) {
	r, err := parseX13sInitramfsOutput(x13sTestOutput(), x13sTestKver)
	if err != nil {
		t.Fatal(err)
	}
	if r.KernelPackageBuild != "kernel-7.3.5-200.fc44.src.rpm" || r.SourceDateEpoch != 1794930078 || len(r.Sources) != 4 {
		t.Errorf("parsed %+v", r)
	}
	if r.Sources[1].Size != 10 || r.Sources[1].Path != "usr/lib/firmware/qcom/sc8280xp/LENOVO/21BX/qcadsp8280.mbn.xz" {
		t.Errorf("firmware pin = %+v", r.Sources[1])
	}

	lines := strings.Split(x13sTestOutput(), "\n")
	without := func(prefix string) string {
		var kept []string
		for _, l := range lines {
			if !strings.HasPrefix(l, prefix) {
				kept = append(kept, l)
			}
		}
		return strings.Join(kept, "\n")
	}
	for name, out := range map[string]string{
		"missing config":       without("TBOX_X13S_KCONFIG="),
		"missing package":      without("TBOX_X13S_KPKG="),
		"missing epoch":        without("TBOX_X13S_EPOCH="),
		"missing pins":         without("TBOX_X13S_PIN="),
		"repeated package":     x13sTestOutput() + "\nTBOX_X13S_KPKG=other.src.rpm",
		"negative epoch":       strings.Replace(x13sTestOutput(), "EPOCH=1794930078", "EPOCH=-1", 1),
		"malformed pin":        x13sTestOutput() + "\nTBOX_X13S_PIN=12 only-two",
		"module of other kver": strings.Replace(x13sTestOutput(), "modules/"+x13sTestKver, "modules/7.2.15-300.fc45.aarch64", 1),
	} {
		if _, err := parseX13sInitramfsOutput(out, x13sTestKver); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

// fakeBlockdevPath returns a PATH whose blockdev is a no-op, or no blockdev
// at all when present is false.
func fakeBlockdevPath(t *testing.T, present bool) string {
	t.Helper()
	bin := t.TempDir()
	for _, tool := range []string{"bash", "cat", "install", "env"} {
		real, err := exec.LookPath(tool)
		if err != nil {
			t.Skipf("%s not available: %v", tool, err)
		}
		if err := os.Symlink(real, filepath.Join(bin, tool)); err != nil {
			t.Fatal(err)
		}
	}
	if present {
		if err := os.WriteFile(filepath.Join(bin, "blockdev"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return bin
}

func TestX13sLiveSafetyScript(t *testing.T) {
	script, err := MaterializeX13sLiveSafety()
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(filepath.Dir(script))

	root := t.TempDir()
	bin := fakeBlockdevPath(t, true)
	cmd := exec.Command(filepath.Join(bin, "bash"), script)
	cmd.Env = []string{"PATH=" + bin, "TBOX_X13S_SAFETY_ROOT=" + root}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("safety script failed: %v\n%s", err, out)
	}
	rules, err := os.ReadFile(filepath.Join(root, "usr/lib/udev/rules.d/90-tbox-x13s-internal-disks.rules"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`KERNEL=="nvme*", ENV{UDISKS_IGNORE}="1"`,
		`RUN+="` + filepath.Join(bin, "blockdev") + ` --setro $devnode"`,
	} {
		if !strings.Contains(string(rules), want) {
			t.Errorf("rules missing %q:\n%s", want, rules)
		}
	}
	for _, line := range strings.Split(string(rules), "\n") {
		if strings.HasPrefix(line, "SUBSYSTEM") && !strings.Contains(line, `KERNEL=="nvme*"`) {
			t.Errorf("rule is not limited to internal nvme disks: %s", line)
		}
	}
	schema, err := os.ReadFile(filepath.Join(root, "usr/share/glib-2.0/schemas/90-tbox-x13s-live.gschema.override"))
	if err != nil || !strings.Contains(string(schema), "automount=false") {
		t.Errorf("GNOME automount override missing: %v\n%s", err, schema)
	}

	cmd = exec.Command(filepath.Join(bin, "bash"), script)
	cmd.Env = []string{"PATH=" + fakeBlockdevPath(t, false), "TBOX_X13S_SAFETY_ROOT=" + t.TempDir()}
	if out, err := cmd.CombinedOutput(); err == nil {
		t.Fatalf("safety script succeeded without blockdev:\n%s", out)
	}
}
