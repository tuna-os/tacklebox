package liveboot

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"hash"
	"io"
	"path"
	"strings"
)

// This parser deliberately accepts a smaller language than Linux initramfs:
// one uncompressed 070701 archive, a required trailer, and zero padding only.
// Field widths, four-byte alignment, file types, and inode/device hardlink keys
// follow https://www.kernel.org/doc/html/latest/driver-api/early-userspace/buffer-format.html.
// CRC, compression, concatenation, and extraction are not supported. Five
// conventional dracut /dev character nodes are accepted as metadata only.
// The intended producer is dracut --no-compress --no-early-microcode.

type InitrdNodeType string

const (
	InitrdRegular    InitrdNodeType = "regular"
	InitrdDirectory  InitrdNodeType = "directory"
	InitrdSymlink    InitrdNodeType = "symlink"
	InitrdCharDevice InitrdNodeType = "character-device"
)

// InitrdEntry describes observed archive data. For hardlinks, Size and SHA256
// describe the group's resolved file content, including for zero-data members.
// Mode is the complete Linux st_mode; symlink targets are opaque, never followed.
type InitrdEntry struct {
	Path       string
	Type       InitrdNodeType
	Mode       uint32
	UID        uint32
	GID        uint32
	Size       int64
	SHA256     string
	LinkTarget string
	RDevMajor  uint32
	RDevMinor  uint32
}

// InitrdArchiveLimits may only lower the default ceilings. Zero uses the default.
// Archive bytes include headers, names, payloads, trailer, and final padding.
// Name bytes bound total names and symlink targets; hardlink bytes bound temporary
// buffering needed when a retained alias follows its group's data member.
type InitrdArchiveLimits struct {
	MaxArchiveBytes  int64
	MaxEntryBytes    int64
	MaxEntries       int
	MaxNameBytes     int64
	MaxRetainedBytes int64
	MaxHardlinkBytes int64
}

type InitrdArchiveOptions struct {
	Limits      InitrdArchiveLimits
	RetainPaths []string
}

// InitrdInventory cannot be constructed as a validated observation by callers.
// Accessors copy mutable maps/slices; policy code in this package must check
// validated before relying on the private fields.
type InitrdInventory struct {
	entries       map[string]InitrdEntry
	contents      map[string][]byte
	archiveSHA256 string
	archiveSize   int64
	validated     bool
}

func (i *InitrdInventory) Entries() map[string]InitrdEntry {
	result := make(map[string]InitrdEntry)
	if i != nil && i.validated {
		for name, entry := range i.entries {
			result[name] = entry
		}
	}
	return result
}

func (i *InitrdInventory) Contents() map[string][]byte {
	result := make(map[string][]byte)
	if i != nil && i.validated {
		for name, data := range i.contents {
			result[name] = append([]byte{}, data...)
		}
	}
	return result
}

func (i *InitrdInventory) SHA256() string {
	if i == nil || !i.validated {
		return ""
	}
	return i.archiveSHA256
}

func (i *InitrdInventory) Size() int64 {
	if i == nil || !i.validated {
		return 0
	}
	return i.archiveSize
}

func initrdLimits(l InitrdArchiveLimits) (InitrdArchiveLimits, error) {
	defaults := InitrdArchiveLimits{1 << 30, 256 << 20, 100000, 16 << 20, 16 << 20, 64 << 20}
	values := []*int64{&l.MaxArchiveBytes, &l.MaxEntryBytes, &l.MaxNameBytes, &l.MaxRetainedBytes, &l.MaxHardlinkBytes}
	maxima := []int64{defaults.MaxArchiveBytes, defaults.MaxEntryBytes, defaults.MaxNameBytes, defaults.MaxRetainedBytes, defaults.MaxHardlinkBytes}
	for n, value := range values {
		if *value < 0 || *value > maxima[n] {
			return l, fmt.Errorf("invalid initrd byte limit")
		}
		if *value == 0 {
			*value = maxima[n]
		}
	}
	if l.MaxEntries < 0 || l.MaxEntries > defaults.MaxEntries {
		return l, fmt.Errorf("invalid initrd entry limit")
	}
	if l.MaxEntries == 0 {
		l.MaxEntries = defaults.MaxEntries
	}
	return l, nil
}

type initrdStream struct {
	r     io.Reader
	hash  hash.Hash
	size  int64
	limit int64
	idle  int
}

