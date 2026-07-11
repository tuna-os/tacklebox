package main

import (
	"strings"
	"testing"

	"github.com/tuna-os/tacklebox/internal/install"
	"github.com/tuna-os/tacklebox/internal/recipe"
)

// Small adapters so the table-driven test can use plain strings.
func modeOf(s string) recipe.BootMode { return recipe.BootMode(s) }
func backendOf(s string) install.Backend {
	switch s {
	case "ostree":
		return install.BackendOstree
	case "composefs":
		return install.BackendComposefs
	}
	return ""
}

func TestBuildKernelCmdline(t *testing.T) {
	// install package types aren't worth importing here; use string constants.
	const ostree = "ostree"
	const composefs = "composefs"

	cases := []struct {
		name     string
		env      string
		mode     string
		backend  string
		usbSafe  bool
		contains []string
		excludes []string
	}{
		{
			name:    "ostree live with usb-safe",
			env:     "bluefin",
			mode:    "live",
			backend: ostree,
			usbSafe: true,
			contains: []string{
				"root=LABEL=TBOX_STORE",
				"tacklebox.root=tbox-install/bluefin",
				"rd.live.overlay=tmpfs",
				"ostree=/ostree/boot.1/bluefin/abc123/0",
				"rootflags=commit=1,errors=remount-ro",
			},
			excludes: []string{"subvol=", "tacklebox.persist"},
		},
		{
			name:    "composefs live with usb-safe merges rootflags",
			env:     "dakota",
			mode:    "live",
			backend: composefs,
			usbSafe: true,
			contains: []string{
				"rootflags=subvol=containers/storage/overlay/default/diff,commit=1,errors=remount-ro",
			},
			excludes: []string{"ostree="},
		},
		{
			name:    "ostree persistent without usb-safe",
			env:     "bluefin",
			mode:    "persistent",
			backend: ostree,
			usbSafe: false,
			contains: []string{
				"tacklebox.persist=LABEL=TBOX_PERSIST",
				"ostree=",
			},
			excludes: []string{"commit=", "errors=remount-ro", "rd.live.overlay"},
		},
		{
			name:     "composefs persistent unsafe — no rootflags at all",
			env:      "dakota",
			mode:     "persistent",
			backend:  composefs,
			usbSafe:  false,
			contains: []string{"rootflags=subvol=containers/storage/overlay/default/diff"},
			excludes: []string{"commit=", "errors=remount-ro"},
		},
	}

	// Indirect through interface{} to avoid pulling install + recipe types
	// into the test signature; the production function casts back.
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := buildKernelCmdline(tc.env, modeOf(tc.mode), backendOf(tc.backend), tc.usbSafe, "abc123")
			for _, s := range tc.contains {
				if !strings.Contains(got, s) {
					t.Errorf("missing %q in %q", s, got)
				}
			}
			for _, s := range tc.excludes {
				if strings.Contains(got, s) {
					t.Errorf("unexpected %q in %q", s, got)
				}
			}
		})
	}
}

func TestBuildLiveKernelCmdline(t *testing.T) {
	cases := []struct {
		name     string
		envID    string
		label    string
		contains []string
		excludes []string
	}{
		{
			name:  "basic",
			envID: "bazzite",
			label: "TBX_ISO",
			contains: []string{
				"root=tbox:CDLABEL=TBX_ISO",
				"tacklebox.live.squashimg=bazzite.rootfs.sfs",
				"tacklebox.live.overlay.size=8192",
				"enforcing=0",
				"tacklebox.env=bazzite",
				"console=ttyS0,115200n8",
			},
			// The rd.live.* / root=live: args belong to dmsquash-live;
			// emitting them alongside root=tbox: would make an image that
			// happens to ship dmsquash-live fight tbox-live for the root.
			excludes: []string{"root=live:", "rd.live."},
		},
		{
			name:  "label with spaces gets passed verbatim",
			envID: "fedora",
			label: "MY_LABEL",
			contains: []string{
				"root=tbox:CDLABEL=MY_LABEL",
				"tacklebox.live.squashimg=fedora.rootfs.sfs",
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := buildLiveKernelCmdline(tc.envID, tc.label)
			for _, s := range tc.contains {
				if !strings.Contains(got, s) {
					t.Errorf("missing %q in %q", s, got)
				}
			}
			for _, s := range tc.excludes {
				if strings.Contains(got, s) {
					t.Errorf("unexpected %q in %q", s, got)
				}
			}
		})
	}
}

