package livemedia

import (
	"strings"
	"testing"
)

func TestLiveKargsEscapesLabelSpaces(t *testing.T) {
	got := LiveKargs("snow", "Tuna OS", "snow.rootfs.sfs", "")
	if !strings.HasPrefix(got, `root=tbox:CDLABEL=Tuna\x20OS `) {
		t.Errorf("label space not escaped: %q", got)
	}
	n := 0
	for _, f := range strings.Fields(got) {
		if strings.HasPrefix(f, "root=") {
			n++
			if f != `root=tbox:CDLABEL=Tuna\x20OS` {
				t.Errorf("root argument = %q", f)
			}
		}
	}
	if n != 1 {
		t.Errorf("got %d root arguments, want 1: %q", n, got)
	}
}

func TestLiveKargsPlainLabel(t *testing.T) {
	want := "root=tbox:CDLABEL=TUNA tacklebox.live.squashimg=snow.rootfs.sfs" +
		" tacklebox.live.overlay.size=8192 enforcing=0" +
		" tacklebox.env=snow tacklebox.root=snow console=ttyS0,115200n8"
	got := LiveKargs("snow", "TUNA", "snow.rootfs.sfs", " tacklebox.root=snow")
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}
