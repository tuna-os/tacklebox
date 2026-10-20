package liveboot

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestInitrdVirtualResolve(t *testing.T) {
	i := &InitrdInventory{validated: true, entries: map[string]InitrdEntry{
		"usr":            {Path: "usr", Type: InitrdDirectory},
		"usr/lib":        {Path: "usr/lib", Type: InitrdDirectory},
		"usr/lib/helper": {Path: "usr/lib/helper", Type: InitrdRegular, SHA256: "observed"},
		"lib":            {Path: "lib", Type: InitrdSymlink, LinkTarget: "usr/lib"},
		"absolute":       {Path: "absolute", Type: InitrdSymlink, LinkTarget: "/usr/lib"},
		"relative":       {Path: "relative", Type: InitrdSymlink, LinkTarget: "usr/../usr/lib"},
	}}
	for _, p := range []string{"/lib/helper", "absolute/helper", "relative/helper", "usr/lib/helper"} {
		got, err := i.Resolve(p)
		if err != nil || got.Path != "usr/lib/helper" {
			t.Fatalf("%s: %+v %v", p, got, err)
		}
	}
	for name, target := range map[string]string{"escape": "../usr/lib", "loop": "loop", "missing": "absent", "nul": "bad\x00path"} {
		i.entries[name] = InitrdEntry{Path: name, Type: InitrdSymlink, LinkTarget: target}
		if _, err := i.Resolve(name + "/helper"); err == nil {
			t.Errorf("accepted %s", name)
		}
	}
	for _, p := range []string{"../usr/lib/helper", "usr/lib/helper/child", "absent"} {
		if _, err := i.Resolve(p); err == nil {
			t.Errorf("accepted %s", p)
		}
	}
	i.validated = false
	if _, err := i.Resolve("lib/helper"); err == nil {
		t.Fatal("accepted unvalidated inventory")
	}
}

// Compare the virtual walk with the operating system without extracting an
// archive: these are independently constructed, small owned test fixtures.
func TestInitrdResolveWalksBeforeDotDot(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation requires Windows privilege")
	}
	root := t.TempDir()
	i := &InitrdInventory{validated: true, entries: map[string]InitrdEntry{}}
	for _, name := range []string{"dir", "other"} {
		if err := os.Mkdir(filepath.Join(root, name), 0700); err != nil {
			t.Fatal(err)
		}
		i.entries[name] = InitrdEntry{Path: name, Type: InitrdDirectory}
	}
	for name, data := range map[string]string{"dir/good": "wrong lexical result", "good": "runtime result", "dir/regular": "not a directory"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
		i.entries[name] = InitrdEntry{Path: name, Type: InitrdRegular}
	}
	for name, target := range map[string]string{"dir/link": "../other", "alias": "dir/link/../good", "missing": "dir/missing/../good", "nondirectory": "dir/regular/../good", "trailing": "dir/regular/", "dot": "dir/regular/."} {
		if err := os.Symlink(target, filepath.Join(root, name)); err != nil {
			t.Fatal(err)
		}
		i.entries[name] = InitrdEntry{Path: name, Type: InitrdSymlink, LinkTarget: target}
	}
	got, err := i.Resolve("alias")
	if err != nil || got.Path != "good" {
		t.Fatalf("resolved %+v: %v", got, err)
	}
	actual, err := os.ReadFile(filepath.Join(root, "alias"))
	if err != nil || string(actual) != "runtime result" {
		t.Fatalf("kernel pathwalk %q: %v", actual, err)
	}
	for _, name := range []string{"missing", "nondirectory", "trailing", "dot", "dir/regular/", "dir/regular/."} {
		if _, err := os.ReadFile(root + "/" + name); err == nil {
			t.Fatalf("kernel accepted %s", name)
		}
		if _, err := i.Resolve(name); err == nil {
			t.Fatalf("virtual walk accepted %s", name)
		}
	}
	// Absolute archive targets refer to its root, never the host root.
	i.entries["dir/link"] = InitrdEntry{Path: "dir/link", Type: InitrdSymlink, LinkTarget: "/other"}
	got, err = i.Resolve("alias")
	if err != nil || got.Path != "good" {
		t.Fatalf("absolute resolved %+v: %v", got, err)
	}
}
