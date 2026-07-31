package install

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestInitramfsCacheKey(t *testing.T) {
	a := initramfsCacheKey("sha256:aaa", IsoInitramfsModules)
	b := initramfsCacheKey("sha256:bbb", IsoInitramfsModules)
	c := initramfsCacheKey("sha256:aaa", BlockInitramfsModules)

	if len(a) != 16 {
		t.Errorf("key length = %d, want 16", len(a))
	}
	if a == b {
		t.Error("different image IDs must produce different keys")
	}
	if a == c {
		t.Error("different module sets must produce different keys (ISO vs block initramfs)")
	}
	if a != initramfsCacheKey("sha256:aaa", IsoInitramfsModules) {
		t.Error("key must be stable for the same inputs")
	}
	if embeddedModulesDigest() == "" {
		t.Error("embedded modules digest must not be empty — a stale digest would let cached initramfses survive module changes")
	}
}

func TestInitramfsScript(t *testing.T) {
	s := initramfsScript(IsoInitramfsModules)
	for _, want := range []string{
		`dracut --force --no-hostonly --reproducible --add "tbox-live tbox-root"`,
		"for m in tbox-live tbox-root; do",
		"/tbox-out/initramfs.img",
		"TBOX_INITRAMFS=",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("script missing %q:\n%s", want, s)
		}
	}
	// The Sprintf %%s escape must come out as a literal %s for printf.
	if !strings.Contains(s, `printf '%s\n'`) {
		t.Errorf("script printf format mangled:\n%s", s)
	}
}

// TestInitramfsScriptOmitsOnlyMissingModules pins the per-image omit list.
// A baked-in `--omit "tpm2-tss pcsc"` breaks Fedora/CentOS images, which
// ship both modules and force-add clevis / systemd-cryptsetup on top of
// them: the omit makes those dependents "cannot be installed" and dracut
// exits 1. Dropping the omit entirely breaks Gentoo images, which ship
// neither. The probe is what keeps both working.
func TestInitramfsScriptOmitsOnlyMissingModules(t *testing.T) {
	s := initramfsScript(IsoInitramfsModules)
	if strings.Contains(s, `--omit "tpm2-tss pcsc"`) {
		t.Errorf("omit list must be probed per image, not baked in:\n%s", s)
	}
	for _, want := range []string{
		"for m in tpm2-tss pcsc; do",
		`/usr/lib/dracut/modules.d/[0-9][0-9]"$m"`,
		`if [ -n "$omit" ]; then run_dracut --omit "$omit"; else run_dracut; fi`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("script missing %q:\n%s", want, s)
		}
	}
}

// TestInitramfsScriptIsValidShell catches quoting or Sprintf-escape damage
// in the generated script before it reaches a container, where the only
// symptom would be a build failing deep inside podman run.
func TestInitramfsScriptIsValidShell(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh on PATH")
	}
	for name, mods := range map[string][]string{
		"iso":   IsoInitramfsModules,
		"block": BlockInitramfsModules,
	} {
		cmd := exec.Command(sh, "-n")
		cmd.Stdin = strings.NewReader(initramfsScript(mods))
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Errorf("%s script is not valid shell: %v\n%s\n%s", name, err, out, initramfsScript(mods))
		}
	}
}

func TestMaterializeDracutModules(t *testing.T) {
	dirs, err := materializeDracutModules()
	if err != nil {
		t.Fatalf("materializeDracutModules: %v", err)
	}
	t.Cleanup(func() {
		for _, d := range dirs {
			_ = os.RemoveAll(d)
		}
	})

	wantFiles := map[string][]string{
		"tbox-root": {"module-setup.sh", "tbox-root-mount.sh", "tbox-root.service"},
		"tbox-live": {"module-setup.sh", "parse-tbox-live.sh", "tbox-live-root.sh", "tbox-live-generator.sh", "tbox-live-mount.sh"},
	}
	for name := range embeddedDracutModules {
		if _, ok := wantFiles[name]; !ok {
			t.Errorf("embedded module %s has no expectation here — add its files", name)
		}
	}
	for name, files := range wantFiles {
		dir, ok := dirs[name]
		if !ok {
			t.Errorf("module %s not materialized", name)
			continue
		}
		for _, f := range files {
			if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
				t.Errorf("embedded module file %s/%s not materialized: %v", name, f, err)
			}
		}
	}
}