func (s *initrdStream) Read(p []byte) (int, error) {
	// Read at most one byte beyond the ceiling, to distinguish exact-size EOF.
	if int64(len(p)) > s.limit-s.size+1 {
		p = p[:s.limit-s.size+1]
	}
	n, err := s.r.Read(p)
	if n > 0 {
		s.idle = 0
		if int64(n) > s.limit-s.size {
			// Return no bytes with the error so io.ReadFull cannot suppress
			// it when the oversized read happened to fill its destination.
			return 0, fmt.Errorf("initrd exceeds archive byte limit")
		}
		s.size += int64(n)
		_, _ = s.hash.Write(p[:n])
	} else if err == nil {
		s.idle++
		if s.idle >= 100 {
			return 0, io.ErrNoProgress
		}
	}
	return n, err
}

func (s *initrdStream) padding() error {
	var b [3]byte
	n := int((-s.size) & 3)
	if _, err := io.ReadFull(s, b[:n]); err != nil {
		return err
	}
	for _, c := range b[:n] {
		if c != 0 {
			return fmt.Errorf("nonzero initrd alignment padding")
		}
	}
	return nil
}

// initrdPath permits conventional "./" and redundant slash/dot spellings, but
// never traversal. The canonical name is used for collision detection.
func initrdPath(name string) (string, error) {
	if name == "" || len(name) > 4095 || strings.HasPrefix(name, "/") || strings.ContainsAny(name, "\x00\\") {
		return "", fmt.Errorf("invalid initrd path")
	}
	for _, part := range strings.Split(name, "/") {
		if part == ".." {
			return "", fmt.Errorf("initrd path traversal")
		}
	}
	clean := path.Clean(name)
	if strings.Count(clean, "/") >= 64 {
		return "", fmt.Errorf("initrd path exceeds depth limit")
	}
	return clean, nil
}

func initrdHex(b []byte) (uint32, error) {
	var value uint32
	for _, c := range b {
		var digit byte
		switch {
		case c >= '0' && c <= '9':
			digit = c - '0'
		case c >= 'a' && c <= 'f':
			digit = c - 'a' + 10
		case c >= 'A' && c <= 'F':
			digit = c - 'A' + 10
		default:
			return 0, fmt.Errorf("invalid newc hexadecimal field")
		}
		value = value<<4 | uint32(digit)
	}
	return value, nil
}

type initrdInode struct{ major, minor, ino uint32 }

type initrdLinkGroup struct {
	mode, uid, gid, links uint32
	names                 []string
	data                  []byte
	size                  int64
	digest                string
	hasData, complete     bool
}

