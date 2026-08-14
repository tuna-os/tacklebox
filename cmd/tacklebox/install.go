// Package-file split of the former 1,420-line cmd/tacklebox/build.go
// (architect refactor, tuna-os/tacklebox#217). Declarations moved
// verbatim between files of the same package — no behavior change.
package main

import (
	"encoding/json"
	"fmt"
	"github.com/tuna-os/tacklebox/internal/blockdev"
	"github.com/tuna-os/tacklebox/internal/install"
	"github.com/tuna-os/tacklebox/internal/recipe"
	"github.com/tuna-os/tacklebox/internal/runner"
	"github.com/tuna-os/tacklebox/internal/target"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

func runEnvs(r recipe.MediaRecipe, tgt target.Target, storeMount, espMount string, parallel int, track func(string, func() error) error, remoraManifests []*install.RemoraManifest) error {
	// Cross-env dedup (ISO only): envs share squashfs content, so the
	// per-env loop shape doesn't apply.
	if tgt.InstallMode() == target.InstallModeLive && r.SharedStore.Dedup {
		if r.SharedStore.DedupLayout == "delta" {
			return installEnvsLiveDelta(r, tgt, storeMount, espMount, track, remoraManifests)
		}
		return installEnvsLiveCombined(r, tgt, storeMount, espMount, track, remoraManifests)
	}
	if parallel <= 1 {
		for i, env := range r.BootableEnvironments {
			if err := installEnv(env, r, tgt, storeMount, espMount, track, remoraAt(remoraManifests, i)); err != nil {
				return err
			}
		}
		return nil
	}

	fmt.Printf(">>> Running %d environments with parallelism=%d\n", len(r.BootableEnvironments), parallel)
	sem := make(chan struct{}, parallel)
	var wg sync.WaitGroup
	errs := make([]error, len(r.BootableEnvironments))
	for i, env := range r.BootableEnvironments {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int, env recipe.BootableEnvironment) {
			defer wg.Done()
			defer func() { <-sem }()
			errs[i] = installEnv(env, r, tgt, storeMount, espMount, track, remoraAt(remoraManifests, i))
		}(i, env)
	}
	wg.Wait()

	var failed []string
	for i, err := range errs {
		if err != nil {
			failed = append(failed, fmt.Sprintf("  - %s: %v", r.BootableEnvironments[i].ID, err))
		}
	}
	if len(failed) > 0 {
		return fmt.Errorf("%d environment(s) failed:\n%s", len(failed), strings.Join(failed, "\n"))
	}
	return nil
}

// combinedSquashName is the single squashfs every env boots from when
// shared_store.dedup is set (ISO targets only).

// envTitle returns the boot-menu title for an env: the recipe's `title`
// field when set (e.g. "Bluefin (GNOME)"), otherwise the env ID, with the
// boot mode appended.
func envTitle(env recipe.BootableEnvironment, mode string) string {
	t := env.Title
	if t == "" {
		t = env.ID
	}
	return fmt.Sprintf("%s (%s)", t, mode)
}

// installEnv runs the per-env install pipeline. Dispatches on the
// target's InstallMode: bootc (block targets) or live (iso targets).
// Safe to invoke concurrently for distinct envs.
func installEnv(env recipe.BootableEnvironment, r recipe.MediaRecipe, tgt target.Target, storeMount, espMount string, track func(string, func() error) error, remoraManifest *install.RemoraManifest) error {
	switch tgt.InstallMode() {
	case target.InstallModeBootc:
		return installEnvBootc(env, r, tgt, storeMount, espMount, track)
	case target.InstallModeLive:
		return installEnvLive(env, r, tgt, storeMount, espMount, track, remoraManifest)
	}
	return fmt.Errorf("unsupported install mode %q", tgt.InstallMode())
}

