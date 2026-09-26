package install

import (
	"os/exec"
	"strings"
	"testing"
)

func TestShellEsc_Basic(t *testing.T) {
	cases := []struct {
		input string
		want  string
	}{
		{"hello", "'hello'"},
		{"", "''"},
		{"it's working", "'it'\"'\"'s working'"},
		{"a'b'c", "'a'\"'\"'b'\"'\"'c'"},
		{`no"quotes`, "'no\"quotes'"},
		{"spaces and $pecial", "'spaces and $pecial'"},
		{"new\nline", "'new\nline'"},
		{"tab\there", "'tab\there'"},
		{"back\\slash", "'back\\slash'"},
		{"semi;colon", "'semi;colon'"},
		{"pipe|char", "'pipe|char'"},
		{"$(whoami)", "'$(whoami)'"},
		{"`whoami`", "'`whoami`'"},
		{"${HOME}", "'${HOME}'"},
	}

	for _, tc := range cases {
		t.Run(tc.input, func(t *testing.T) {
			got := shellEsc(tc.input)
			if got != tc.want {
				t.Errorf("shellEsc(%q) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}

func TestShellEsc_PreventsInjection(t *testing.T) {
	dangerous := []string{
		"; rm -rf /",
		"$(rm -rf /)",
		"`rm -rf /`",
		"'; rm -rf /; echo '",
		"\\'; rm -rf /; echo '",
	}

	for _, payload := range dangerous {
		escaped := shellEsc(payload)
		if !strings.HasPrefix(escaped, "'") || !strings.HasSuffix(escaped, "'") {
			t.Errorf("shellEsc(%q) = %q — not wrapped in quotes", payload, escaped)
		}
		inner := escaped[1 : len(escaped)-1]
		if strings.Contains(inner, "'") {
			if !strings.Contains(escaped, `'"'"'`) && strings.Count(escaped, "'") > 2 {
				t.Errorf("shellEsc(%q) = %q — single quote not escaped", payload, escaped)
			}
		}
	}
}

func TestShellEsc_Idempotent(t *testing.T) {
	safe := "simple-image-name"
	once := shellEsc(safe)
	twice := shellEsc(once)
	if !strings.HasPrefix(twice, "'") || !strings.HasSuffix(twice, "'") {
		t.Errorf("double shellEsc(%q) = %q — not wrapped", safe, twice)
	}
}

func TestSquashParams(t *testing.T) {
	t.Setenv("SUPERISO_COMPRESSION", "")

	if level, block := squashParams(""); level != "3" || block != "131072" {
		t.Errorf("default: got level=%s block=%s, want 3/131072", level, block)
	}
	for _, c := range []string{"release", "max"} {
		if level, block := squashParams(c); level != "15" || block != "1048576" {
			t.Errorf("compression=%s: got level=%s block=%s, want 15/1048576", c, level, block)
		}
	}
	if level, _ := squashParams("zstd"); level != "3" {
		t.Errorf("compression=zstd should use fast default, got level=%s", level)
	}

	t.Setenv("SUPERISO_COMPRESSION", "release")
	if level, _ := squashParams(""); level != "15" {
		t.Errorf("SUPERISO_COMPRESSION=release should win, got level=%s", level)
	}
}

func TestSquashCacheName(t *testing.T) {
	a := squashCacheName([]string{"bluefin=sha1", "bazzite=sha2"}, "3", "131072")
	b := squashCacheName([]string{"bazzite=sha2", "bluefin=sha1"}, "3", "131072")
	if a != b {
		t.Error("cache name must be independent of env declaration order")
	}
	if a == squashCacheName([]string{"bluefin=sha1", "bazzite=sha2"}, "15", "1048576") {
		t.Error("compression settings must change the cache name")
	}
	if a == squashCacheName([]string{"bluefin=sha9", "bazzite=sha2"}, "3", "131072") {
		t.Error("an image ID change must change the cache name")
	}
}

func TestSquashCompArgs(t *testing.T) {
	t.Setenv("TACKLEBOX_SQUASHFS_COMPRESSOR", "")
	t.Cleanup(func() { _ = SetSquashCompressor("") })

	for _, tc := range []struct {
		compressor, level, want string
	}{
		{"", "3", "-comp zstd -Xcompression-level 3"},
		{"zstd", "15", "-comp zstd -Xcompression-level 15"},
		{"xz", "3", "-comp xz"},
		{"xz", "15", "-comp xz"},
		{"gzip", "3", "-comp gzip -Xcompression-level 6"},
		{"gzip", "15", "-comp gzip -Xcompression-level 9"},
		{"lz4", "3", "-comp lz4"},
		{"lz4", "15", "-comp lz4 -Xhc"},
		{"lzo", "3", "-comp lzo"},
	} {
		if err := SetSquashCompressor(tc.compressor); err != nil {
			t.Fatalf("SetSquashCompressor(%q): %v", tc.compressor, err)
		}
		if got := squashCompArgs(tc.level); got != tc.want {
			t.Errorf("compressor=%q level=%s: got %q, want %q", tc.compressor, tc.level, got, tc.want)
		}
	}

	if err := SetSquashCompressor("zst"); err == nil {
		t.Error("unknown compressor must be rejected")
	}

	// The env var overrides the recipe.
	_ = SetSquashCompressor("zstd")
	t.Setenv("TACKLEBOX_SQUASHFS_COMPRESSOR", "xz")
	if got := squashCompArgs("3"); got != "-comp xz" {
		t.Errorf("env override: got %q", got)
	}
}

// Changing the compressor must change the cache key, or a cached zstd
// squashfs would be reused for a recipe that asked for xz. The default
// (zstd) key must stay what it was so existing caches keep hitting.
func TestSquashCacheName_Compressor(t *testing.T) {
	t.Setenv("TACKLEBOX_SQUASHFS_COMPRESSOR", "")
	t.Cleanup(func() { _ = SetSquashCompressor("") })

	_ = SetSquashCompressor("")
	def := squashCacheName([]string{"sha1"}, "3", "131072")
	_ = SetSquashCompressor("zstd")
	if squashCacheName([]string{"sha1"}, "3", "131072") != def {
		t.Error("explicit zstd must match the default cache key")
	}
	_ = SetSquashCompressor("xz")
	if squashCacheName([]string{"sha1"}, "3", "131072") == def {
		t.Error("xz must not share the zstd cache key")
	}
}

func TestCombinedSquashScript_Compressor(t *testing.T) {
	t.Setenv("TACKLEBOX_SQUASHFS_COMPRESSOR", "")
	t.Cleanup(func() { _ = SetSquashCompressor("") })
	_ = SetSquashCompressor("xz")

	s := combinedSquashScript([]LiveEnv{{ID: "a", Image: "img"}}, "/usr/bin/mksquashfs", "/tmp/out.sfs", "3", "131072")
	if !strings.Contains(s, "-noappend -comp xz -b 131072") {
		t.Errorf("want xz flags in script:\n%s", s)
	}
	if strings.Contains(s, "zstd") {
		t.Errorf("xz script still mentions zstd:\n%s", s)
	}
}

func TestCombinedSquashScript(t *testing.T) {
	envs := []LiveEnv{
		{ID: "bluefin", Image: "ghcr.io/ublue-os/bluefin:stable"},
		{ID: "bazzite", Image: "ghcr.io/ublue-os/bazzite:stable"},
	}
	s := combinedSquashScript(envs, "/usr/bin/mksquashfs", "/tmp/out.sfs", "3", "131072")

	for _, want := range []string{
		"podman image mount 'ghcr.io/ublue-os/bluefin:stable'",
		"podman image mount 'ghcr.io/ublue-os/bazzite:stable'",
		`mkdir -p "$STAGE"/'bluefin'`,
		`mount --rbind "$M" "$STAGE"/'bazzite'`,
		"-e 'bluefin/proc'",
		"'bazzite/var/lib/containers/storage'",
		"-comp zstd -Xcompression-level 3 -b 131072",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("script missing %q:\n%s", want, s)
		}
	}
	if got := strings.Count(s, "/usr/bin/mksquashfs"); got != 1 {
		t.Errorf("want exactly 1 mksquashfs invocation, got %d", got)
	}
	if got := strings.Count(s, "podman image unmount"); got != 2 {
		t.Errorf("want 2 unmounts in trap, got %d", got)
	}
}

// The extract script must never pick a +debug kernel when a regular one
// exists, and must fail with an actionable message when the chosen kernel
// lacks an initramfs (tuna-os/tacklebox#86 item 3). Exercised with a fake
// modules tree via sh, no containers needed.
func TestExtractScriptKernelSelection(t *testing.T) {
	run := func(t *testing.T, setup string) (string, error) {
		t.Helper()
		root := t.TempDir()
		dest := t.TempDir()
		script := "set -eu\nROOT=" + root + "\nDEST=" + dest + "\n" + setup + "\n" +
			strings.ReplaceAll(strings.ReplaceAll(extractScript, "/usr/lib/modules", root), "/dest", dest)
		out, err := exec.Command("sh", "-c", script).CombinedOutput()
		return string(out), err
	}
	mk := `mkdir -p "$ROOT/$1"; touch "$ROOT/$1/modules.dep" "$ROOT/$1/vmlinuz"; [ "$2" = initrd ] && touch "$ROOT/$1/initramfs.img" || true`

	t.Run("prefers non-debug with initramfs", func(t *testing.T) {
		setup := `mkkernel() { ` + mk + `; }
mkkernel "6.12.0-1.el10.x86_64+debug" noinitrd
mkkernel "6.12.0-1.el10.x86_64" initrd`
		out, err := run(t, setup)
		if err != nil {
			t.Fatalf("expected success, got %v: %s", err, out)
		}
		if !strings.Contains(out, "KVER=6.12.0-1.el10.x86_64") || strings.Contains(out, "+debug") {
			t.Fatalf("picked wrong kernel: %s", out)
		}
	})

	t.Run("clear error when only a debug kernel without initramfs exists", func(t *testing.T) {
		setup := `mkkernel() { ` + mk + `; }
mkkernel "6.12.0-1.el10.x86_64+debug" noinitrd`
		out, err := run(t, setup)
		if err == nil {
			t.Fatalf("expected failure, got success: %s", out)
		}
		if !strings.Contains(out, "no initramfs.img") || !strings.Contains(out, "dracut") {
			t.Fatalf("error not actionable: %s", out)
		}
	})
}
