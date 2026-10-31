package tacklebox

import (
	"io/fs"
	"os/exec"
	"testing"
)

func TestX13sADSPAssetsEmbedded(t *testing.T) {
	for _, name := range []string{"tbox-x13s-adsp.sh", "tbox-x13s-adsp-pre-udev.sh"} {
		if data, err := fs.ReadFile(DracutModules, "src/dracut/90tbox-live/"+name); err != nil || len(data) == 0 {
			t.Fatalf("required X13s runtime asset %s: %v", name, err)
		}
	}
}

func TestX13sADSPBehavior(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Fatal("python3 is required for the isolated X13s shell behavior fixtures")
	}
	cmd := exec.Command(python, "scripts/test-x13s-adsp.py")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("X13s shell fixtures failed: %v\n%s", err, output)
	}
}
