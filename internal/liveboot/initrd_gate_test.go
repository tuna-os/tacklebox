package liveboot

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strings"
	"testing"
)

func gateInventory() (*InitrdInventory, X13sGateSources) {
	i := &InitrdInventory{validated: true, entries: map[string]InitrdEntry{}}
	s := X13sGateSources{Root: []byte("root"), Helper: []byte("helper"), PreUdev: []byte("preudev"), Cmdline: []byte("cmdline")}
	for name, data := range map[string][]byte{
		"usr/sbin/tbox-live-root": s.Root, "usr/lib/tbox-x13s-adsp.sh": s.Helper,
		"var/lib/dracut/hooks/pre-udev/30-tbox-x13s-adsp-pre-udev.sh": s.PreUdev,
		"var/lib/dracut/hooks/cmdline/30-parse-tbox-live.sh":          s.Cmdline,
		"etc/tacklebox/x13s-adsp-required":                            []byte(Board + "\n"),
	} {
		parts := strings.Split(name, "/")
		for n := 1; n < len(parts); n++ {
			p := strings.Join(parts[:n], "/")
			i.entries[p] = InitrdEntry{Path: p, Type: InitrdDirectory, Mode: 0040755}
		}
		h := sha256.Sum256(data)
		i.entries[name] = InitrdEntry{Path: name, Type: InitrdRegular, Mode: 0100755, Size: int64(len(data)), SHA256: hex.EncodeToString(h[:])}
	}
	i.entries["lib"] = InitrdEntry{Path: "lib", Type: InitrdSymlink, LinkTarget: "usr/lib"}
	i.entries["sbin"] = InitrdEntry{Path: "sbin", Type: InitrdSymlink, LinkTarget: "usr/sbin"}
	i.entries["run"] = InitrdEntry{Path: "run", Type: InitrdDirectory, Mode: 0040755}
	return i, s
}
func TestVerifyX13sGate(t *testing.T) {
	i, s := gateInventory()
	if err := VerifyX13sGate(i, s); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*InitrdInventory, *X13sGateSources){
		"absent helper":   func(i *InitrdInventory, _ *X13sGateSources) { delete(i.entries, "usr/lib/tbox-x13s-adsp.sh") },
		"source mismatch": func(_ *InitrdInventory, s *X13sGateSources) { s.Root = []byte("other") },
		"source omitted":  func(_ *InitrdInventory, s *X13sGateSources) { s.PreUdev = nil },
		"marker bytes": func(i *InitrdInventory, _ *X13sGateSources) {
			e := i.entries["etc/tacklebox/x13s-adsp-required"]
			e.SHA256 = strings.Repeat("0", 64)
			i.entries[e.Path] = e
		},
		"non executable": func(i *InitrdInventory, _ *X13sGateSources) {
			e := i.entries["usr/sbin/tbox-live-root"]
			e.Mode = 0100644
			i.entries[e.Path] = e
		},
		"writable": func(i *InitrdInventory, _ *X13sGateSources) {
			e := i.entries["usr/sbin/tbox-live-root"]
			e.Mode = 0100777
			i.entries[e.Path] = e
		},
		"wrong owner": func(i *InitrdInventory, _ *X13sGateSources) {
			e := i.entries["usr/sbin/tbox-live-root"]
			e.UID = 1000
			i.entries[e.Path] = e
		},
		"runtime success": func(i *InitrdInventory, _ *X13sGateSources) {
			p := "run/tbox-x13s-adsp-pre-udev-loaded"
			i.entries[p] = InitrdEntry{Path: p, Type: InitrdRegular}
		},
		"broken done link": func(i *InitrdInventory, _ *X13sGateSources) {
			p := "run/tacklebox-live-done"
			i.entries[p] = InitrdEntry{Path: p, Type: InitrdSymlink, LinkTarget: "absent"}
		},
		"run alias": func(i *InitrdInventory, _ *X13sGateSources) {
			i.entries["run"] = InitrdEntry{Path: "run", Type: InitrdSymlink, LinkTarget: "/"}
		},
		"unvalidated": func(i *InitrdInventory, _ *X13sGateSources) { i.validated = false },
	} {
		t.Run(name, func(t *testing.T) {
			i, s := gateInventory()
			mutate(i, &s)
			if err := VerifyX13sGate(i, s); err == nil {
				t.Fatal("invalid gate accepted")
			}
		})
	}
}

func TestReadArchiveThenVerifyX13sGate(t *testing.T) {
	inventory, sources := gateInventory()
	payloads := map[string]string{
		"usr/sbin/tbox-live-root":                                     string(sources.Root),
		"usr/lib/tbox-x13s-adsp.sh":                                   string(sources.Helper),
		"var/lib/dracut/hooks/pre-udev/30-tbox-x13s-adsp-pre-udev.sh": string(sources.PreUdev),
		"var/lib/dracut/hooks/cmdline/30-parse-tbox-live.sh":          string(sources.Cmdline),
		"etc/tacklebox/x13s-adsp-required":                            Board + "\n",
	}
	for _, corrupt := range []bool{false, true} {
		var entries []archiveTestEntry
		names := make([]string, 0, len(inventory.entries))
		for name := range inventory.entries {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			entry := inventory.entries[name]
			switch entry.Type {
			case InitrdDirectory:
				entries = append(entries, archiveDirectory(name))
			case InitrdSymlink:
				entries = append(entries, archiveSymlink(name, entry.LinkTarget))
			case InitrdRegular:
				data := payloads[name]
				if corrupt && name == "usr/lib/tbox-x13s-adsp.sh" {
					data = "changed helper"
				}
				e := archiveRegular(name, data)
				e.fields[1] = entry.Mode
				entries = append(entries, e)
			}
		}
		observed, err := ReadInitrdArchive(bytes.NewReader(archiveFixture(entries...)), InitrdArchiveOptions{})
		if err != nil {
			t.Fatal(err)
		}
		err = VerifyX13sGate(observed, sources)
		if (err != nil) != corrupt {
			t.Fatalf("corrupt=%v: %v", corrupt, err)
		}
		if observed.Size() == 0 || !validDigest(observed.SHA256()) {
			t.Fatal("missing actual archive receipt")
		}
	}
}
