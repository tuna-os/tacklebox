package livemedia

import "testing"

// The paths are an on-media contract: existing ISOs and boot entries use
// them, so a change here must be deliberate.
func TestPxebootLayout(t *testing.T) {
	for _, tc := range []struct{ name, got, want string }{
		{"dir", PxebootDir("snow"), "/images/pxeboot/snow/"},
		{"kernel", KernelPath("snow"), "/images/pxeboot/snow/vmlinuz"},
		{"initrd", InitrdPath("snow"), "/images/pxeboot/snow/initrd.img"},
		{"devicetree", DevicetreePath("snow"), "/images/pxeboot/snow/devicetree"},
	} {
		if tc.got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, tc.got, tc.want)
		}
	}
}
