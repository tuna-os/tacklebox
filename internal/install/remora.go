package install

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/tuna-os/tacklebox/internal/runner"
)

// RemoraManifest is a resolved remora manifest ready for execution.
// When Path is non-empty it points to a host file (a remora manifest on disk
// or a URL remora can fetch). When Path is empty the inline fields carry the
// manifest content to be written to a temp file and mounted into the container.
type RemoraManifest struct {
	Path     string          `json:"-"`
	Packages []string        `json:"packages,omitempty"`
	Remove   []string        `json:"remove,omitempty"`
	Configs  []RemoraConfig  `json:"configs,omitempty"`
}

// RemoraConfig is one configuration file drop-in.
type RemoraConfig struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

// RemoraCustomize runs /usr/bin/remora apply inside a container of image
// with the given manifest, committing the result to a content-addressed
// derived image. Returns the derived image ref.
//
// The derived tag is keyed by the image ID plus the full manifest content,
// so changing packages/configs produces a new tag and reuses the existing
// one when nothing changed.
//
// remora runs as root with CAP_SYS_ADMIN and network (needed for package
// manager invocations: dnf install, apt-get, etc.).
//
// For file-based manifests (local paths or inline), the manifest is
// bind-mounted read-only at /tmp/remora-manifest.json. For URL manifests,
// the URL is passed directly to `remora apply` which handles fetching.
func RemoraCustomize(image string, manifest *RemoraManifest) (string, error) {
	if manifest == nil {
		return image, nil
	}

	prefix, id, err := podmanForImage(image)
	if err != nil {
		return "", err
	}

	// Serialize the manifest for cache keying (stable JSON).
	manifestJSON, err := json.Marshal(manifest)
	if err != nil {
		return "", fmt.Errorf("marshal remora manifest: %w", err)
	}

	key := remoraCacheKey(id, manifestJSON)
	tag := "localhost/tbox-remora:" + key

	existsArgs := append(prefix[1:], "image", "exists", tag)
	if err := runner.Run(prefix[0], existsArgs...); err == nil {
		fmt.Printf(">>> [remora] derived image cache hit for %s (%s)\n", image, tag)
		return tag, nil
	}

	// Resolve the manifest argument for remora: either a bind-mounted file
	// path inside the container, or a URL passed directly.
	manifestArg, cleanupManifest, runArgsExtra, err := remoraManifestArg(manifest)
	if err != nil {
		return "", err
	}
	defer cleanupManifest()

	ctr := "tbox-remora-" + key
	rmArgs := append(prefix[1:], "rm", "-f", "--ignore", ctr)
	_ = runner.Run(prefix[0], rmArgs...)

	runArgs := append(prefix[1:],
		"run", "--name", ctr,
		"--cap-add", "sys_admin",
		"--security-opt", "label=disable",
		"--log-driver", "k8s-file",
	)
	runArgs = append(runArgs, runArgsExtra...)
	// Honour the same network escape hatch as customize (netavark/firewalld
	// can break the default bridge on some hosts).
	if net := os.Getenv("TBOX_CUSTOMIZE_NETWORK"); net != "" {
		runArgs = append(runArgs, "--network", net)
	}
	runArgs = append(runArgs,
		"--entrypoint", "/usr/bin/remora",
		image, "apply", manifestArg,
	)

	fmt.Printf(">>> [remora] applying manifest (%d packages, %d removals, %d configs) against %s\n",
		len(manifest.Packages), len(manifest.Remove), len(manifest.Configs), image)
	fmt.Printf(">>> [remora] (remora will install packages: network required)\n")
	if err := runner.Run(prefix[0], runArgs...); err != nil {
		rmArgs := append(prefix[1:], "rm", "-f", "--ignore", ctr)
		_ = runner.Run(prefix[0], rmArgs...)
		return "", fmt.Errorf("remora apply %s: %w", image, err)
	}

	commitArgs := append(prefix[1:], "commit", "--quiet", ctr, tag)
	if err := runner.Run(prefix[0], commitArgs...); err != nil {
		return "", fmt.Errorf("commit remora-customized %s: %w", image, err)
	}
	rmArgs = append(prefix[1:], "rm", "-f", "--ignore", ctr)
	_ = runner.Run(prefix[0], rmArgs...)

	fmt.Printf(">>> [remora] committed %s\n", tag)
	return tag, nil
}

// remoraCacheKey derives a short hex tag from the image ID and the full
// manifest JSON, so changing any package or config produces a new tag.
func remoraCacheKey(imageID string, manifestJSON []byte) string {
	h := sha256.New()
	fmt.Fprintf(h, "%s\n", imageID)
	h.Write(manifestJSON)
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// remoraManifestArg returns the argument to pass to `remora apply` and any
// extra podman run flags needed (e.g. -v for bind-mounting).
//
// Three forms:
//   - URL (contains "://"): passed directly to remora (it fetches).
//   - Local file path: bind-mounted at /tmp/remora-manifest.json, arg is
//     the container path.
//   - Inline manifest: written to a temp file, bind-mounted, arg is the
//     container path.
//
// The returned cleanup function removes the temp file (no-op otherwise).
func remoraManifestArg(manifest *RemoraManifest) (arg string, cleanup func(), extraArgs []string, err error) {
	cleanup = func() {}
	if manifest.Path != "" {
		// URL: pass directly to remora.
		if strings.Contains(manifest.Path, "://") {
			return manifest.Path, cleanup, nil, nil
		}
		// Local file: resolve and bind-mount.
		abs, err := filepath.Abs(manifest.Path)
		if err != nil {
			return "", cleanup, nil, fmt.Errorf("resolve remora manifest path %s: %w", manifest.Path, err)
		}
		if _, err := os.Stat(abs); err != nil {
			return "", cleanup, nil, fmt.Errorf("remora manifest %s: %w", abs, err)
		}
		return "/tmp/remora-manifest.json", cleanup,
			[]string{"-v", abs + ":/tmp/remora-manifest.json:ro"}, nil
	}

	// Inline manifest: write to a temp file and bind-mount.
	tmp, err := os.CreateTemp("", "tbox-remora-*.json")
	if err != nil {
		return "", cleanup, nil, fmt.Errorf("create temp remora manifest: %w", err)
	}
	enc := json.NewEncoder(tmp)
	enc.SetIndent("", "  ")
	if err := enc.Encode(manifest); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return "", cleanup, nil, fmt.Errorf("write remora manifest: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return "", cleanup, nil, fmt.Errorf("close remora manifest: %w", err)
	}
	return "/tmp/remora-manifest.json",
		func() { os.Remove(tmp.Name()) },
		[]string{"-v", tmp.Name() + ":/tmp/remora-manifest.json:ro"}, nil
}