func installEnvBootc(env recipe.BootableEnvironment, r recipe.MediaRecipe, tgt target.Target, storeMount, espMount string, track func(string, func() error) error) error {
	backend := install.Backend(env.Backend)
	if backend == "" {
		detected, err := install.DetectBackend(env.Image)
		if err != nil {
			return err
		}
		backend = detected
	}

	envRoot := filepath.Join(storeMount, "tbox-install", env.ID)
	if err := track("clear:"+env.ID, func() error { return install.ClearEnvDir(envRoot) }); err != nil {
		return fmt.Errorf("clear env dir for %s: %w", env.ID, err)
	}
	if err := runner.Run("sudo", "mkdir", "-p", envRoot); err != nil {
		return fmt.Errorf("create env root for %s: %w", env.ID, err)
	}
	if err := track("install:"+env.ID, func() error {
		return install.PullAndInstall(env.Image, envRoot, env.ID, backend)
	}); err != nil {
		return fmt.Errorf("install %s: %w", env.ID, err)
	}

	bootDir := filepath.Join(espMount, "EFI", env.ID)
	if err := runner.Run("sudo", "mkdir", "-p", bootDir); err != nil {
		return fmt.Errorf("create boot dir %s: %w", bootDir, err)
	}
	if err := runner.Run("sudo", "chmod", "0755", bootDir); err != nil {
		return fmt.Errorf("chmod boot dir %s: %w", bootDir, err)
	}
	var initrdOverride string
	if err := track("initramfs:"+env.ID, func() error {
		var err error
		initrdOverride, err = install.PrepareInitramfs(env.Image, install.BlockInitramfsModules, env.SkipInitramfsRebuild)
		return err
	}); err != nil {
		return fmt.Errorf("prepare initramfs for %s: %w", env.ID, err)
	}
	var kver string
	if err := track("extract:"+env.ID, func() error {
		var err error
		kver, err = install.ExtractBootFiles(env.Image, bootDir, initrdOverride)
		return err
	}); err != nil {
		return fmt.Errorf("extract boot files for %s: %w", env.ID, err)
	}

	var ostreeBootcsum string
	if backend == install.BackendOstree {
		csum, fErr := install.FindOstreeDeployment(envRoot, env.ID)
		if fErr != nil {
			return fmt.Errorf("locate ostree deployment for %s: %w", env.ID, fErr)
		}
		if csum == "" {
			return fmt.Errorf("ostree backend declared but no deployment found under %s", envRoot)
		}
		ostreeBootcsum = csum
	}

	for _, mode := range env.Modes {
		id := fmt.Sprintf("%s-%s", env.ID, mode)
		options := appendKargs(buildKernelCmdline(env.ID, mode, backend, blockdev.UsbSafe, ostreeBootcsum), r.Kargs)
		isDefault := env.ID == r.DefaultBoot
		if err := install.WriteBLSEntry(espMount, id, envTitle(env, string(mode)), tgt.KernelPath(env.ID), tgt.InitrdPath(env.ID), options, isDefault); err != nil {
			return err
		}
	}

	// Provision the update-all timer and tools into the env's deployment.
	// Only for BlockTarget (live ISOs are ephemeral/read-only anyway).
	if err := track("provision:"+env.ID, func() error {
		return provisionUpdateSystem(envRoot, env.ID, r)
	}); err != nil {
		fmt.Fprintf(os.Stderr, ">>> WARNING: failed to provision update tools into %s: %v\n", env.ID, err)
	}

	fmt.Printf(">>> Finished environment: %s (kernel=%s)\n", env.ID, kver)
	return nil
}

// installEnvLive packs env's container rootfs as a single squashfs and
// writes a tbox-live BLS entry. ISO label comes from the IsoTarget;
// we reach it via a small interface to avoid a hard target.IsoTarget
// import.

