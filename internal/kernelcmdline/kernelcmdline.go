// Package kernelcmdline owns the boot contract between generated BLS entries
// and tacklebox's initramfs modules.
package kernelcmdline

import (
	"fmt"
	"strings"

	"github.com/tuna-os/tacklebox/internal/install"
	"github.com/tuna-os/tacklebox/internal/recipe"
)

const (
	CombinedSquashName = "combined.rootfs.sfs"
	BaseSquashName     = "base.rootfs.sfs"
)

// Append appends recipe-level kernel arguments, skipping empty entries.
func Append(options string, kargs []string) string {
	for _, karg := range kargs {
		karg = strings.TrimSpace(karg)
		if karg != "" {
			options += " " + karg
		}
	}
	return options
}

// Live returns the BLS options for a per-environment live squashfs.
func Live(envID, label string) string {
	return live(envID, label, envID+".rootfs.sfs", "")
}

// LiveCombined returns the BLS options for an environment in a combined image.
func LiveCombined(envID, label string) string {
	return live(envID, label, CombinedSquashName, " tacklebox.root="+envID)
}

// LiveDelta returns the BLS options for an environment in a delta image.
func LiveDelta(envID, label string, isBase bool) string {
	extra := ""
	if !isBase {
		extra = " tacklebox.live.delta=" + envID + ".delta.sfs"
	}
	return live(envID, label, BaseSquashName, extra)
}

func live(envID, label, squashimg, extra string) string {
	// ISO labels containing spaces use udev's escaped by-label spelling so
	// kernel command-line tokenization does not split the root argument.
	label = strings.ReplaceAll(label, " ", "\\x20")
	return fmt.Sprintf(
		"root=tbox:CDLABEL=%s tacklebox.live.squashimg=%s"+
			" tacklebox.live.overlay.size=8192 enforcing=0"+
			" tacklebox.env=%s%s console=ttyS0,115200n8",
		label, squashimg, envID, extra,
	)
}

// Build returns the BLS options for one bootc environment.
func Build(envID string, mode recipe.BootMode, backend install.Backend, usbSafe bool, ostreeBootcsum string) string {
	cmdline := fmt.Sprintf("root=LABEL=TBOX_STORE rw console=ttyS0 tacklebox.root=tbox-install/%s", envID)
	if mode == recipe.ModeLive {
		cmdline += " rd.live.overlay=tmpfs"
	} else {
		cmdline += " tacklebox.persist=LABEL=TBOX_PERSIST"
	}

	var rootflags []string
	if backend == install.BackendOstree {
		cmdline += fmt.Sprintf(" ostree=/ostree/boot.1/%s/%s/0", envID, ostreeBootcsum)
	} else {
		rootflags = append(rootflags, "subvol=containers/storage/overlay/default/diff")
	}
	if usbSafe {
		rootflags = append(rootflags, "commit=1", "errors=remount-ro")
	}
	if len(rootflags) > 0 {
		cmdline += " rootflags=" + strings.Join(rootflags, ",")
	}
	return cmdline
}
