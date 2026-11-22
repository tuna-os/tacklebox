package medium

import (
	"errors"
	"fmt"
	"os"
)

// TargetPolicy is the caller's explicit consent. The zero value is the
// safest: only a missing or empty regular file is accepted.
type TargetPolicy struct {
	// OverwriteFile allows a regular file that is not empty.
	OverwriteFile bool
	// ConfirmDevice must equal the target path to allow a block device.
	ConfirmDevice string
	// AllowFixedDisk allows a block device that is not removable. It is
	// separate from ConfirmDevice on purpose.
	AllowFixedDisk bool
	// Inspect describes a block device. Nil uses the host implementation.
	Inspect func(path string) (DeviceInfo, error)
}

// DeviceInfo is what the safety checks need to know about a block device.
type DeviceInfo struct {
	Removable bool
	// Mounted lists mount points of the device or any partition of it.
	Mounted []string
}

// Target is an opened destination.
type Target struct {
	F      *os.File
	Path   string
	Device bool
	// Fresh is true for a file that was empty or created now. Only a fresh
	// file may be written sparse, because holes keep old bytes otherwise.
	Fresh bool
}

// ErrRefused wraps every safety refusal.
var ErrRefused = errors.New("refusing to write target")

func refuse(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrRefused, fmt.Sprintf(format, a...))
}

// OpenTarget applies the policy and opens the target for writing. resume
// means an existing non-empty file is expected, so OverwriteFile is implied
// for regular files; devices still need confirmation.
func OpenTarget(path string, p TargetPolicy, resume bool) (*Target, error) {
	st, err := os.Stat(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o644)
		if err != nil {
			return nil, err
		}
		return &Target{F: f, Path: path, Fresh: true}, nil
	case err != nil:
		return nil, err
	}
	switch {
	case st.Mode().IsRegular():
		if st.Size() > 0 && !p.OverwriteFile && !resume {
			return nil, refuse("%s is a non-empty file (%d bytes); pass --force-overwrite or --resume", path, st.Size())
		}
		f, err := os.OpenFile(path, os.O_RDWR, 0)
		if err != nil {
			return nil, err
		}
		return &Target{F: f, Path: path, Fresh: st.Size() == 0}, nil
	case st.Mode()&os.ModeDevice != 0 && st.Mode()&os.ModeCharDevice == 0:
		if p.ConfirmDevice != path {
			return nil, refuse("%s is a block device; pass --confirm-device=%s to write it", path, path)
		}
		inspect := p.Inspect
		if inspect == nil {
			inspect = inspectDevice
		}
		info, err := inspect(path)
		if err != nil {
			return nil, refuse("cannot inspect %s: %v", path, err)
		}
		if len(info.Mounted) > 0 {
			return nil, refuse("%s is in use, mounted at %v", path, info.Mounted)
		}
		if !info.Removable && !p.AllowFixedDisk {
			return nil, refuse("%s is not a removable device; pass --allow-fixed-disk as well if you are sure", path)
		}
		f, err := os.OpenFile(path, os.O_RDWR|os.O_EXCL, 0)
		if err != nil {
			return nil, err
		}
		return &Target{F: f, Path: path, Device: true}, nil
	default:
		return nil, refuse("%s is neither a regular file nor a block device", path)
	}
}
