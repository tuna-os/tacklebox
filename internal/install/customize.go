package install

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	tacklebox "github.com/tuna-os/tacklebox"
	"github.com/tuna-os/tacklebox/internal/runner"
)

// customizeTimeoutSeconds is the podman --timeout applied to the customize
// container. Default 0 (disabled): podman 5.8.4 on the GitHub runners wedges
// `podman commit` after a `--timeout` run (tuna-os/tunaOS#1893), so the cap
// is now opt-in via TBOX_CUSTOMIZE_TIMEOUT=<seconds>. The caller's outer
// deadline still bounds a wedged customize; setting a value here only
// restores the tighter in-container cap on a known-good runner.
func customizeTimeoutSeconds() int {
	if v := strings.TrimSpace(os.Getenv("TBOX_CUSTOMIZE_TIMEOUT")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			return n
		}
	}
	return 0
}

// CustomizeLive runs the recipe's live_customize scripts inside a container
// of image and commits the result to a content-addressed derived image,
// returning its ref. The derived ref is what gets squashed/extracted, so the
// original image is never mutated.
//
// The derived tag is keyed by the image ID plus the content of every file in
// each script's directory — rerunning with unchanged inputs reuses the
// committed image (and therefore the downstream squashfs cache); any edit,
// including to a sourced sibling, produces a new tag.
//
// Each script runs as root with CAP_SYS_ADMIN and network (the dakota-iso
// configure-live environment: enough for flatpak install, dbus-daemon,
// dconf update). Script i's directory is mounted read-only at
// /run/tbox-customize/<i> and is the script's working directory, so scripts
// can reference sibling assets relatively.
func CustomizeLive(image string, scripts []string) (string, error) {
	// The embedded baseline runs before any recipe script (tuna-os/
	// tacklebox#97): live user, autologin, networking, sleep masking —
	// so consumers don't reimplement it per project. Media with no
	// live_customize at all keep the passthrough contract (appliance
	// ISOs shouldn't grow a live user unasked).
	if len(scripts) > 0 {
		baselineDir, err := materializeLiveBaseline()
		if err != nil {
			return "", err
		}
		scripts = append([]string{filepath.Join(baselineDir, "baseline.sh")}, scripts...)
	}
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
	// Hard cap on the customize container (tuna-os/tunaOS#1772): a customize
	// script that wedges — flatpak against a blackholed network being the
	// documented shape (see TBOX_CUSTOMIZE_NETWORK below) — used to sit
	// silently until the CALLER's budget killed the whole job, reporting
	// `cancelled` with no failing step. podman's --timeout has conmon SIGKILL
	// the container instead, so the failure happens HERE, names this step,
	// and leaves the streamed output as evidence. 30 minutes is generous —
	// a healthy customize (dnf + flatpak preinstall) is minutes; override
	// with TBOX_CUSTOMIZE_TIMEOUT=<seconds>, 0 to disable.
	timeoutSecs := customizeTimeoutSeconds()
	if timeoutSecs > 0 {
		runArgs = append(runArgs, "--timeout", strconv.Itoa(timeoutSecs))
	}
	// The customize scripts need outbound network (flatpak install above all),
	// and podman's default bridge is not always usable: on a host whose
	// netavark/firewalld rules have gone stale, root-context containers get an
	// interface with no route out while rootless ones work fine. The symptom is
	// remote — "Could not resolve hostname tunaos.org" from inside flatpak —
	// and the build then produces an ISO with no installer on it.
	// TBOX_CUSTOMIZE_NETWORK=host is the escape hatch; unset keeps podman's
	// default, so nothing changes for hosts that are fine.
	if net := strings.TrimSpace(os.Getenv("TBOX_CUSTOMIZE_NETWORK")); net != "" {
		runArgs = append(runArgs, "--network", net)
	}
	var inner strings.Builder
	inner.WriteString("set -eu\n")
	for i, s := range scripts {
		abs, err := filepath.Abs(s)
		if err != nil {
			return "", fmt.Errorf("resolve customize script %s: %w", s, err)
		}
		mnt := fmt.Sprintf("/run/tbox-customize/%d", i)
		runArgs = append(runArgs, "-v", filepath.Dir(abs)+":"+mnt+":ro")
		// Marker before each script so a hang or failure is attributable to
		// ONE script from the streamed log alone — #1772's silent hang could
		// only be blamed on "the pair".
		fmt.Fprintf(&inner, "echo %s\n",
			shellEsc(fmt.Sprintf(">>> [customize] (%d/%d) %s", i+1, len(scripts), filepath.Base(abs))))
		fmt.Fprintf(&inner, "cd %s && bash ./%s\n", mnt, shellEsc(filepath.Base(abs)))
	}
	runArgs = append(runArgs, "--entrypoint", "/bin/bash", image, "-c", inner.String())

	fmt.Printf(">>> [customize] running %d script(s) against %s\n", len(scripts), image)
	// Streamed, not runner.Run: these are consumer scripts doing package and
	// flatpak installs — minutes of legitimate output that IS the diagnosis
	// when something wedges. Quiet mode used to discard all of it (#1772).
	if err := runner.RunStreamed(prefix[0], runArgs...); err != nil {
		rmArgs := append(prefix[1:], "rm", "-f", "--ignore", ctr)
		_ = runner.Run(prefix[0], rmArgs...)
		if timeoutSecs > 0 {
			return "", fmt.Errorf("live customize %s (killed if it exceeded the %ds cap — see TBOX_CUSTOMIZE_TIMEOUT): %w", image, timeoutSecs, err)
		}
		return "", fmt.Errorf("live customize %s: %w", image, err)
	}

	// The commit flattens the customize container's writable layer into the
	// derived image. It can legitimately run for minutes on a large desktop
	// layer, and --quiet made it silent in exactly the way #1772's customize
	// hang was: a slow (or wedged) commit read as dead air until the CALLER's
	// budget killed the job. Stream it and mark it so a hang is attributable
	// to this step instead of invisible.
	fmt.Printf(">>> [customize] committing %s -> %s\n", ctr, tag)
	// Ensure the container is fully stopped before commit: a `run` under
	// `--timeout` can leave conmon holding the container in a half-reaped
	// state, and `podman commit` of that state wedges for minutes instead of
	// failing (tuna-os/tunaOS#1893 — the iso-smoke job reproduced a 56-minute
	// commit hang on a trivial echo script). `stop --time=0` SIGKILLs any
	// leftover process; on an already-exited container it is a fast no-op and
	// its error is deliberately ignored.
	stopArgs := append(append([]string{}, prefix[1:]...), "stop", "--time", "0", ctr)
	_ = runner.Run(prefix[0], stopArgs...)
	// Bound the commit too: if a runner's podman still wedges here, fail in
	// 600s with a named error instead of hanging until the caller's job budget
	// cancels the whole run (#1893). `timeout --foreground` wraps the full
	// prefix so it works in both root and SUDO_USER contexts.
	commitArgs := append(append([]string{}, prefix...), "commit", ctr, tag)
	bounded := append([]string{"timeout", "--foreground", "600"}, commitArgs...)
	if err := runner.RunStreamed(bounded[0], bounded[1:]...); err != nil {
		return "", fmt.Errorf("commit customized %s: %w", image, err)
	}
	rmArgs = append(prefix[1:], "rm", "-f", "--ignore", ctr)
	_ = runner.Run(prefix[0], rmArgs...)

	fmt.Printf(">>> [customize] committed %s\n", tag)
	return tag, nil
}

