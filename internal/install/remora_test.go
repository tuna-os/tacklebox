package install

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tuna-os/tacklebox/internal/runner"
)

func writeManifest(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func remoraKey(t *testing.T, m *RemoraManifest) string {
	t.Helper()
	identity, _, err := remoraManifestIdentity(m)
	if err != nil {
		t.Fatal(err)
	}
	return remoraCacheKey("sha256:abc", identity)
}

// A path manifest serializes to "{}" (Path is not a JSON field), so the key
// must come from the file's content: two manifests, or two revisions of one,
// against the same base image are different derived images.
func TestRemoraCacheKeyFollowsManifestFileContent(t *testing.T) {
	dir := t.TempDir()
	a := writeManifest(t, dir, "a.json", `{"packages":["vim"]}`)
	b := writeManifest(t, dir, "b.json", `{"remove":["openssh-server"]}`)

	keyA := remoraKey(t, &RemoraManifest{Path: a})
	if keyB := remoraKey(t, &RemoraManifest{Path: b}); keyA == keyB {
		t.Fatalf("different manifest files share key %s", keyA)
	}
	if again := remoraKey(t, &RemoraManifest{Path: a}); again != keyA {
		t.Fatalf("unchanged manifest file changed key: %s vs %s", keyA, again)
	}

	writeManifest(t, dir, "a.json", `{"packages":["vim"],"remove":["telnet"]}`)
	if edited := remoraKey(t, &RemoraManifest{Path: a}); edited == keyA {
		t.Fatalf("edited manifest file kept key %s", keyA)
	}
}

func TestRemoraCacheKeyVariesByURL(t *testing.T) {
	a := remoraKey(t, &RemoraManifest{Path: "https://example.com/a.json"})
	b := remoraKey(t, &RemoraManifest{Path: "https://example.com/b.json"})
	if a == b {
		t.Fatalf("different manifest URLs share key %s", a)
	}
}

// Inline manifests keep the key they had before path manifests were keyed by
// content, so derived images already in a store are still found.
func TestRemoraInlineIdentityIsManifestJSON(t *testing.T) {
	identity, reusable, err := remoraManifestIdentity(&RemoraManifest{Packages: []string{"vim"}})
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"packages":["vim"]}`; !bytes.Equal(identity, []byte(want)) {
		t.Fatalf("inline identity = %s, want %s", identity, want)
	}
	if !reusable {
		t.Fatal("inline manifest must be reusable")
	}
}

func TestRemoraManifestIdentityMissingFile(t *testing.T) {
	_, _, err := remoraManifestIdentity(&RemoraManifest{Path: filepath.Join(t.TempDir(), "gone.json")})
	if err == nil {
		t.Fatal("expected an error for a manifest file that cannot be read")
	}
}

// fakeRemoraPodman answers podman as a store that already holds every derived
// tag, and returns the podman subcommands RemoraCustomize ran.
func fakeRemoraPodman(t *testing.T) *[]string {
	t.Helper()
	t.Setenv("TACKLEBOX_CONTEXT", "root")
	origOut, origRun := runner.OutputFn, runner.RunFn
	t.Cleanup(func() { runner.OutputFn, runner.RunFn = origOut, origRun })

	var ran []string
	runner.OutputFn = func(name string, args ...string) ([]byte, error) {
		return []byte("sha256:abc\n"), nil
	}
	runner.RunFn = func(stdin io.Reader, name string, args ...string) error {
		ran = append(ran, strings.Join(args, " "))
		return nil
	}
	return &ran
}

func ranSubcommand(ran []string, sub string) bool {
	for _, c := range ran {
		if strings.HasPrefix(c, sub+" ") {
			return true
		}
	}
	return false
}

// The content behind a URL is fetched by remora inside the container and can
// change between builds, so an existing derived image is never reused for it.
func TestRemoraCustomizeAlwaysAppliesURLManifest(t *testing.T) {
	ran := fakeRemoraPodman(t)

	tag, err := RemoraCustomize("img", &RemoraManifest{Path: "https://example.com/remora.json"})
	if err != nil {
		t.Fatal(err)
	}
	if !ranSubcommand(*ran, "run") || !ranSubcommand(*ran, "commit") {
		t.Fatalf("URL manifest was not applied; podman ran: %q", *ran)
	}
	if !strings.HasPrefix(tag, "localhost/tbox-remora:") {
		t.Fatalf("tag = %q", tag)
	}
}

func TestRemoraCustomizeReusesImageForUnchangedFile(t *testing.T) {
	ran := fakeRemoraPodman(t)
	manifest := writeManifest(t, t.TempDir(), "m.json", `{"packages":["vim"]}`)

	if _, err := RemoraCustomize("img", &RemoraManifest{Path: manifest}); err != nil {
		t.Fatal(err)
	}
	if ranSubcommand(*ran, "run") {
		t.Fatalf("unchanged file manifest was re-applied; podman ran: %q", *ran)
	}
}
