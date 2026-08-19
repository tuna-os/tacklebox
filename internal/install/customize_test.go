package install

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tuna-os/tacklebox/internal/runner"
)

var errUncached = errors.New("no such image")

func writeScript(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestCustomizeCacheKeyStable(t *testing.T) {
	dir := t.TempDir()
	s := writeScript(t, dir, "a.sh", "echo hi\n")

	k1, err := customizeCacheKey("sha256:abc", []string{s})
	if err != nil {
		t.Fatal(err)
	}
	k2, err := customizeCacheKey("sha256:abc", []string{s})
	if err != nil {
		t.Fatal(err)
	}
	if k1 != k2 {
		t.Fatalf("same inputs produced different keys: %s vs %s", k1, k2)
	}
	if len(k1) != 16 {
		t.Fatalf("key length = %d, want 16", len(k1))
	}
}

func TestCustomizeCacheKeyVariesByContentAndImage(t *testing.T) {
	dir := t.TempDir()
	s := writeScript(t, dir, "a.sh", "echo hi\n")

	base, err := customizeCacheKey("sha256:abc", []string{s})
	if err != nil {
		t.Fatal(err)
	}

	otherImage, err := customizeCacheKey("sha256:def", []string{s})
	if err != nil {
		t.Fatal(err)
	}
	if otherImage == base {
		t.Fatal("different image IDs produced the same key")
	}

	writeScript(t, dir, "a.sh", "echo changed\n")
	changed, err := customizeCacheKey("sha256:abc", []string{s})
	if err != nil {
		t.Fatal(err)
	}
	if changed == base {
		t.Fatal("changed script content produced the same key")
	}
}

func TestCustomizeCacheKeyVariesByScriptOrder(t *testing.T) {
	dir := t.TempDir()
	a := writeScript(t, dir, "a.sh", "echo a\n")
	b := writeScript(t, dir, "b.sh", "echo b\n")

	ab, err := customizeCacheKey("sha256:abc", []string{a, b})
	if err != nil {
		t.Fatal(err)
	}
	ba, err := customizeCacheKey("sha256:abc", []string{b, a})
	if err != nil {
		t.Fatal(err)
	}
	if ab == ba {
		t.Fatal("script order should change the key (scripts run in order)")
	}
}

func TestCustomizeCacheKeyMissingScript(t *testing.T) {
	if _, err := customizeCacheKey("sha256:abc", []string{"/nonexistent/x.sh"}); err == nil {
		t.Fatal("expected error for missing script")
	}
}

func TestCustomizeLiveNoScriptsPassthrough(t *testing.T) {
	ref, err := CustomizeLive("ghcr.io/example/image:latest", nil)
	if err != nil {
		t.Fatal(err)
	}
	if ref != "ghcr.io/example/image:latest" {
		t.Fatalf("no-scripts case must return the original ref, got %s", ref)
	}
}

func TestCustomizeTimeoutSeconds(t *testing.T) {
	t.Setenv("TBOX_CUSTOMIZE_TIMEOUT", "")
	if got := customizeTimeoutSeconds(); got != 1800 {
		t.Fatalf("default = %d, want 1800", got)
	}
	t.Setenv("TBOX_CUSTOMIZE_TIMEOUT", "300")
	if got := customizeTimeoutSeconds(); got != 300 {
		t.Fatalf("override = %d, want 300", got)
	}
	t.Setenv("TBOX_CUSTOMIZE_TIMEOUT", "0")
	if got := customizeTimeoutSeconds(); got != 0 {
		t.Fatalf("0 must disable, got %d", got)
	}
	// Junk must keep the cap, not silently disable it.
	t.Setenv("TBOX_CUSTOMIZE_TIMEOUT", "soon")
	if got := customizeTimeoutSeconds(); got != 1800 {
		t.Fatalf("junk = %d, want the 1800 default", got)
	}
	t.Setenv("TBOX_CUSTOMIZE_TIMEOUT", "-5")
	if got := customizeTimeoutSeconds(); got != 1800 {
		t.Fatalf("negative = %d, want the 1800 default", got)
	}
}

// tuna-os/tunaOS#1772: a wedged customize script produced 87 minutes of
// silence ending in a bare job cancellation, because (a) quiet mode discarded
// the container's output, (b) nothing bounded the container, and (c) the two
// scripts were indistinguishable in the log. This pins all three fixes at the
// runner seams: the customize `podman run` must go through the STREAMED
// runner, carry --timeout, and its inner script must announce each script.
func TestCustomizeLiveStreamsWithTimeoutAndMarkers(t *testing.T) {
	dir := t.TempDir()
	s := writeScript(t, dir, "customize-live.sh", "echo hi\n")

	origOut, origRun, origStreamed := runner.OutputFn, runner.RunFn, runner.RunStreamedFn
	t.Cleanup(func() {
		runner.OutputFn, runner.RunFn, runner.RunStreamedFn = origOut, origRun, origStreamed
	})

	runner.OutputFn = func(name string, args ...string) ([]byte, error) {
		// podmanForImage image inspect — succeed on the first (user) probe.
		return []byte("sha256:testimageid\n"), nil
	}
	var plainCalls [][]string
	runner.RunFn = func(stdin io.Reader, name string, args ...string) error {
		plainCalls = append(plainCalls, append([]string{name}, args...))
		if len(args) > 0 && args[0] == "image" { // `image exists` → cache miss
			return errUncached
		}
		return nil
	}
	var streamed [][]string
	runner.RunStreamedFn = func(stdin io.Reader, name string, args ...string) error {
		streamed = append(streamed, append([]string{name}, args...))
		return nil
	}

	if _, err := CustomizeLive("ghcr.io/example/image:latest", []string{s}); err != nil {
		t.Fatal(err)
	}

	if len(streamed) != 2 {
		t.Fatalf("expected the customize run and its commit to stream, got %d streamed calls", len(streamed))
	}
	joined := strings.Join(streamed[0], " ")
	if !strings.Contains(joined, "--timeout 1800") {
		t.Fatalf("customize run must carry the default --timeout cap; args: %s", joined)
	}
	if !strings.Contains(strings.Join(streamed[1], " "), "commit") {
		t.Fatalf("the customize commit must stream too; args: %s", strings.Join(streamed[1], " "))
	}
	inner := streamed[0][len(streamed[0])-1]
	// baseline.sh is prepended, so two scripts and two markers.
	if !strings.Contains(inner, "(1/2) baseline.sh") || !strings.Contains(inner, "(2/2) customize-live.sh") {
		t.Fatalf("inner script must announce each script; got:\n%s", inner)
	}
	for _, c := range plainCalls {
		j := strings.Join(c, " ")
		if strings.Contains(j, " run ") {
			t.Fatalf("the customize `podman run` went through the quiet runner: %s", j)
		}
	}
}

// Regression: the key hashed only the named scripts, but the whole directory
// is mounted and customize-live.sh sources desktop-<flavor>.sh out of it. So
// editing an adapter left the tag unchanged, the build reported
// "derived image cache hit", and shipped the previous live payload while
// looking successful — the ISO silently did not contain the change.
func TestCustomizeCacheKeyVariesBySourcedSibling(t *testing.T) {
	dir := t.TempDir()
	main := filepath.Join(dir, "customize-live.sh")
	sibling := filepath.Join(dir, "desktop-cosmic.sh")
	if err := os.WriteFile(main, []byte("source ./desktop-cosmic.sh\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sibling, []byte("OnlyShowIn=COSMIC;\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	before, err := customizeCacheKey("sha256:img", []string{main})
	if err != nil {
		t.Fatal(err)
	}

	// Edit ONLY the sibling — the named script is untouched.
	if err := os.WriteFile(sibling, []byte("# no OnlyShowIn\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	after, err := customizeCacheKey("sha256:img", []string{main})
	if err != nil {
		t.Fatal(err)
	}

	if before == after {
		t.Fatalf("editing a sourced sibling must change the cache key; got %q both times", before)
	}
}
