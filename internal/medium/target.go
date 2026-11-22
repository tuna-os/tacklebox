package medium

import (
	"errors"
	"fmt"
	"io"
	"os"
)

// Sink receives the raw image bytes: random-access writes, an explicit
// size, and a flush. The write loop streams chunks through at most one
// 1 MiB buffer however large the image is; a Sink must never hold the
// image in memory.
//
// A Sink may also implement io.ReaderAt. When it does, resume can skip
// chunks the target already holds and the writer reads the target back
// to prove the bytes. A write-only handle (a browser save-file stream)
// simply does not implement it: then resume is rejected and ReadBackOK
// stays false.
type Sink interface {
	io.WriterAt
	Truncate(size int64) error
	Sync() error
}

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

type Target struct {
	F      *os.File
	Path   string
	Device bool
	// Fresh is true for a file that was empty or created now. Only a fresh
	// file may be written sparse, because holes keep old bytes otherwise.
	Fresh bool
	// Sparse lets whole 4 KiB zero blocks stay holes. Set for fresh
	// files only; devices and caller-provided sinks are written dense.
	Sparse bool
	// sink overrides the *os.File as the write destination. Nil means
	// fileSink{F}: every target opened here writes to its file.
	sink Sink
}

// Sink returns the write destination: the caller's sink, or the file.
func (t *Target) Sink() Sink {
	if t.sink != nil {
		return t.sink
	}
	return fileSink{t.F}
}

// NewStreamTarget wraps a caller-provided sink, e.g. a browser handle:
// no device policy, no sparseness, no resume. The stream is written
// dense from offset 0.
func NewStreamTarget(s Sink, path string) *Target {
	return &Target{Path: path, Fresh: true, sink: s}
}

// fileSink adapts *os.File to Sink (and io.ReaderAt for resume/read-back).
type fileSink struct{ f *os.File }

func (s fileSink) WriteAt(p []byte, off int64) (int, error) { return s.f.WriteAt(p, off) }
func (s fileSink) ReadAt(p []byte, off int64) (int, error)  { return s.f.ReadAt(p, off) }
func (s fileSink) Truncate(size int64) error                { return s.f.Truncate(size) }
func (s fileSink) Sync() error                              { return s.f.Sync() }

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
		return &Target{F: f, Path: path, Fresh: true, Sparse: true}, nil
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
		return &Target{F: f, Path: path, Fresh: st.Size() == 0, Sparse: st.Size() == 0}, nil
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