func TestParseSize(t *testing.T) {
	cases := []struct {
		in     string
		want   uint64
		wantOk bool
	}{
		{"1G", 1 << 30, true},
		{"32G", 32 << 30, true},
		{"16384M", 16384 << 20, true},
		{"1T", 1 << 40, true},
		{"500K", 500 << 10, true},
		{"", 0, false},
		{"big", 0, false},
		{"0G", 0, false},
		{"10X", 0, false},
	}
	for _, tc := range cases {
		got, ok := parseSize(tc.in)
		if ok != tc.wantOk || got != tc.want {
			t.Errorf("parseSize(%q) = (%d, %v), want (%d, %v)", tc.in, got, ok, tc.want, tc.wantOk)
		}
	}
}

func TestComputePartitions(t *testing.T) {
	t.Run("30G recipe", func(t *testing.T) {
		parts, err := computePartitions(recipe.MediaRecipe{
			Size:        "30G",
			SharedStore: recipe.SharedStore{Format: "ext4"},
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(parts) != 3 {
			t.Fatalf("want 3 partitions, got %d", len(parts))
		}
		if parts[0].Size != "+1G" {
			t.Errorf("ESP size = %q, want +1G", parts[0].Size)
		}
		// 30 - 1 - 2 = 27 GiB
		if parts[1].Size != "+27G" {
			t.Errorf("STORE size = %q, want +27G", parts[1].Size)
		}
		if parts[2].Size != "0" {
			t.Errorf("PERSIST size = %q, want 0 (remainder)", parts[2].Size)
		}
		if parts[1].FS != "ext4" {
			t.Errorf("STORE FS = %q, want ext4", parts[1].FS)
		}
	})

	t.Run("too small", func(t *testing.T) {
		_, err := computePartitions(recipe.MediaRecipe{
			Size:        "4G",
			SharedStore: recipe.SharedStore{Format: "ext4"},
		})
		if err == nil || !strings.Contains(err.Error(), "too small") {
			t.Errorf("want size-too-small error, got %v", err)
		}
	})

	t.Run("bad size string", func(t *testing.T) {
		_, err := computePartitions(recipe.MediaRecipe{
			Size: "huge",
		})
		if err == nil {
			t.Error("want error for bad size")
		}
	})

	t.Run("estimateStoreUsage warns when undersized", func(t *testing.T) {
		// 30 G recipe + 3 ostree envs (10 G each estimated) -> 30 > 27 store
		needed, store, ok := estimateStoreUsage(recipe.MediaRecipe{
			Size: "30G",
			BootableEnvironments: []recipe.BootableEnvironment{
				{ID: "a", Image: "x"},
				{ID: "b", Image: "y"},
				{ID: "c", Image: "z"},
			},
		})
		if !ok {
			t.Fatal("estimate failed")
		}
		if needed <= store {
			t.Errorf("expected estimated %d > store %d for 3-env 30G recipe", needed, store)
		}
	})

	t.Run("estimateStoreUsage ok for sized recipe", func(t *testing.T) {
		// 60 G recipe + 3 ostree envs -> 30 <= 57 store
		needed, store, ok := estimateStoreUsage(recipe.MediaRecipe{
			Size: "60G",
			BootableEnvironments: []recipe.BootableEnvironment{
				{ID: "a", Image: "x"},
				{ID: "b", Image: "y"},
				{ID: "c", Image: "z"},
			},
		})
		if !ok || needed > store {
			t.Errorf("60G recipe should fit 3 envs: needed=%d store=%d", needed, store)
		}
	})

	t.Run("composefs envs estimated smaller", func(t *testing.T) {
		ostreeNeed, _, _ := estimateStoreUsage(recipe.MediaRecipe{
			Size: "60G",
			BootableEnvironments: []recipe.BootableEnvironment{
				{ID: "a", Image: "x"},
			},
		})
		composeNeed, _, _ := estimateStoreUsage(recipe.MediaRecipe{
			Size: "60G",
			BootableEnvironments: []recipe.BootableEnvironment{
				{ID: "a", Image: "x", Backend: "composefs"},
			},
		})
		if composeNeed >= ostreeNeed {
			t.Errorf("composefs estimate (%d) should be smaller than ostree (%d)", composeNeed, ostreeNeed)
		}
	})

	t.Run("partition overrides honoured", func(t *testing.T) {
		parts, err := computePartitions(recipe.MediaRecipe{
			Size:        "40G",
			SharedStore: recipe.SharedStore{Format: "btrfs"},
			Partitions:  recipe.Partitions{ESP: "2G", Persist: "8G"},
		})
		if err != nil {
			t.Fatalf("unexpected: %v", err)
		}
		if parts[0].Size != "+2G" {
			t.Errorf("ESP override lost: %q", parts[0].Size)
		}
		// Persist override -> not remainder, store fills the middle: 40-2-8=30
		if parts[1].Size != "+30G" {
			t.Errorf("STORE = %q, want +30G", parts[1].Size)
		}
		if parts[2].Size != "+8G" {
			t.Errorf("PERSIST = %q, want +8G", parts[2].Size)
		}
	})

	t.Run("explicit STORE override", func(t *testing.T) {
		parts, err := computePartitions(recipe.MediaRecipe{
			Size:        "60G",
			SharedStore: recipe.SharedStore{Format: "ext4"},
			Partitions:  recipe.Partitions{Store: "40G"},
		})
		if err != nil {
			t.Fatalf("unexpected: %v", err)
		}
		if parts[1].Size != "+40G" {
			t.Errorf("STORE = %q, want +40G", parts[1].Size)
		}
		// PERSIST remains remainder (no override)
		if parts[2].Size != "0" {
			t.Errorf("PERSIST should be remainder, got %q", parts[2].Size)
		}
	})

	t.Run("invalid override rejected", func(t *testing.T) {
		_, err := computePartitions(recipe.MediaRecipe{
			Size:       "60G",
			Partitions: recipe.Partitions{ESP: "huge"},
		})
		if err == nil || !strings.Contains(err.Error(), "partitions.esp") {
			t.Errorf("want partitions.esp error, got %v", err)
		}
	})

	t.Run("STORE scales with recipe", func(t *testing.T) {
		for _, recipeSize := range []string{"10G", "16G", "64G", "128G"} {
			parts, err := computePartitions(recipe.MediaRecipe{
				Size:        recipeSize,
				SharedStore: recipe.SharedStore{Format: "btrfs"},
			})
			if err != nil {
				t.Errorf("%s: %v", recipeSize, err)
				continue
			}
			// STORE size should grow with recipe size.
			if !strings.HasPrefix(parts[1].Size, "+") {
				t.Errorf("%s: STORE size %q missing +", recipeSize, parts[1].Size)
			}
		}
	})
}

func TestEnvTitle(t *testing.T) {
	withTitle := recipe.BootableEnvironment{ID: "bluefin", Title: "Bluefin (GNOME)"}
	if got := envTitle(withTitle, "live"); got != "Bluefin (GNOME) (live)" {
		t.Errorf("envTitle with title = %q", got)
	}
	noTitle := recipe.BootableEnvironment{ID: "bazzite"}
	if got := envTitle(noTitle, "persistent"); got != "bazzite (persistent)" {
		t.Errorf("envTitle without title = %q", got)
	}
}

func TestBuildLiveKernelCmdlineCombined(t *testing.T) {
	got := buildLiveKernelCmdlineCombined("bazzite", "TBX_ISO")
	for _, want := range []string{
		"root=tbox:CDLABEL=TBX_ISO",
		"tacklebox.live.squashimg=combined.rootfs.sfs",
		"tacklebox.root=bazzite",
		"tacklebox.env=bazzite",
		"tacklebox.live.overlay.size=8192",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("combined cmdline missing %q: %s", want, got)
		}
	}
	// The plain per-env variant must NOT carry the pivot arg — tbox-root
	// would try to rebase /sysroot onto a subdir that doesn't exist.
	if plain := buildLiveKernelCmdline("bazzite", "TBX_ISO"); strings.Contains(plain, "tacklebox.root=") {
		t.Errorf("per-env cmdline must not set tacklebox.root: %s", plain)
	}
}

func TestAppendKargs(t *testing.T) {
	got := appendKargs("root=live:CDLABEL=X quiet", []string{"console=ttyS0", "", "  ", "console=tty0"})
	want := "root=live:CDLABEL=X quiet console=ttyS0 console=tty0"
	if got != want {
		t.Fatalf("appendKargs = %q, want %q", got, want)
	}
	if appendKargs("a", nil) != "a" {
		t.Fatal("nil kargs must be a no-op")
	}
}
