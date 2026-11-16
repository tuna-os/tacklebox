package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tuna-os/tacklebox/internal/recipe"
)

// mustMkdirAll and mustSymlink build the tampered media layouts below.
func mustMkdirAll(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0755); err != nil {
		t.Fatalf("mkdir %s: %v", path, err)
	}
}

func mustSymlink(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("symlink %s -> %s: %v", link, target, err)
	}
}

func wantSymlinkRefusal(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("expected the symbolic link to be refused, got nil error")
	}
	if !strings.Contains(err.Error(), "symbolic link") {
		t.Fatalf("error does not name the symbolic link: %v", err)
	}
}

func TestMediaSubpath(t *testing.T) {
	base := t.TempDir()
	outside := t.TempDir()
	mustMkdirAll(t, filepath.Join(base, "real", "etc"))
	if err := os.WriteFile(filepath.Join(base, "real", "etc", "file"), []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
	mustSymlink(t, outside, filepath.Join(base, "link"))
	mustSymlink(t, filepath.Join(outside, "target"), filepath.Join(base, "real", "etc", "filelink"))

	ok := []struct {
		name string
		rel  []string
	}{
		{"existing directories", []string{"real", "etc"}},
		{"existing regular file as last component", []string{"real", "etc", "file"}},
		{"missing tail below a real directory", []string{"real", "etc", "tacklebox", "recipe.json"}},
		{"nothing exists yet", []string{"new", "etc"}},
	}
	for _, c := range ok {
		got, err := mediaSubpath(base, c.rel...)
		if err != nil {
			t.Errorf("%s: unexpected error: %v", c.name, err)
			continue
		}
		if want := filepath.Join(append([]string{base}, c.rel...)...); got != want {
			t.Errorf("%s: got %q, want %q", c.name, got, want)
		}
	}

	refused := []struct {
		name string
		rel  []string
		want string
	}{
		{"symbolic link as a directory component", []string{"link", "etc"}, "symbolic link"},
		{"symbolic link as the last component", []string{"real", "etc", "filelink"}, "symbolic link"},
		{"dangling symbolic link still counts", []string{"real", "etc", "filelink", "x"}, "symbolic link"},
		{"regular file where a directory is needed", []string{"real", "etc", "file", "x"}, "not a directory"},
	}
	for _, c := range refused {
		_, err := mediaSubpath(base, c.rel...)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want one containing %q", c.name, err, c.want)
		}
	}
}

// A media whose tbox-install is a link lists another directory's children as
// its environments, and every later rm/mkdir on <id> lands there.
func TestInstalledEnvsRefusesSymlinkedInstallRoot(t *testing.T) {
	storeMount := t.TempDir()
	outside := t.TempDir()
	mustMkdirAll(t, filepath.Join(outside, "ssh"))
	mustSymlink(t, outside, filepath.Join(storeMount, "tbox-install"))

	_, err := installedEnvs(storeMount)
	wantSymlinkRefusal(t, err)
}

func TestReadPersistedRecipeRefusesSymlink(t *testing.T) {
	storeMount := t.TempDir()
	outside := t.TempDir()
	mustMkdirAll(t, filepath.Join(outside, "tacklebox"))
	data, _ := json.Marshal(recipe.MediaRecipe{MediaName: "not-on-the-media"})
	if err := os.WriteFile(filepath.Join(outside, "tacklebox", "recipe.json"), data, 0644); err != nil {
		t.Fatal(err)
	}
	mustMkdirAll(t, filepath.Join(storeMount, "tbox-install", "foo"))
	mustSymlink(t, outside, filepath.Join(storeMount, "tbox-install", "foo", "etc"))

	got, err := readPersistedRecipe(storeMount)
	wantSymlinkRefusal(t, err)
	if got != nil {
		t.Errorf("returned a recipe read through the link: %+v", got)
	}
}

