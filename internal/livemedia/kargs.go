package livemedia

import (
	"fmt"
	"strings"
)

// LiveKargs returns the kernel arguments that boot a live ISO root from the
// squashfs or EROFS image squashimg. extra is appended after tacklebox.env and
// must be empty or start with a space.
//
// ISO labels containing spaces use udev's escaped by-label spelling (\x20) so
// kernel command-line tokenization does not split the root argument. Every
// live builder must use this function so the escaping cannot drift.
func LiveKargs(envID, label, squashimg, extra string) string {
	label = strings.ReplaceAll(label, " ", "\\x20")
	return fmt.Sprintf(
		"root=tbox:CDLABEL=%s tacklebox.live.squashimg=%s"+
			" tacklebox.live.overlay.size=8192 enforcing=0"+
			" tacklebox.env=%s%s console=ttyS0,115200n8",
		label, squashimg, envID, extra,
	)
}
