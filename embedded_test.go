package tacklebox

import (
	"io/fs"
	"testing"
	"testing/fstest"
)

// Every file each embedded dracut module must carry. A missing file here
// means PrepareInitramfs would bind-mount an incomplete module into the
// rebuild container and produce a broken initramfs.
var expectedModuleFiles = []string{
	"src/dracut/95tbox-root/module-setup.sh",
	"src/dracut/95tbox-root/tbox-root-mount.sh",
	"src/dracut/95tbox-root/tbox-root.service",
	"src/dracut/90tbox-live/module-setup.sh",
	"src/dracut/90tbox-live/parse-tbox-live.sh",
	"src/dracut/90tbox-live/tbox-live-root.sh",
	"src/dracut/90tbox-live/tbox-live-generator.sh",
	"src/dracut/90tbox-live/tbox-live-mount.sh",
}

func TestDracutModulesEmbedded(t *testing.T) {
	for _, f := range expectedModuleFiles {
		data, err := fs.ReadFile(DracutModules, f)
		if err != nil {
			t.Errorf("expected embedded file %s: %v", f, err)
		}
		if len(data) == 0 {
			t.Errorf("embedded file %s is empty", f)
		}
	}
}

func TestDracutModulesFSIsValid(t *testing.T) {
	// Run fstest.TestFS to validate the embed.FS implementation
	if err := fstest.TestFS(DracutModules, expectedModuleFiles...); err != nil {
		t.Errorf("embedded FS validation failed: %v", err)
	}
}