// writeEnvRecipe runs `sudo mkdir -p` and `sudo cp` on the path. Both follow
// links, so a link anywhere below the env root must stop it before either
// command runs.
func TestWriteEnvRecipeRefusesSymlink(t *testing.T) {
	cases := []struct {
		name  string
		plant func(t *testing.T, envRoot, outside string)
	}{
		{"etc", func(t *testing.T, envRoot, outside string) {
			mustSymlink(t, outside, filepath.Join(envRoot, "etc"))
		}},
		{"etc/tacklebox", func(t *testing.T, envRoot, outside string) {
			mustMkdirAll(t, filepath.Join(envRoot, "etc"))
			mustSymlink(t, outside, filepath.Join(envRoot, "etc", "tacklebox"))
		}},
		{"etc/tacklebox/recipe.json", func(t *testing.T, envRoot, outside string) {
			mustMkdirAll(t, filepath.Join(envRoot, "etc", "tacklebox"))
			mustSymlink(t, filepath.Join(outside, "victim"), filepath.Join(envRoot, "etc", "tacklebox", "recipe.json"))
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := newMockRunner(t)
			envRoot := filepath.Join(t.TempDir(), "tbox-install", "alpha")
			mustMkdirAll(t, envRoot)
			c.plant(t, envRoot, t.TempDir())

			err := writeEnvRecipe(envRoot, recipe.MediaRecipe{MediaName: "m"})
			wantSymlinkRefusal(t, err)
			if len(m.calls) != 0 {
				t.Errorf("privileged commands ran before the refusal: %v", m.calls)
			}
		})
	}
}

func TestWriteEnvRecipeWritesBelowEnvRoot(t *testing.T) {
	m := newMockRunner(t)
	envRoot := filepath.Join(t.TempDir(), "tbox-install", "alpha")
	mustMkdirAll(t, envRoot)

	if err := writeEnvRecipe(envRoot, recipe.MediaRecipe{MediaName: "m"}); err != nil {
		t.Fatalf("writeEnvRecipe: %v", err)
	}
	dir := filepath.Join(envRoot, "etc", "tacklebox")
	if !m.anyCallContains("sudo mkdir -p " + dir) {
		t.Errorf("recipe directory not created; calls: %v", m.calls)
	}
	if !m.anyCallContains(filepath.Join(dir, "recipe.json")) {
		t.Errorf("recipe not copied into place; calls: %v", m.calls)
	}
}

// update and add clear and recreate tbox-install/<id>; remove clears it.
// Through a linked tbox-install that is `rm -rf` and `mkdir -p` on the host.
func TestUpdateEnvBootcRefusesSymlinkedInstallRoot(t *testing.T) {
	m := newMockRunner(t)
	storeMount := t.TempDir()
	outside := t.TempDir()
	mustMkdirAll(t, filepath.Join(outside, "aurora"))
	mustSymlink(t, outside, filepath.Join(storeMount, "tbox-install"))

	env := baseTestEnv("aurora")
	r := recipe.MediaRecipe{BootableEnvironments: []recipe.BootableEnvironment{env}}
	track := func(_ string, fn func() error) error { return fn() }

	err := updateEnvBootc(env, r, storeMount, t.TempDir(), track)
	wantSymlinkRefusal(t, err)
	if len(m.calls) != 0 {
		t.Errorf("commands ran before the refusal: %v", m.calls)
	}
}

func TestRemoveEnvRefusesSymlinkedInstallRoot(t *testing.T) {
	m := newMockRunner(t)
	storeMount := t.TempDir()
	outside := t.TempDir()
	mustMkdirAll(t, filepath.Join(outside, "ssh"))
	mustSymlink(t, outside, filepath.Join(storeMount, "tbox-install"))

	err := removeEnv(storeMount, t.TempDir(), "ssh")
	wantSymlinkRefusal(t, err)
	if len(m.calls) != 0 {
		t.Errorf("commands ran before the refusal: %v", m.calls)
	}
}
