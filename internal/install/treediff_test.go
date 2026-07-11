package install

import (
	"os"
	"path/filepath"
	"sort"
	"testing"
)

// stubWhiteouts redirects mkWhiteout (mknod needs root or a userns) to
// plain marker files and records every whiteout path relative to out.
func stubWhiteouts(t *testing.T, out string) *[]string {
	t.Helper()
	var got []string
	old := mkWhiteout
	mkWhiteout = func(path string) error {
		rel, err := filepath.Rel(out, path)
		if err != nil {
			return err
		}
		got = append(got, rel)
		return os.WriteFile(path, nil, 0000)
	}
	t.Cleanup(func() { mkWhiteout = old })
	return &got
}

// mkTree materializes files ("path" -> content), dirs (trailing slash),
// and symlinks ("path" -> "->target").
func mkTree(t *testing.T, root string, entries map[string]string) {
	t.Helper()
	// Dirs first so files can nest under explicitly-listed dirs.
	keys := make([]string, 0, len(entries))
	for k := range entries {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, p := range keys {
		v := entries[p]
		full := filepath.Join(root, p)
		switch {
		case p[len(p)-1] == '/':
			if err := os.MkdirAll(full, 0755); err != nil {
				t.Fatal(err)
			}
		case len(v) > 2 && v[:2] == "->":
			if err := os.MkdirAll(filepath.Dir(full), 0755); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(v[2:], full); err != nil {
				t.Fatal(err)
			}
		default:
			if err := os.MkdirAll(filepath.Dir(full), 0755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(full, []byte(v), 0644); err != nil {
				t.Fatal(err)
			}
		}
	}
}

// listTree returns every path under root (files, dirs, symlinks),
// relative, sorted, excluding root itself.
func listTree(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	err := filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if p == root {
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		out = append(out, rel)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(out)
	return out
}

func runDiff(t *testing.T, base, env map[string]string, exclude []string) (string, *[]string) {
	t.Helper()
	baseDir, envDir, outDir := t.TempDir(), t.TempDir(), filepath.Join(t.TempDir(), "delta")
	mkTree(t, baseDir, base)
	mkTree(t, envDir, env)
	wh := stubWhiteouts(t, outDir)
	if err := TreeDiff(baseDir, envDir, outDir, exclude); err != nil {
		t.Fatalf("TreeDiff: %v", err)
	}
	return outDir, wh
}

func TestTreeDiffIdentical(t *testing.T) {
	tree := map[string]string{"etc/os-release": "ID=fedora\n", "usr/bin/": ""}
	out, wh := runDiff(t, tree, tree, nil)
	if got := listTree(t, out); len(got) != 0 {
		t.Errorf("identical trees must produce an empty delta, got %v", got)
	}
	if len(*wh) != 0 {
		t.Errorf("identical trees must produce no whiteouts, got %v", *wh)
	}
}

func TestTreeDiffAddModifyDelete(t *testing.T) {
	base := map[string]string{
		"etc/shared":       "same\n",
		"etc/changed":      "old\n",
		"etc/same-size":    "aaaa\n",
		"usr/bin/removed":  "gone\n",
		"usr/lib/kept":     "keep\n",
		"var/olddir/f1":    "x\n",
		"var/olddir/f2":    "y\n",
		"etc/became-link":  "regular\n",
		"srv/only-in-base": "bye\n",
	}
	env := map[string]string{
		"etc/shared":      "same\n",
		"etc/changed":     "new\n",
		"etc/same-size":   "bbbb\n", // same size, different bytes
		"usr/lib/kept":    "keep\n",
		"etc/new-file":    "hello\n",
		"opt/newdir/deep": "d\n",
		"etc/became-link": "->/usr/lib/kept",
		// Parent dirs of deletions still exist in the env, so the
		// whiteouts land on the deleted entries, not the dirs.
		"usr/bin/": "",
		"srv/":     "",
		"var/":     "",
	}
	out, wh := runDiff(t, base, env, nil)

	want := []string{
		"etc", "etc/became-link", "etc/changed", "etc/new-file", "etc/same-size",
		"opt", "opt/newdir", "opt/newdir/deep",
		"srv", "srv/only-in-base", // whiteout marker
		"usr", "usr/bin", "usr/bin/removed", // whiteout marker
		"var", "var/olddir", // whiteout marker for the whole dir
	}
	if got := listTree(t, out); !equalStrings(got, want) {
		t.Errorf("delta tree = %v, want %v", got, want)
	}

	wantWh := []string{"srv/only-in-base", "usr/bin/removed", "var/olddir"}
	sort.Strings(*wh)
	if !equalStrings(*wh, wantWh) {
		t.Errorf("whiteouts = %v, want %v", *wh, wantWh)
	}

	// A deleted subtree is a single whiteout on the dir, not per-child.
	if _, err := os.Lstat(filepath.Join(out, "var/olddir/f1")); err == nil {
		t.Error("children of a whiteouted dir must not appear in the delta")
	}
	// The type change carried the symlink over.
	if target, err := os.Readlink(filepath.Join(out, "etc/became-link")); err != nil || target != "/usr/lib/kept" {
		t.Errorf("became-link: target=%q err=%v", target, err)
	}
	// Same-size content change was caught by the byte compare.
	data, err := os.ReadFile(filepath.Join(out, "etc/same-size"))
	if err != nil || string(data) != "bbbb\n" {
		t.Errorf("same-size: %q err=%v", data, err)
	}
}

func TestTreeDiffModeOnlyChange(t *testing.T) {
	baseDir, envDir, outDir := t.TempDir(), t.TempDir(), filepath.Join(t.TempDir(), "delta")
	mkTree(t, baseDir, map[string]string{"usr/bin/tool": "#!/bin/sh\n"})
	mkTree(t, envDir, map[string]string{"usr/bin/tool": "#!/bin/sh\n"})
	if err := os.Chmod(filepath.Join(envDir, "usr/bin/tool"), 0755); err != nil {
		t.Fatal(err)
	}
	stubWhiteouts(t, outDir)
	if err := TreeDiff(baseDir, envDir, outDir, nil); err != nil {
		t.Fatalf("TreeDiff: %v", err)
	}
	info, err := os.Stat(filepath.Join(outDir, "usr/bin/tool"))
	if err != nil {
		t.Fatalf("mode-only change must copy the file: %v", err)
	}
	if info.Mode().Perm() != 0755 {
		t.Errorf("copied mode = %v, want 0755", info.Mode().Perm())
	}
}

func TestTreeDiffExcludes(t *testing.T) {
	base := map[string]string{"var/lib/containers/storage/junk": "a\n", "etc/f": "1\n"}
	// env keeps the parent dirs but has no storage subtree at all.
	env := map[string]string{"etc/f": "2\n", "var/lib/containers/": ""}
	out, wh := runDiff(t, base, env, []string{"var/lib/containers/storage"})
	if len(*wh) != 0 {
		t.Errorf("excluded subtree must not be whiteouted, got %v", *wh)
	}
	if _, err := os.Lstat(filepath.Join(out, "var")); err == nil {
		t.Error("nothing under an excluded path should reach the delta")
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
