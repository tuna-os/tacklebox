package install

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/tuna-os/tacklebox/internal/runner"
)

// CustomizeLive runs the recipe's live_customize scripts inside a container
// of image and commits the result to a content-addressed derived image,
// returning its ref. The derived ref is what gets squashed/extracted, so the
// original image is never mutated.
//
// The derived tag is keyed by the image ID plus every script's content —
// rerunning with unchanged inputs reuses the committed image (and therefore
// the downstream squashfs cache); any edit produces a new tag.
//
// Each script runs as root with CAP_SYS_ADMIN and network (the dakota-iso
// configure-live environment: enough for flatpak install, dbus-daemon,
// dconf update). Script i's directory is mounted read-only at
// /run/tbox-customize/<i> and is the script's working directory, so scripts
// can reference sibling assets relatively.
func CustomizeLive(image string, scripts []string) (string, error) {
	if len(scripts) == 0 {
		return image, nil
	}

	prefix, id, err := podmanForImage(image)
	if err != nil {
		return "", err
	}

	key, err := customizeCacheKey(id, scripts)
	if err != nil {
		return "", err
	}
	tag := "localhost/tbox-live-custom:" + key

	existsArgs := append(prefix[1:], "image", "exists", tag)
	if err := runner.Run(prefix[0], existsArgs...); err == nil {
		fmt.Printf(">>> [customize] derived image cache hit for %s (%s)\n", image, tag)
		return tag, nil
	}

	ctr := "tbox-customize-" + key
	rmArgs := append(prefix[1:], "rm", "-f", "--ignore", ctr)
	_ = runner.Run(prefix[0], rmArgs...)

	runArgs := append(prefix[1:],
		"run", "--name", ctr,
		"--cap-add", "sys_admin",
		"--security-opt", "label=disable",
	)
	var inner strings.Builder
	inner.WriteString("set -eu\n")
	for i, s := range scripts {
		abs, err := filepath.Abs(s)
		if err != nil {
			return "", fmt.Errorf("resolve customize script %s: %w", s, err)
		}
		mnt := fmt.Sprintf("/run/tbox-customize/%d", i)
		runArgs = append(runArgs, "-v", filepath.Dir(abs)+":"+mnt+":ro")
		fmt.Fprintf(&inner, "cd %s && bash ./%s\n", mnt, shellEsc(filepath.Base(abs)))
	}
	runArgs = append(runArgs, "--entrypoint", "/bin/bash", image, "-c", inner.String())

	fmt.Printf(">>> [customize] running %d script(s) against %s\n", len(scripts), image)
	if err := runner.Run(prefix[0], runArgs...); err != nil {
		rmArgs := append(prefix[1:], "rm", "-f", "--ignore", ctr)
		_ = runner.Run(prefix[0], rmArgs...)
		return "", fmt.Errorf("live customize %s: %w", image, err)
	}

	commitArgs := append(prefix[1:], "commit", "--quiet", ctr, tag)
	if err := runner.Run(prefix[0], commitArgs...); err != nil {
		return "", fmt.Errorf("commit customized %s: %w", image, err)
	}
	rmArgs = append(prefix[1:], "rm", "-f", "--ignore", ctr)
	_ = runner.Run(prefix[0], rmArgs...)

	fmt.Printf(">>> [customize] committed %s\n", tag)
	return tag, nil
}

// customizeCacheKey hashes the base image ID and every script's path + content
// into the derived image tag. Content (not mtime) keying means CI rebuilds
// from a fresh checkout still hit the cache when nothing changed.
func customizeCacheKey(imageID string, scripts []string) (string, error) {
	h := sha256.New()
	fmt.Fprintf(h, "%s\n", imageID)
	for _, s := range scripts {
		data, err := os.ReadFile(s)
		if err != nil {
			return "", fmt.Errorf("read customize script %s: %w", s, err)
		}
		fmt.Fprintf(h, "%s\n", filepath.Base(s))
		h.Write(data)
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))[:16], nil
}