// customizeCacheKey hashes the base image ID and the full contents of every
// directory holding a customize script — not just the named scripts — into the
// derived image tag. The whole directory is what gets mounted, and scripts
// source siblings out of it, so anything less under-keys the cache.
//
// Content (not mtime) keying means CI rebuilds from a fresh checkout still hit
// the cache when nothing changed.
func customizeCacheKey(imageID string, scripts []string) (string, error) {
	h := sha256.New()
	fmt.Fprintf(h, "%s\n", imageID)

	// Execution order is part of the identity: the scripts run in sequence and
	// a later one can depend on an earlier one's effects. Hash the ordered
	// invocation list first, separately from directory contents — the
	// per-directory dedup below would otherwise collapse two orderings of the
	// same directory to the same key.
	for i, s := range scripts {
		fmt.Fprintf(h, "run:%d:%s\n", i, filepath.Base(s))
	}

	seenDir := map[string]bool{}
	for _, s := range scripts {
		abs, err := filepath.Abs(s)
		if err != nil {
			return "", fmt.Errorf("resolve customize script %s: %w", s, err)
		}
		// Hash the script's whole directory, not just the named script. The
		// entire directory is mounted at /run/tbox-customize/<i>, and scripts
		// are expected to source siblings from it — tunaOS's customize-live.sh
		// sources desktop-<flavor>.sh that way. Keying on the named scripts
		// alone meant editing an adapter did not change the tag, so the build
		// silently reused the previous derived image and shipped the OLD live
		// payload while reporting success. That cost a full verification cycle
		// on 2026-07-26: a rebuilt ISO still carried the pre-fix autostart
		// entry, with "[customize] derived image cache hit" the only clue.
		dir := filepath.Dir(abs)
		if seenDir[dir] {
			continue
		}
		seenDir[dir] = true

		entries, err := os.ReadDir(dir)
		if err != nil {
			return "", fmt.Errorf("read customize dir %s: %w", dir, err)
		}
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			if !e.IsDir() {
				names = append(names, e.Name())
			}
		}
		sort.Strings(names) // ReadDir order is not guaranteed stable across platforms
		fmt.Fprintf(h, "dir:%s\n", filepath.Base(dir))
		for _, name := range names {
			data, err := os.ReadFile(filepath.Join(dir, name))
			if err != nil {
				return "", fmt.Errorf("read customize file %s: %w", name, err)
			}
			fmt.Fprintf(h, "%s\n", name)
			h.Write(data)
			h.Write([]byte{0})
		}
	}
	return hex.EncodeToString(h.Sum(nil))[:16], nil
}

// materializeLiveBaseline writes the embedded live baseline script to a
// temp dir so it can be bind-mounted into the customize container like
// any recipe script.
func materializeLiveBaseline() (string, error) {
	dir, err := os.MkdirTemp("", "tbox-live-baseline-*")
	if err != nil {
		return "", err
	}
	data, err := tacklebox.LiveBaseline.ReadFile("src/live/baseline.sh")
	if err != nil {
		return "", fmt.Errorf("embedded live baseline: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "baseline.sh"), data, 0o755); err != nil {
		return "", err
	}
	return dir, nil
}
