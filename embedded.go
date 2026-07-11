// Package tacklebox holds repo-level assets embedded into the binary so
// that internal packages can use them without the source checkout being
// present at runtime (installed binaries, CI runners, containers).
package tacklebox

import "embed"

// DracutModules carries the source of every tacklebox dracut module.
// PrepareInitramfs (internal/install) materializes them to temp dirs and
// bind-mounts each into the dracut rebuild container at
// /usr/lib/dracut/modules.d/<name>:
//
//   - 95tbox-root: per-env root pivot + persist overlay (all targets)
//   - 90tbox-live: distro-neutral live root — ISO by label -> squashfs ->
//     tmpfs overlay -> /sysroot (ISO targets; replaces dmsquash-live)
//
//go:embed src/dracut/95tbox-root src/dracut/90tbox-live
var DracutModules embed.FS
