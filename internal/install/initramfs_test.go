package install

import (
	"os"
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