// installEnvLive packs env's container rootfs as a single squashfs and
// writes a tbox-live BLS entry. ISO label comes from the IsoTarget;
// we reach it via a small interface to avoid a hard target.IsoTarget
// import.
func installEnvLive(env recipe.BootableEnvironment, r recipe.MediaRecipe, tgt target.Target, storeMount, espMount string, track func(string, func() error) error, remoraManifest *install.RemoraManifest) error {
	type labelled interface{ IsoLabel() string }
	label := "TACKLEBOX"
	if l, ok := tgt.(labelled); ok {
		label = l.IsoLabel()
	}

	// Live customization first: everything downstream (initramfs probe,
	// squash, boot-file extraction) works from the derived image. env is a
	// by-value copy, so rewriting Image here is local to this install.
	if len(env.LiveCustomize) > 0 {
		if err := track("customize:"+env.ID, func() error {
			derived, err := install.CustomizeLive(env.Image, env.LiveCustomize)
			if err == nil {
				env.Image = derived
			}
			return err
		}); err != nil {
			return fmt.Errorf("live customize for %s: %w", env.ID, err)
		}
	}

	// Remora package/config layering: runs /usr/bin/remora apply inside a
	// container of the (possibly already customized) image, commits the
	// result, and squashes the derived image. Must happen after
	// live_customize so remora sees any filesystem changes those scripts
	// made, and before squash/extract so the layered packages land in the
	// final rootfs.
	if remoraManifest != nil {
		if err := track("remora:"+env.ID, func() error {
			derived, err := install.RemoraCustomize(env.Image, remoraManifest)
			if err == nil {
				env.Image = derived
			}
			return err
		}); err != nil {
			return fmt.Errorf("remora customize for %s: %w", env.ID, err)
		}
	}

	// Initramfs first: a hopeless image (no dracut) fails here in
	// seconds instead of after a multi-minute squash.
	var initrdOverride string
	if err := track("initramfs:"+env.ID, func() error {
		var err error
		initrdOverride, err = install.PrepareInitramfs(env.Image, install.IsoInitramfsModules, env.SkipInitramfsRebuild)
		return err
	}); err != nil {
		return fmt.Errorf("prepare initramfs for %s: %w", env.ID, err)
	}

	sfs := filepath.Join(storeMount, env.ID+".rootfs.sfs")
	if err := track("install:"+env.ID, func() error {
		return install.InstallLive(env.Image, sfs, r.SharedStore.Compression)
	}); err != nil {
		return fmt.Errorf("squashfs %s: %w", env.ID, err)
	}

	// Per-env kernel/initrd lives at /images/pxeboot/<env>/ inside the
	// FAT ESP (mirrored to iso-root by IsoTarget.Finalize).
	bootDir := filepath.Join(espMount, "images", "pxeboot", env.ID)
	if err := runner.Run("sudo", "mkdir", "-p", bootDir); err != nil {
		return fmt.Errorf("create boot dir %s: %w", bootDir, err)
	}
	if err := runner.Run("sudo", "chmod", "0755", bootDir); err != nil {
		return fmt.Errorf("chmod boot dir %s: %w", bootDir, err)
	}
	var kver string
	if err := track("extract:"+env.ID, func() error {
		var err error
		kver, err = install.ExtractBootFiles(env.Image, bootDir, initrdOverride)
		return err
	}); err != nil {
		return fmt.Errorf("extract boot files for %s: %w", env.ID, err)
	}

	options := appendKargs(buildLiveKernelCmdline(env.ID, label), r.Kargs)
	isDefault := env.ID == r.DefaultBoot
	if err := install.WriteBLSEntry(espMount, env.ID, envTitle(env, "live"), tgt.KernelPath(env.ID), tgt.InitrdPath(env.ID), options, isDefault); err != nil {
		return err
	}
	fmt.Printf(">>> Finished environment: %s (kernel=%s)\n", env.ID, kver)
	return nil
}