// ReadInitrdArchive validates and inventories the entire stream before returning
// any observation. RetainPaths must identify existing regular files. Regular
// content is hashed incrementally; unrequested ordinary file bodies are never
// retained. Regular hardlinks require consistent metadata, no more observed
// members than nlink, and at most one nonempty data member. Unobserved links may
// be outside the producer's tree. All-zero groups represent empty files.
func ReadInitrdArchive(r io.Reader, options InitrdArchiveOptions) (*InitrdInventory, error) {
	if r == nil {
		return nil, fmt.Errorf("missing initrd reader")
	}
	limits, err := initrdLimits(options.Limits)
	if err != nil {
		return nil, err
	}
	if len(options.RetainPaths) > limits.MaxEntries {
		return nil, fmt.Errorf("too many retained initrd paths")
	}
	retain := make(map[string]bool)
	var requestedNameBytes int64
	for _, raw := range options.RetainPaths {
		name, err := initrdPath(raw)
		if err != nil || name == "." || retain[name] {
			return nil, fmt.Errorf("invalid or duplicate retained initrd path")
		}
		requestedNameBytes += int64(len(raw) + 1)
		if requestedNameBytes > limits.MaxNameBytes {
			return nil, fmt.Errorf("retained initrd path budget exceeded")
		}
		retain[name] = true
	}
	inventory := &InitrdInventory{entries: make(map[string]InitrdEntry), contents: make(map[string][]byte)}
	stream := &initrdStream{r: r, hash: sha256.New(), limit: limits.MaxArchiveBytes}
	groups := make(map[initrdInode]*initrdLinkGroup)
	// Single-link inode keys cannot also describe a hardlink group.
	singleInodes := make(map[initrdInode]bool)
	var namesSize, retainedSize, pendingSize int64
	var buffer [32 << 10]byte
	store := func(name string, data []byte) error {
		if !retain[name] {
			return nil
		}
		if int64(len(data)) > limits.MaxRetainedBytes-retainedSize {
			return fmt.Errorf("initrd retained content exceeds limit")
		}
		retainedSize += int64(len(data))
		inventory.contents[name] = data
		return nil
	}
	finishGroup := func(group *initrdLinkGroup) error {
		if !group.hasData {
			empty := sha256.Sum256(nil)
			group.digest = hex.EncodeToString(empty[:])
			group.data = []byte{}
		}
		for _, member := range group.names {
			e := inventory.entries[member]
			e.Size, e.SHA256 = group.size, group.digest
			inventory.entries[member] = e
			if err := store(member, group.data); err != nil {
				return err
			}
		}
		pendingSize -= group.size
		group.data = nil
		group.complete = true
		return nil
	}
	for {
		var header [110]byte
		if _, err := io.ReadFull(stream, header[:]); err != nil {
			return nil, fmt.Errorf("initrd header or required trailer: %w", err)
		}
		if string(header[:6]) != "070701" {
			return nil, fmt.Errorf("only uncompressed newc initrd is supported")
		}
		var fields [13]uint32
		for n := range fields {
			fields[n], err = initrdHex(header[6+n*8 : 14+n*8])
			if err != nil {
				return nil, err
			}
		}
		ino, mode, uid, gid, links := fields[0], fields[1], fields[2], fields[3], fields[4]
		size, nameSize := int64(fields[6]), int64(fields[11])
		if fields[12] != 0 || nameSize < 2 || nameSize > 4096 || size > limits.MaxEntryBytes {
			return nil, fmt.Errorf("invalid newc checksum, name size, or payload size")
		}
		nameBytes := make([]byte, int(nameSize))
		if _, err := io.ReadFull(stream, nameBytes); err != nil {
			return nil, fmt.Errorf("initrd filename: %w", err)
		}
		if nameBytes[len(nameBytes)-1] != 0 || strings.IndexByte(string(nameBytes[:len(nameBytes)-1]), 0) >= 0 {
			return nil, fmt.Errorf("initrd filename must have one final NUL")
		}
		rawName := string(nameBytes[:len(nameBytes)-1])
		if err := stream.padding(); err != nil {
			return nil, fmt.Errorf("initrd name padding: %w", err)
		}
		if rawName == "TRAILER!!!" {
			if size != 0 || mode != 0 {
				return nil, fmt.Errorf("initrd trailer has data or a node type")
			}
			break
		}
		name, err := initrdPath(rawName)
		if err != nil {
			return nil, err
		}
		if _, exists := inventory.entries[name]; exists {
			return nil, fmt.Errorf("duplicate normalized initrd path %q", name)
		}
		// Linux extraction does not create missing ancestors. The strict
		// producer emits directories before children; root itself is implicit.
		for parent := path.Dir(name); parent != "."; parent = path.Dir(parent) {
			entry, exists := inventory.entries[parent]
			if !exists || entry.Type != InitrdDirectory {
				return nil, fmt.Errorf("initrd path %q lacks preceding directory %q", name, parent)
			}
		}
		namesSize += nameSize
		if len(inventory.entries) >= limits.MaxEntries || namesSize > limits.MaxNameBytes {
			return nil, fmt.Errorf("initrd entry or name budget exceeded")
		}
		if mode & ^uint32(0177777) != 0 || links == 0 {
			return nil, fmt.Errorf("unsupported initrd metadata")
		}
		entry := InitrdEntry{Path: name, Mode: mode, UID: uid, GID: gid, Size: size}
		switch mode & 0170000 {
		case 0100000:
			entry.Type = InitrdRegular
		case 0040000:
			entry.Type = InitrdDirectory
			if size != 0 {
				return nil, fmt.Errorf("initrd directory has data")
			}
		case 0120000:
			entry.Type = InitrdSymlink
			if links != 1 || size == 0 || size > 4095 {
				return nil, fmt.Errorf("invalid initrd symlink")
			}
			namesSize += size
			if namesSize > limits.MaxNameBytes {
				return nil, fmt.Errorf("initrd names and symlink targets exceed budget")
			}
		case 0020000:
			entry.Type = InitrdCharDevice
			// dracut 108 creates these nodes unless DRACUT_NO_MKNOD is set.
			// This is only a record of their metadata, never a host mknod.
			devices := map[string][2]uint32{
				"dev/null": {1, 3}, "dev/kmsg": {1, 11},
				"dev/console": {5, 1}, "dev/random": {1, 8}, "dev/urandom": {1, 9},
			}
			device, ok := devices[name]
			if !ok || size != 0 || links != 1 || fields[9] != device[0] || fields[10] != device[1] {
				return nil, fmt.Errorf("unsupported initrd character device")
			}
			entry.RDevMajor, entry.RDevMinor = fields[9], fields[10]
		default:
			return nil, fmt.Errorf("unsupported initrd node type")
		}
		if entry.Type != InitrdCharDevice && (fields[9] != 0 || fields[10] != 0) {
			return nil, fmt.Errorf("nondevice initrd node has device metadata")
		}
		if entry.Type != InitrdDirectory && (name == "." || strings.HasSuffix(rawName, "/") || strings.HasSuffix(rawName, "/.")) {
			return nil, fmt.Errorf("nondirectory initrd path has directory spelling")
		}
		if retain[name] && entry.Type != InitrdRegular {
			return nil, fmt.Errorf("retained initrd path is not regular")
		}
		var group *initrdLinkGroup
		if entry.Type == InitrdRegular {
			key := initrdInode{fields[7], fields[8], ino}
			if links == 1 {
				if groups[key] != nil {
					return nil, fmt.Errorf("inconsistent initrd hardlink count")
				}
				singleInodes[key] = true
			} else {
				if links > uint32(limits.MaxEntries) || singleInodes[key] {
					return nil, fmt.Errorf("invalid initrd hardlink group")
				}
				group = groups[key]
				if group == nil {
					group = &initrdLinkGroup{mode: mode, uid: uid, gid: gid, links: links}
					groups[key] = group
				}
				if group.complete || group.mode != mode || group.uid != uid || group.gid != gid || group.links != links || (size != 0 && group.hasData) {
					return nil, fmt.Errorf("ambiguous initrd hardlink group")
				}
				group.names = append(group.names, name)
				if size > 0 {
					if size > limits.MaxHardlinkBytes-pendingSize {
						return nil, fmt.Errorf("pending initrd hardlink content exceeds limit")
					}
					pendingSize += size
					group.hasData = true
					group.size = size
				}
			}
		}
		keep := entry.Type == InitrdSymlink || retain[name] || group != nil
		if group == nil && retain[name] && size > limits.MaxRetainedBytes-retainedSize {
			return nil, fmt.Errorf("initrd retained content exceeds limit")
		}
		var data []byte
		if keep {
			data = make([]byte, 0, int(size))
		}
		digest := sha256.New()
		for remaining := size; remaining > 0; {
			n := int64(len(buffer))
			if remaining < n {
				n = remaining
			}
			if _, err := io.ReadFull(stream, buffer[:n]); err != nil {
				return nil, fmt.Errorf("initrd payload: %w", err)
			}
			_, _ = digest.Write(buffer[:n])
			if keep {
				data = append(data, buffer[:n]...)
			}
			remaining -= n
		}
		if err := stream.padding(); err != nil {
			return nil, fmt.Errorf("initrd data padding: %w", err)
		}
		if entry.Type == InitrdRegular {
			entry.SHA256 = hex.EncodeToString(digest.Sum(nil))
		} else if entry.Type == InitrdSymlink {
			if strings.IndexByte(string(data), 0) >= 0 {
				return nil, fmt.Errorf("embedded NUL in initrd symlink")
			}
			entry.LinkTarget = string(data)
		}
		inventory.entries[name] = entry
		if group == nil {
			if err := store(name, data); err != nil {
				return nil, err
			}
		} else {
			if size != 0 {
				group.data, group.digest = data, entry.SHA256
			}
			if uint32(len(group.names)) == group.links {
				if err := finishGroup(group); err != nil {
					return nil, err
				}
			}
		}
	}
	for _, group := range groups {
		if !group.complete {
			if err := finishGroup(group); err != nil {
				return nil, err
			}
		}
	}
	for name := range inventory.entries {
		for parent := path.Dir(name); parent != "."; parent = path.Dir(parent) {
			if entry, ok := inventory.entries[parent]; ok && entry.Type != InitrdDirectory {
				return nil, fmt.Errorf("initrd path %q has nondirectory ancestor %q", name, parent)
			}
		}
	}
	for name := range retain {
		if _, exists := inventory.contents[name]; !exists {
			return nil, fmt.Errorf("required retained initrd path %q is missing", name)
		}
	}
	for {
		n, err := stream.Read(buffer[:])
		for _, b := range buffer[:n] {
			if b != 0 {
				return nil, fmt.Errorf("trailing archive or nonzero initrd data")
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
	}
	inventory.archiveSHA256 = hex.EncodeToString(stream.hash.Sum(nil))
	inventory.archiveSize, inventory.validated = stream.size, true
	return inventory, nil
}
