package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	tacklebox "github.com/tuna-os/tacklebox"
	"github.com/tuna-os/tacklebox/internal/recipe"
)

// The units provisionUpdateSystem installs run as root at every boot of the
// media. They must be the ones built into the binary: a src/systemd/ below the
// working directory belongs to whoever owns that directory, which is the
// source checkout only when tacklebox runs from its repository root.
func TestProvisionUpdateSystemInstallsEmbeddedUnits(t *testing.T) {
	m := newMockRunner(t)

	cwd := t.TempDir()
	planted := filepath.Join(cwd, "src", "systemd")
	mustMkdirAll(t, planted)
	for _, f := range updateAllUnits {
		body := []byte("[Service]\nExecStart=/planted\n")
		if err := os.WriteFile(filepath.Join(planted, f), body, 0644); err != nil {
			t.Fatal(err)
		}
	}
	t.Chdir(cwd)

	envRoot := filepath.Join(t.TempDir(), "tbox-install", "alpha")
	mustMkdirAll(t, envRoot)
	if err := provisionUpdateSystem(envRoot, "alpha", recipe.MediaRecipe{MediaName: "m"}); err != nil {
		t.Fatalf("provisionUpdateSystem: %v", err)
	}

	unitDir := filepath.Join(envRoot, "etc", "systemd", "system")
	for _, f := range updateAllUnits {
		want, err := tacklebox.SystemdUnits.ReadFile("src/systemd/" + f)
		if err != nil {
			t.Fatalf("embedded unit %s: %v", f, err)
		}
		got, err := os.ReadFile(filepath.Join(unitDir, f))
		if err != nil {
			t.Fatalf("unit %s was not installed: %v", f, err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("installed %s is not the embedded unit:\n%s", f, got)
		}
		if !m.anyCallContains("sudo chmod 0644 " + filepath.Join(unitDir, f)) {
			t.Errorf("%s was not made 0644; calls: %v", f, m.callStrings())
		}
	}
	for _, c := range m.callStrings() {
		if strings.Contains(c, "src/systemd") {
			t.Errorf("a command read a unit from the working directory: %s", c)
		}
	}
}