// installEnvsLiveCombined is the cross-env dedup variant of the live
// install loop (shared_store.dedup). One mksquashfs pass packs every
// env's rootfs as a subtree of LiveOS/combined.rootfs.sfs, deduplicating
// files shared between images. Each env still gets its own kernel,
// initrd, and BLS entry; the entry pivots into the env's subtree via
// tacklebox.root= (see buildLiveKernelCmdlineCombined).

// installEnvsLiveCombined is the cross-env dedup variant of the live
// install loop (shared_store.dedup). One mksquashfs pass packs every
// env's rootfs as a subtree of LiveOS/combined.rootfs.sfs, deduplicating
// files shared between images. Each env still gets its own kernel,
// initrd, and BLS entry; the entry pivots into the env's subtree via
// tacklebox.root= (see buildLiveKernelCmdlineCombined).
func installEnvsLiveCombined(r recipe.MediaRecipe, tgt target.Target, storeMount, espMount string, track func(string, func() error) error, remoraManifests []*install.RemoraManifest) error {
	type labelled interface{ IsoLabel() string }
	label := "TACKLEBOX"
	if l, ok := tgt.(labelled); ok {
		label = l.IsoLabel()
	}

	// Live customization first (see installEnvLive). Work on a copy of the
	// env list so the derived image refs stay local to this build pass.
	localEnvs := append([]recipe.BootableEnvironment(nil), r.BootableEnvironments...)
	for i := range localEnvs {
		env := &localEnvs[i]
		if len(env.LiveCustomize) == 0 {
			continue
		}
		if err := track("customize:"+env.ID, func() error {
			derived, err := install.CustomizeLive(env.Image, env.LiveCustomize)
			if err == nil {
				env.Image = derived
			}
			return err
		}); err != nil {
			return fmt.Errorf("live customize for %s: %w", env.ID, err)
		}
	}

	// Remora layering for each env (after customize, before squash).
	for i := range localEnvs {
		env := &localEnvs[i]
		if remoraAt(remoraManifests, i) == nil {
			continue
		}
		if err := track("remora:"+env.ID, func() error {
			derived, err := install.RemoraCustomize(env.Image, remoraAt(remoraManifests, i))
			if err == nil {
				env.Image = derived
			}
			return err
		}); err != nil {
			return fmt.Errorf("remora customize for %s: %w", env.ID, err)
		}
	}

	// Initramfs prep for every env up front: a hopeless image fails in
	// seconds, before the (single, expensive) combined squash.
	initrdOverrides := make(map[string]string, len(localEnvs))
	for _, env := range localEnvs {
		env := env
		if err := track("initramfs:"+env.ID, func() error {
			p, err := install.PrepareInitramfs(env.Image, install.IsoInitramfsModules, env.SkipInitramfsRebuild)
			initrdOverrides[env.ID] = p
			return err
		}); err != nil {
			return fmt.Errorf("prepare initramfs for %s: %w", env.ID, err)
		}
	}

	envs := make([]install.LiveEnv, 0, len(localEnvs))
	for _, e := range localEnvs {
		envs = append(envs, install.LiveEnv{ID: e.ID, Image: e.Image})
	}
	sfs := filepath.Join(storeMount, combinedSquashName)
	if err := track("install:combined", func() error {
		return install.InstallLiveCombined(envs, sfs, r.SharedStore.Compression)
	}); err != nil {
		return fmt.Errorf("combined squashfs: %w", err)
	}

	for _, env := range localEnvs {
		bootDir := filepath.Join(espMount, "images", "pxeboot", env.ID)
		if err := runner.Run("sudo", "mkdir", "-p", bootDir); err != nil {
			return fmt.Errorf("create boot dir %s: %w", bootDir, err)
		}
		if err := runner.Run("sudo", "chmod", "0755", bootDir); err != nil {
			return fmt.Errorf("chmod boot dir %s: %w", bootDir, err)
		}
		var kver string
		env := env
		if err := track("extract:"+env.ID, func() error {
			var err error
			kver, err = install.ExtractBootFiles(env.Image, bootDir, initrdOverrides[env.ID])
			return err
		}); err != nil {
			return fmt.Errorf("extract boot files for %s: %w", env.ID, err)
		}

		options := appendKargs(buildLiveKernelCmdlineCombined(env.ID, label), r.Kargs)
		isDefault := env.ID == r.DefaultBoot
		if err := install.WriteBLSEntry(espMount, env.ID, envTitle(env, "live"), tgt.KernelPath(env.ID), tgt.InitrdPath(env.ID), options, isDefault); err != nil {
			return err
		}
		fmt.Printf(">>> Finished environment: %s (kernel=%s, combined)\n", env.ID, kver)
	}
	return nil
}

