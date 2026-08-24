package kernelcmdline

import (
	"strings"
	"testing"

	"github.com/tuna-os/tacklebox/internal/install"
	"github.com/tuna-os/tacklebox/internal/recipe"
)

func TestAppend(t *testing.T) {
	cases := []struct {
		name    string
		options string
		kargs   []string
		want    string
	}{
		{"no kargs", "root=x", nil, "root=x"},
		{"single karg", "root=x", []string{"quiet"}, "root=x quiet"},
		{"multiple kargs", "root=x", []string{"quiet", "splash"}, "root=x quiet splash"},
		{"empty entries skipped", "root=x", []string{"", "quiet", "  "}, "root=x quiet"},
		{"whitespace trimmed", "root=x", []string{"  quiet  "}, "root=x quiet"},
		{"empty options", "", []string{"quiet"}, " quiet"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := Append(c.options, c.kargs)
			if got != c.want {
				t.Errorf("Append(%q, %v) = %q, want %q", c.options, c.kargs, got, c.want)
			}
		})
	}
}

func TestLive(t *testing.T) {
	got := Live("env1", "MyLabel")
	want := "root=tbox:CDLABEL=MyLabel tacklebox.live.squashimg=env1.rootfs.sfs" +
		" tacklebox.live.overlay.size=8192 enforcing=0" +
		" tacklebox.env=env1 console=ttyS0,115200n8"
	if got != want {
		t.Errorf("Live() = %q, want %q", got, want)
	}
}

func TestLive_LabelSpaceEscaped(t *testing.T) {
	got := Live("env1", "My Label")
	if !strings.Contains(got, `CDLABEL=My\x20Label`) {
		t.Errorf("Live() with spaced label = %q, want CDLABEL=My\\x20Label", got)
	}
	if strings.Contains(got, "My Label") {
		t.Errorf("Live() leaked raw space into cmdline: %q", got)
	}
}

func TestLiveCombined(t *testing.T) {
	got := LiveCombined("env1", "MyLabel")
	if !strings.Contains(got, "tacklebox.live.squashimg="+CombinedSquashName) {
		t.Errorf("LiveCombined() = %q, want squashimg=%s", got, CombinedSquashName)
	}
	if !strings.Contains(got, "tacklebox.root=env1") {
		t.Errorf("LiveCombined() = %q, want tacklebox.root=env1", got)
	}
}

func TestLiveDelta(t *testing.T) {
	t.Run("base image", func(t *testing.T) {
		got := LiveDelta("env1", "MyLabel", true)
		if !strings.Contains(got, "tacklebox.live.squashimg="+BaseSquashName) {
			t.Errorf("LiveDelta(base) = %q, want squashimg=%s", got, BaseSquashName)
		}
		if strings.Contains(got, "tacklebox.live.delta") {
			t.Errorf("LiveDelta(base) must not set delta arg: %q", got)
		}
	})
	t.Run("delta image", func(t *testing.T) {
		got := LiveDelta("env1", "MyLabel", false)
		if !strings.Contains(got, "tacklebox.live.delta=env1.delta.sfs") {
			t.Errorf("LiveDelta(delta) = %q, want tacklebox.live.delta=env1.delta.sfs", got)
		}
		if !strings.Contains(got, "tacklebox.live.squashimg="+BaseSquashName) {
			t.Errorf("LiveDelta(delta) = %q, want squashimg=%s", got, BaseSquashName)
		}
	})
}

func TestBuild(t *testing.T) {
	cases := []struct {
		name           string
		envID          string
		mode           recipe.BootMode
		backend        install.Backend
		usbSafe        bool
		ostreeBootcsum string
		wantContains   []string
		wantExcludes   []string
	}{
		{
			name: "persistent ostree", envID: "env1", mode: recipe.ModePersistent,
			backend: install.BackendOstree, ostreeBootcsum: "abc123",
			wantContains: []string{
				"root=LABEL=TBOX_STORE rw console=ttyS0 tacklebox.root=tbox-install/env1",
				"tacklebox.persist=LABEL=TBOX_PERSIST",
				"ostree=/ostree/boot.1/env1/abc123/0",
			},
			wantExcludes: []string{"rd.live.overlay=tmpfs", "rootflags="},
		},
		{
			name: "live ostree", envID: "env1", mode: recipe.ModeLive,
			backend: install.BackendOstree, ostreeBootcsum: "abc123",
			wantContains: []string{
				"rd.live.overlay=tmpfs",
				"ostree=/ostree/boot.1/env1/abc123/0",
			},
			wantExcludes: []string{"tacklebox.persist=", "rootflags="},
		},
		{
			name: "persistent composefs", envID: "env1", mode: recipe.ModePersistent,
			backend: install.BackendComposefs,
			wantContains: []string{
				"tacklebox.persist=LABEL=TBOX_PERSIST",
				"rootflags=subvol=containers/storage/overlay/default/diff",
			},
			wantExcludes: []string{"ostree="},
		},
		{
			name: "usb safe composefs adds mount-safety rootflags", envID: "env1",
			mode: recipe.ModePersistent, backend: install.BackendComposefs, usbSafe: true,
			wantContains: []string{
				"rootflags=subvol=containers/storage/overlay/default/diff,commit=1,errors=remount-ro",
			},
		},
		{
			name: "usb safe ostree adds bare rootflags (no subvol)", envID: "env1",
			mode: recipe.ModePersistent, backend: install.BackendOstree, usbSafe: true,
			ostreeBootcsum: "abc123",
			wantContains: []string{
				"rootflags=commit=1,errors=remount-ro",
			},
			wantExcludes: []string{"subvol="},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := Build(c.envID, c.mode, c.backend, c.usbSafe, c.ostreeBootcsum)
			for _, want := range c.wantContains {
				if !strings.Contains(got, want) {
					t.Errorf("Build(...) = %q, want it to contain %q", got, want)
				}
			}
			for _, exclude := range c.wantExcludes {
				if strings.Contains(got, exclude) {
					t.Errorf("Build(...) = %q, want it to NOT contain %q", got, exclude)
				}
			}
		})
	}
}
