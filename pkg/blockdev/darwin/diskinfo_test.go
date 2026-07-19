package darwin

import (
	"os"
	"testing"
)

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return b
}

func TestDecodeDiskList(t *testing.T) {
	ids, err := DecodeDiskList(mustRead(t, "testdata/diskutil-list.plist"))
	if err != nil {
		t.Fatalf("DecodeDiskList: %v", err)
	}
	// Captured from a real machine with 5 whole disks attached (internal
	// SSD, two APFS containers, a Time Machine-style volume, and the
	// hdiutil test image created during this research).
	if len(ids) == 0 {
		t.Fatal("expected at least one whole disk, got none")
	}
	found := map[string]bool{}
	for _, id := range ids {
		found[id] = true
	}
	if !found["disk0"] {
		t.Errorf("expected disk0 (internal SSD) in WholeDisks, got %v", ids)
	}
	if !found["disk4"] {
		t.Errorf("expected disk4 (hdiutil test image) in WholeDisks, got %v", ids)
	}
}

// TestIsSafeWriteTarget_RealFixtures is the core safety-filter test: it
// must reject the real internal SSD and the real virtual disk image
// captured from actual hardware, and accept the (synthetic, pending real
// hardware) USB stick. This is the single most safety-critical assertion
// in this package — see the package doc comment for why it has to be
// testable outside a live macOS run.
func TestIsSafeWriteTarget_RealFixtures(t *testing.T) {
	cases := []struct {
		name       string
		fixture    string
		wantSafe   bool
		wantReason string // substring, empty means "don't check"
	}{
		{
			name:       "internal SSD must be rejected",
			fixture:    "testdata/diskutil-info-internal-ssd.plist",
			wantSafe:   false,
			wantReason: "internal",
		},
		{
			name:       "hdiutil virtual disk image must be rejected",
			fixture:    "testdata/diskutil-info-virtual-dmg.plist",
			wantSafe:   false,
			wantReason: "virtual",
		},
		{
			name:     "USB stick (synthetic, see fixture comment) must be accepted",
			fixture:  "testdata/diskutil-info-usb-stick-synthetic.plist",
			wantSafe: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			info, err := DecodeDiskInfo(mustRead(t, tc.fixture))
			if err != nil {
				t.Fatalf("DecodeDiskInfo: %v", err)
			}
			safe, reason := IsSafeWriteTarget(info)
			if safe != tc.wantSafe {
				t.Fatalf("IsSafeWriteTarget(%s) = (%v, %q), want safe=%v",
					tc.fixture, safe, reason, tc.wantSafe)
			}
			if tc.wantReason != "" && !contains(reason, tc.wantReason) {
				t.Errorf("reason %q does not mention %q", reason, tc.wantReason)
			}
		})
	}
}

// TestIsSafeWriteTarget_Adversarial exercises the specific attack the real
// fixtures proved matters: a device that looks removable but isn't
// physical media. This is a hand-built adversarial case (a virtual disk
// that ALSO happens to report RemovableMedia=true, which real testing
// showed is the normal case for hdiutil images, not an edge case) so a
// future regression that starts trusting RemovableMedia again gets caught
// immediately.
func TestIsSafeWriteTarget_Adversarial(t *testing.T) {
	d := DiskInfo{
		DeviceIdentifier:  "disk9",
		VirtualOrPhysical: "Virtual",
		RemovableMedia:    true, // looks removable
		Internal:          false,
		WholeDisk:         true,
		Size:              1 << 30,
	}
	safe, reason := IsSafeWriteTarget(d)
	if safe {
		t.Fatal("a Virtual device reporting RemovableMedia=true must still be rejected")
	}
	if !contains(reason, "virtual") {
		t.Errorf("expected rejection reason to mention virtual, got %q", reason)
	}
}

func TestIsSafeWriteTarget_NotWholeDisk(t *testing.T) {
	d := DiskInfo{WholeDisk: false, VirtualOrPhysical: "Physical", Size: 1 << 30}
	if safe, _ := IsSafeWriteTarget(d); safe {
		t.Fatal("a partition (WholeDisk=false) must be rejected")
	}
}

func TestIsSafeWriteTarget_ZeroSize(t *testing.T) {
	d := DiskInfo{WholeDisk: true, VirtualOrPhysical: "Physical", Size: 0}
	if safe, _ := IsSafeWriteTarget(d); safe {
		t.Fatal("a zero-size device must be rejected")
	}
}

func contains(s, substr string) bool {
	for i := 0; i+len(substr) <= len(s); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