// installEnvsLiveDelta is the delta-dedup variant of the live install
// loop (shared_store.dedup_layout=delta). The base env's rootfs becomes
// LiveOS/base.rootfs.sfs; every other env gets a small
// LiveOS/<env>.delta.sfs diffed against it (install.TreeDiff — copies +
// overlayfs whiteouts). Each env keeps its own kernel, initrd, and BLS
// entry; non-base entries stack their delta as an extra overlay
// lowerdir via tacklebox.live.delta=.
//
// Compared to the combined layout: slightly weaker dedup (the diff is
// against one base, not global), but updating a single env's image only
// re-squashes that env's delta instead of the whole store.

// installEnvsLiveDelta is the delta-dedup variant of the live install
// loop (shared_store.dedup_layout=delta). The base env's rootfs becomes
// LiveOS/base.rootfs.sfs; every other env gets a small
// LiveOS/<env>.delta.sfs diffed against it (install.TreeDiff — copies +
// overlayfs whiteouts). Each env keeps its own kernel, initrd, and BLS
// entry; non-base entries stack their delta as an extra overlay
// lowerdir via tacklebox.live.delta=.
//
// Compared to the combined layout: slightly weaker dedup (the diff is
// against one base, not global), but updating a single env's image only
// re-squashes that env's delta instead of the whole store.
func installEnvsLiveDelta(r recipe.MediaRecipe, tgt target.Target, storeMount, espMount string, track func(string, func() error) error, remoraManifests []*install.RemoraManifest) error {
	type labelled interface{ IsoLabel() string }
	label := "TACKLEBOX"
	if l, ok := tgt.(labelled); ok {
		label = l.IsoLabel()
	}
	baseID := deltaBaseEnv(r)

	// Live customization first (see installEnvLive). Work on a copy of the
	// env list so the derived image refs stay local to this build pass.
	localEnvs := append([]recipe.BootableEnvironment(nil), r.BootableEnvironments...)
	for i := range localEnvs {
		env := &localEnvs[i]
		if len(env.LiveCustomize) == 0 {
			continue
		}
		if err := track("customize:"+env.ID, func() error {
			derived, err := install.CustomizeLive(env.Image, env.LiveCustomize)
			if err == nil {
				env.Image = derived
			}
			return err
		}); err != nil {
			return fmt.Errorf("live customize for %s: %w", env.ID, err)
		}
	}

	// Remora layering for each env (after customize, before squash).
	for i := range localEnvs {
		env := &localEnvs[i]
		if remoraAt(remoraManifests, i) == nil {
			continue
		}
		if err := track("remora:"+env.ID, func() error {
			derived, err := install.RemoraCustomize(env.Image, remoraAt(remoraManifests, i))
			if err == nil {
				env.Image = derived
			}
			return err
		}); err != nil {
			return fmt.Errorf("remora customize for %s: %w", env.ID, err)
		}
	}

	// Initramfs prep for every env up front: a hopeless image fails in
	// seconds, before the expensive base squash + diffs.
	initrdOverrides := make(map[string]string, len(localEnvs))
	for _, env := range localEnvs {
		env := env
		if err := track("initramfs:"+env.ID, func() error {
			p, err := install.PrepareInitramfs(env.Image, install.IsoInitramfsModules, env.SkipInitramfsRebuild)
			initrdOverrides[env.ID] = p
			return err
		}); err != nil {
			return fmt.Errorf("prepare initramfs for %s: %w", env.ID, err)
		}
	}

	var baseEnv install.LiveEnv
	envs := make([]install.LiveEnv, 0, len(localEnvs))
	for _, e := range localEnvs {
		le := install.LiveEnv{ID: e.ID, Image: e.Image}
		envs = append(envs, le)
		if e.ID == baseID {
			baseEnv = le
		}
	}
	if err := track("install:delta", func() error {
		return install.InstallLiveDelta(baseEnv, envs, storeMount, baseSquashName, r.SharedStore.Compression)
	}); err != nil {
		return fmt.Errorf("delta squashfs store: %w", err)
	}

	for _, env := range localEnvs {
		bootDir := filepath.Join(espMount, "images", "pxeboot", env.ID)
		if err := runner.Run("sudo", "mkdir", "-p", bootDir); err != nil {
			return fmt.Errorf("create boot dir %s: %w", bootDir, err)
		}
		if err := runner.Run("sudo", "chmod", "0755", bootDir); err != nil {
			return fmt.Errorf("chmod boot dir %s: %w", bootDir, err)
		}
		var kver string
		env := env
		if err := track("extract:"+env.ID, func() error {
			var err error
			kver, err = install.ExtractBootFiles(env.Image, bootDir, initrdOverrides[env.ID])
			return err
		}); err != nil {
			return fmt.Errorf("extract boot files for %s: %w", env.ID, err)
		}

		options := appendKargs(buildLiveKernelCmdlineDelta(env.ID, label, env.ID == baseID), r.Kargs)
		isDefault := env.ID == r.DefaultBoot
		if err := install.WriteBLSEntry(espMount, env.ID, envTitle(env, "live"), tgt.KernelPath(env.ID), tgt.InitrdPath(env.ID), options, isDefault); err != nil {
			return err
		}
		fmt.Printf(">>> Finished environment: %s (kernel=%s, delta)\n", env.ID, kver)
	}
	return nil
}

// prePullAll pulls all env images + offline payloads concurrently,
// deduplicating references that appear in both lists. Errors are
// aggregated so the user sees every failure in one go.
//
// The destination store matches who reads the images later:
//   - userStore (ISO/live targets): the invoking user's rootless store —
//     squash, extract, and dracut-rebuild all run there via podman
//     unshare / UserPodmanPrefix. Pulling into root's store instead would
//     make each of those auto-pull a second copy.
//   - root store (block targets): `podman run … bootc install` runs as
//     root. localhost/ images are skipped here — they live in the user's
//     store and rootless podman accesses them directly.

// prePullAll pulls all env images + offline payloads concurrently,
// deduplicating references that appear in both lists. Errors are
// aggregated so the user sees every failure in one go.
//
// The destination store matches who reads the images later:
//   - userStore (ISO/live targets): the invoking user's rootless store —
//     squash, extract, and dracut-rebuild all run there via podman
//     unshare / UserPodmanPrefix. Pulling into root's store instead would
//     make each of those auto-pull a second copy.
//   - root store (block targets): `podman run … bootc install` runs as
//     root. localhost/ images are skipped here — they live in the user's
//     store and rootless podman accesses them directly.
func prePullAll(r recipe.MediaRecipe, userStore bool) error {
	seen := make(map[string]struct{})
	var unique []string
	add := func(ref string) {
		if !userStore && strings.HasPrefix(ref, "localhost/") {
			return // built locally; rootless podman accesses them directly
		}
		if _, dup := seen[ref]; !dup {
			seen[ref] = struct{}{}
			unique = append(unique, ref)
		}
	}
	for _, e := range r.BootableEnvironments {
		add(e.Image)
	}
	for _, p := range r.OfflinePayloads {
		add(p.Source)
	}
	if len(unique) == 0 {
		return nil
	}

	pull := install.Pull
	if userStore {
		pull = install.PullUser
	}
	fmt.Printf(">>> Pre-pulling %d image(s) in parallel (%d env, %d payload)\n",
		len(unique), len(r.BootableEnvironments), len(r.OfflinePayloads))
	var wg sync.WaitGroup
	errs := make([]error, len(unique))
	for i, img := range unique {
		wg.Add(1)
		go func(i int, img string) {
			defer wg.Done()
			errs[i] = pull(img)
		}(i, img)
	}
	wg.Wait()

	// Parallel pulls of images sharing layers race in podman's storage
	// ("rename …tar-split.gz: no such file or directory" mid-commit).
	// Retry stragglers serially — the competing layer is committed by
	// now, so the second attempt is cheap and deterministic.
	for i, err := range errs {
		if err == nil {
			continue
		}
		fmt.Printf(">>> Pre-pull retry (serial) for %s\n", unique[i])
		errs[i] = pull(unique[i])
	}

	var failed []string
	for i, err := range errs {
		if err != nil {
			failed = append(failed, fmt.Sprintf("  - %s: %v", unique[i], err))
		}
	}
	if len(failed) > 0 {
		return fmt.Errorf("pre-pull failed for %d image(s):\n%s", len(failed), strings.Join(failed, "\n"))
	}
	return nil
}

// provisionUpdateSystem drops the tacklebox binary, systemd units, and
// build-time recipe into the env's filesystem so it can stay current
// autonomously.
func provisionUpdateSystem(envRoot, envID string, r recipe.MediaRecipe) error {
	destBin := filepath.Join(envRoot, "usr", "local", "bin", "tacklebox")
	destUnitDir := filepath.Join(envRoot, "etc", "systemd", "system")
	destRecipeDir := filepath.Join(envRoot, "etc", "tacklebox")

	// 1. tacklebox binary
	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve self-path: %w", err)
	}
	runner.Run("sudo", "mkdir", "-p", filepath.Dir(destBin))
	if err := runner.Run("sudo", "cp", self, destBin); err != nil {
		return fmt.Errorf("copy tacklebox binary to %s: %w", destBin, err)
	}

	// 2. recipe.json
	runner.Run("sudo", "mkdir", "-p", destRecipeDir)
	recipeData, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal recipe: %w", err)
	}
	if err := os.WriteFile("/tmp/tbox-recipe.json", recipeData, 0644); err != nil {
		return err
	}
	if err := runner.Run("sudo", "cp", "/tmp/tbox-recipe.json", filepath.Join(destRecipeDir, "recipe.json")); err != nil {
		return fmt.Errorf("copy recipe to %s: %w", destRecipeDir, err)
	}

	// 3. systemd units
	// These are shipped in src/systemd/ inside the repo.
	runner.Run("sudo", "mkdir", "-p", destUnitDir)
	for _, f := range []string{"tacklebox-update-all.service", "tacklebox-update-all.timer"} {
		src := filepath.Join("src", "systemd", f)
		if _, err := os.Stat(src); err != nil {
			// Fallback for when running from a non-repo binary (e.g. installed)
			continue
		}
		if err := runner.Run("sudo", "cp", src, filepath.Join(destUnitDir, f)); err != nil {
			return fmt.Errorf("copy %s: %w", f, err)
		}
	}

	// 4. enable the timer
	timerWants := filepath.Join(destUnitDir, "timers.target.wants")
	runner.Run("sudo", "mkdir", "-p", timerWants)
	runner.Run("sudo", "ln", "-sf",
		"../tacklebox-update-all.timer",
		filepath.Join(timerWants, "tacklebox-update-all.timer"))

	return nil
}
