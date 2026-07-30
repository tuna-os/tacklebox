package purefs

import (
	"fmt"
	"io"
	"path"
	"sort"
	"strings"
	"time"
)

// Pure-Go ISO9660 + Rock Ridge + El Torito (EFI) writer.
//
// Replaces both xorriso (a shell-out) and go-diskfs's iso9660 (which
// stages the whole tree in a real-filesystem "workspace" before copying
// it into the image — double disk, and impossible under GOOS=js). This
// writer computes the complete layout up front from declared file sizes,
// then streams: descriptors → path tables → directories → boot catalog →
// file contents, each source read exactly once, sequentially.
//
// Scope is deliberately the media tacklebox authors: EFI/El Torito boot
// (no BIOS emulation), Rock Ridge NM/PX for real names and modes, no
// Joliet (the kernel mounts our xorriso ISOs nojoliet already), no
// multi-extent files above 4 GiB — the ESP, kernel, initrd and rootfs
// image are each below that; the rootfs is size-checked at author time.

const sectorSize = 2048

// IsoInput describes one file to place in the image. Size must be exact:
// the layout is computed before any content is read.
type IsoInput struct {
	Path   string // absolute ISO path, e.g. /LiveOS/root.sfs
	Size   int64
	Source func() (io.ReadCloser, error)
}

// WriteIso9660 writes a bootable ISO to w. bootImage is the ISO path of
// the El Torito EFI boot image (the ESP); it must be one of files.
func WriteIso9660(w io.Writer, volumeID string, files []IsoInput, bootImage string) error {
	ly, err := layoutIso(volumeID, files, bootImage)
	if err != nil {
		return err
	}
	return ly.write(w)
}

// ── layout ──────────────────────────────────────────────────────────────────

type isoDir struct {
	name    string
	parent  *isoDir
	subdirs []*isoDir
	files   []*isoEntry
	extent  int64 // sector of this directory's records
	size    int64 // bytes (multiple of sectorSize)
	pathIdx int   // 1-based path table index
}

type isoEntry struct {
	name   string
	size   int64
	extent int64
	src    func() (io.ReadCloser, error)
}

type isoLayout struct {
	volumeID     string
	root         *isoDir
	dirs         []*isoDir // breadth-first, root first (path table order)
	bootEntry    *isoEntry
	bootCatalog  int64 // sector
	pathTableL   int64 // sector
	pathTableM   int64
	pathTableLen int
	totalSectors int64
	files        []*isoEntry // in extent order
	now          time.Time
}

func layoutIso(volumeID string, files []IsoInput, bootImage string) (*isoLayout, error) {
	root := &isoDir{name: ""}
	dirByPath := map[string]*isoDir{"/": root}
	getDir := func(p string) *isoDir {
		if d, ok := dirByPath[p]; ok {
			return d
		}
		cur := root
		acc := ""
		for _, part := range strings.Split(strings.Trim(p, "/"), "/") {
			acc += "/" + part
			d, ok := dirByPath[acc]
			if !ok {
				d = &isoDir{name: part, parent: cur}
				cur.subdirs = append(cur.subdirs, d)
				dirByPath[acc] = d
			}
			cur = d
		}
		return cur
	}

	ly := &isoLayout{volumeID: volumeID, root: root, now: time.Now().UTC()}
	for i := range files {
		f := files[i]
		clean := path.Clean("/" + f.Path)
		e := &isoEntry{name: path.Base(clean), size: f.Size, src: f.Source}
		getDir(path.Dir(clean)).files = append(getDir(path.Dir(clean)).files, e)
		if clean == path.Clean("/"+bootImage) {
			ly.bootEntry = e
		}
	}
	if ly.bootEntry == nil {
		return nil, fmt.Errorf("boot image %s not among inputs", bootImage)
	}

	// Breadth-first directory list = ISO9660 path table order.
	queue := []*isoDir{root}
	for len(queue) > 0 {
		d := queue[0]
		queue = queue[1:]
		sort.Slice(d.subdirs, func(i, j int) bool { return d.subdirs[i].name < d.subdirs[j].name })
		sort.Slice(d.files, func(i, j int) bool { return d.files[i].name < d.files[j].name })
		d.pathIdx = len(ly.dirs) + 1
		ly.dirs = append(ly.dirs, d)
		queue = append(queue, d.subdirs...)
	}

	// Directory sizes need extents, extents need sizes — but record
	// lengths don't depend on extent values, so size first, place after.
	for _, d := range ly.dirs {
		d.size = ceilSector(ly.dirContentLen(d))
	}
	ptLen := 0
	for _, d := range ly.dirs {
		ptLen += pathTableRecordLen(d)
	}
	ly.pathTableLen = ptLen

	// Placement: 16 system + 3 descriptors → path tables → dirs →
	// boot catalog → files.
	sector := int64(16 + 3)
	ly.pathTableL = sector
	sector += ceilSector(int64(ptLen)) / sectorSize
	ly.pathTableM = sector
	sector += ceilSector(int64(ptLen)) / sectorSize
	for _, d := range ly.dirs {
		d.extent = sector
		sector += d.size / sectorSize
	}
	ly.bootCatalog = sector
	sector++
	for _, d := range ly.dirs {
		for _, f := range d.files {
			f.extent = sector
			sector += ceilSector(f.size) / sectorSize
			ly.files = append(ly.files, f)
		}
	}
	ly.totalSectors = sector
	return ly, nil
}

func ceilSector(n int64) int64 {
	return (n + sectorSize - 1) / sectorSize * sectorSize
}

// ── record encoding ─────────────────────────────────────────────────────────

// isoName is the ISO9660-level identifier (uppercased, restricted set);
// the Rock Ridge NM entry carries the real name.
func isoName(name string, isDir bool) string {
	up := strings.ToUpper(name)
	var b strings.Builder
	for _, r := range up {
		switch {
		case r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '.':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	s := b.String()
	if !isDir && !strings.Contains(s, ".") {
		s += "."
	}
	if !isDir {
		s += ";1"
	}
	if len(s) > 30 {
		s = s[len(s)-30:]
	}
	return s
}

// dirContentLen sizes a directory by encoding its records with dummy
// extents — record length is independent of extent values, so sizing
// and writing can never disagree.
func (ly *isoLayout) dirContentLen(d *isoDir) int64 {
	var used, inSector int64
	add := func(rec []byte) {
		rl := int64(len(rec))
		if inSector+rl > sectorSize {
			used += sectorSize - inSector
			inSector = 0
		}
		used += rl
		inSector = (inSector + rl) % sectorSize
	}
	dotName := ""
	if d.parent == nil {
		dotName = "SP"
	}
	add(ly.dirRecord(dotName, 0, 0, true, 1, false))
	add(ly.dirRecord("", 0, 0, true, 2, false))
	for _, s := range d.subdirs {
		add(ly.dirRecord(s.name, 0, 0, true, 0, false))
	}
	for _, f := range d.files {
		// One record per extent — the layout pass only needs the count and
		// the name (record length is name-derived), but it MUST match what
		// the write pass emits or directory sizes drift and the image is
		// silently malformed.
		for range extentSpans(f.size) {
			add(ly.dirRecord(f.name, 0, 0, false, 0, false))
		}
	}
	return used
}

func pathTableRecordLen(d *isoDir) int {
	id := isoName(d.name, true)
	if d.parent == nil {
		id = "\x00"
	}
	l := 8 + len(id)
	if l%2 == 1 {
		l++
	}
	return l
}

func both32(v uint32) []byte {
	return []byte{
		byte(v), byte(v >> 8), byte(v >> 16), byte(v >> 24),
		byte(v >> 24), byte(v >> 16), byte(v >> 8), byte(v),
	}
}

func both16(v uint16) []byte {
	return []byte{byte(v), byte(v >> 8), byte(v >> 8), byte(v)}
}

func recTime(t time.Time) []byte {
	return []byte{
		byte(t.Year() - 1900), byte(t.Month()), byte(t.Day()),
		byte(t.Hour()), byte(t.Minute()), byte(t.Second()), 0,
	}
}

// dirRecord encodes one directory record (with RR) into buf.
// maxExtent is the largest file span one directory record can describe: the
// size field is 32-bit, so the ceiling is the biggest multiple of the sector
// size below 2^32. Anything larger needs several records.
const maxExtent = 0xFFFFF800 // 4294965248 = (2^32 - 1) rounded down to 2048

// extentSpans splits a file into the (offset, length) pairs that each get
// their own directory record. Always returns at least one span, so a zero
// length file still gets a record.
//
// ISO9660 Level 3 multi-extent: a file too large for one record is described
// by consecutive records with the same name, each covering the next chunk,
// with the multi-extent bit set in File Flags on every record but the last.
// Readers concatenate them. Without this the writer had to reject the file —
// which is what made a 4.9 GB rootfs produce a 0-byte ISO
// (tuna-os/tacklebox#158), and forced the ISO Builder and purebuild to shell
// out to xorriso for any desktop edition.
func extentSpans(size int64) [][2]int64 {
	if size <= maxExtent {
		return [][2]int64{{0, size}}
	}
	var spans [][2]int64
	for off := int64(0); off < size; off += maxExtent {
		n := size - off
		if n > maxExtent {
			n = maxExtent
		}
		spans = append(spans, [2]int64{off, n})
	}
	return spans
}

func (ly *isoLayout) dirRecord(name string, extent, size int64, isDir bool, dot int, multi bool) []byte {
	// dot: 0 = regular entry, 1 = ".", 2 = ".."
	var id string
	switch dot {
	case 1:
		id = "\x00"
	case 2:
		id = "\x01"
	default:
		id = isoName(name, isDir)
	}
	base := 33 + len(id)
	pad := 0
	if base%2 == 1 {
		pad = 1
	}

	var su []byte
	if dot == 1 && name == "SP" { // root "." carries SP + ER (RRIP 1.09)
		su = append(su, 'S', 'P', 7, 1, 0xBE, 0xEF, 0)
		// Minimal ER: identifier only — the full-text description would
		// push the root "." record past the 255-byte record limit.
		er := []byte{'E', 'R', 0, 1, 10, 0, 0, 1}
		er = append(er, "RRIP_1991A"...)
		er[2] = byte(len(er))
		su = append(su, er...)
	}
	// RR overview entry (deprecated in 1.12 but keys many readers,
	// including xorriso, onto Rock Ridge processing)
	rrFlags := byte(0x01 | 0x08) // PX + NM
	if dot != 0 {
		rrFlags = 0x01
	}
	su = append(su, 'R', 'R', 5, 1, rrFlags)
	// PX: POSIX attributes
	mode := uint32(0o100444)
	nlink := uint32(1)
	if isDir {
		mode = 0o040555
		nlink = 2
	}
	px := []byte{'P', 'X', 36, 1}
	px = append(px, both32(mode)...)
	px = append(px, both32(nlink)...)
	px = append(px, both32(0)...) // uid
	px = append(px, both32(0)...) // gid
	su = append(su, px...)
	if dot == 0 {
		nm := []byte{'N', 'M', byte(5 + len(name)), 1, 0}
		nm = append(nm, name...)
		su = append(su, nm...)
	}
	if (base+pad+len(su))%2 == 1 {
		su = append(su, 0)
	}

	rl := base + pad + len(su)
	rec := make([]byte, rl)
	rec[0] = byte(rl)
	copy(rec[2:10], both32(uint32(extent)))
	copy(rec[10:18], both32(uint32(size)))
	copy(rec[18:25], recTime(ly.now))
	if isDir {
		rec[25] = 0x02
	}
	if multi {
		// Bit 7: "not the final extent of this file".
		rec[25] |= 0x80
	}
	copy(rec[28:32], both16(1)) // volume sequence number
	rec[32] = byte(len(id))
	copy(rec[33:], id)
	copy(rec[33+len(id)+pad:], su)
	return rec
}

// ── writing ─────────────────────────────────────────────────────────────────

type sectorWriter struct {
	w   io.Writer
	pos int64 // bytes written
	err error
}

func (sw *sectorWriter) write(b []byte) {
	if sw.err != nil {
		return
	}
	n, err := sw.w.Write(b)
	sw.pos += int64(n)
	sw.err = err
}

func (sw *sectorWriter) padTo(sector int64) {
	if sw.err != nil {
		return
	}
	want := sector * sectorSize
	if sw.pos > want {
		sw.err = fmt.Errorf("iso layout overrun: at %d, want sector %d", sw.pos, sector)
		return
	}
	zeros := make([]byte, 32*1024)
	for sw.pos < want && sw.err == nil {
		n := want - sw.pos
		if n > int64(len(zeros)) {
			n = int64(len(zeros))
		}
		sw.write(zeros[:n])
	}
}

func (ly *isoLayout) write(w io.Writer) error {
	sw := &sectorWriter{w: w}

	// Sectors 0-15: system area.
	sw.padTo(16)

	// PVD.
	pvd := make([]byte, sectorSize)
	pvd[0] = 1
	copy(pvd[1:6], "CD001")
	pvd[6] = 1
	copy(pvd[8:40], padStr("TACKLEBOX", 32))
	copy(pvd[40:72], padStr(ly.volumeID, 32))
	copy(pvd[80:88], both32(uint32(ly.totalSectors)))
	copy(pvd[120:124], both16(1)) // set size
	copy(pvd[124:128], both16(1)) // sequence number
	copy(pvd[128:132], both16(sectorSize))
	copy(pvd[132:140], both32(uint32(ly.pathTableLen)))
	le32(pvd[140:144], uint32(ly.pathTableL))
	be32(pvd[148:152], uint32(ly.pathTableM))
	rootRec := ly.rootRecordForPVD()
	copy(pvd[156:156+34], rootRec)
	copy(pvd[190:318], padStr("", 128))          // volume set
	copy(pvd[318:446], padStr("TACKLEBOX", 128)) // publisher
	copy(pvd[446:574], padStr("", 128))
	copy(pvd[574:702], padStr("TACKLEBOX PUREFS", 128))
	copy(pvd[813:830], decDatetime(ly.now))
	copy(pvd[830:847], decDatetime(ly.now))
	copy(pvd[847:863], padStr("", 16))
	copy(pvd[864:880], padStr("", 16))
	pvd[881] = 1
	sw.write(pvd)

	// El Torito boot record volume descriptor.
	brvd := make([]byte, sectorSize)
	brvd[0] = 0
	copy(brvd[1:6], "CD001")
	brvd[6] = 1
	copy(brvd[7:39], padStr("EL TORITO SPECIFICATION", 32))
	le32(brvd[71:75], uint32(ly.bootCatalog))
	sw.write(brvd)

	// Terminator.
	term := make([]byte, sectorSize)
	term[0] = 255
	copy(term[1:6], "CD001")
	term[6] = 1
	sw.write(term)

	// Path tables.
	writePT := func(le bool) {
		buf := make([]byte, 0, ly.pathTableLen)
		for _, d := range ly.dirs {
			id := isoName(d.name, true)
			if d.parent == nil {
				id = "\x00"
			}
			rec := make([]byte, 8+len(id))
			rec[0] = byte(len(id))
			parentIdx := uint16(1)
			if d.parent != nil {
				parentIdx = uint16(d.parent.pathIdx)
			}
			if le {
				le32(rec[2:6], uint32(d.extent))
				rec[6] = byte(parentIdx)
				rec[7] = byte(parentIdx >> 8)
			} else {
				be32(rec[2:6], uint32(d.extent))
				rec[6] = byte(parentIdx >> 8)
				rec[7] = byte(parentIdx)
			}
			copy(rec[8:], id)
			buf = append(buf, rec...)
			if len(rec)%2 == 1 {
				buf = append(buf, 0)
			}
		}
		sw.write(buf)
	}
	sw.padTo(ly.pathTableL)
	writePT(true)
	sw.padTo(ly.pathTableM)
	writePT(false)

	// Directories.
	for _, d := range ly.dirs {
		sw.padTo(d.extent)
		buf := make([]byte, 0, d.size)
		inSector := 0
		add := func(rec []byte) {
			if inSector+len(rec) > sectorSize {
				pad := make([]byte, sectorSize-inSector)
				buf = append(buf, pad...)
				inSector = 0
			}
			buf = append(buf, rec...)
			inSector = (inSector + len(rec)) % sectorSize
		}
		self := d
		parent := d.parent
		if parent == nil {
			parent = d
		}
		dotName := ""
		if d.parent == nil {
			dotName = "SP" // marker: root "." carries the SUSP SP entry
		}
		add(ly.dirRecord(dotName, self.extent, self.size, true, 1, false))
		add(ly.dirRecord("", parent.extent, parent.size, true, 2, false))
		for _, s := range d.subdirs {
			add(ly.dirRecord(s.name, s.extent, s.size, true, 0, false))
		}
		for _, f := range d.files {
			spans := extentSpans(f.size)
			for i, sp := range spans {
				// f.extent is the file's first sector; each later span starts
				// maxExtent bytes further in, which is sector-aligned by
				// construction.
				add(ly.dirRecord(f.name, f.extent+sp[0]/sectorSize, sp[1],
					false, 0, i < len(spans)-1))
			}
		}
		sw.write(buf)
	}

	// Boot catalog.
	sw.padTo(ly.bootCatalog)
	cat := make([]byte, sectorSize)
	// Validation entry, platform 0xEF (EFI).
	cat[0] = 1
	cat[1] = 0xEF
	copy(cat[4:28], padStr("TACKLEBOX", 24))
	cat[30] = 0x55
	cat[31] = 0xAA
	var sum uint16
	for i := 0; i < 32; i += 2 {
		sum += uint16(cat[i]) | uint16(cat[i+1])<<8
	}
	csum := 0x10000 - int(sum)
	cat[28] = byte(csum)
	cat[29] = byte(csum >> 8)
	// Initial/default entry: no-emulation, load the ESP.
	cat[32] = 0x88
	// sector count in 512-byte units, capped like xorriso (0 = rely on
	// firmware reading the full image from the extent).
	cnt := ly.bootEntry.size / 512
	if cnt > 0xFFFF {
		cnt = 0
	}
	cat[38] = byte(cnt)
	cat[39] = byte(cnt >> 8)
	le32(cat[40:44], uint32(ly.bootEntry.extent))
	sw.write(cat)

	// File contents.
	for _, f := range ly.files {
		sw.padTo(f.extent)
		rc, err := f.src()
		if err != nil {
			return fmt.Errorf("%s: %w", f.name, err)
		}
		n, err := io.Copy(sw.w, rc)
		rc.Close()
		sw.pos += n
		if err != nil {
			return fmt.Errorf("%s: %w", f.name, err)
		}
		if n != f.size {
			return fmt.Errorf("%s: declared %d bytes, source yielded %d", f.name, f.size, n)
		}
	}
	sw.padTo(ly.totalSectors)
	return sw.err
}

func (ly *isoLayout) rootRecordForPVD() []byte {
	// The 34-byte root record in the PVD has no Rock Ridge.
	rec := make([]byte, 34)
	rec[0] = 34
	copy(rec[2:10], both32(uint32(ly.root.extent)))
	copy(rec[10:18], both32(uint32(ly.root.size)))
	copy(rec[18:25], recTime(ly.now))
	rec[25] = 0x02
	copy(rec[28:32], both16(1))
	rec[32] = 1
	rec[33] = 0
	return rec
}

func padStr(s string, n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = ' '
	}
	copy(b, s)
	return b
}

func decDatetime(t time.Time) []byte {
	s := t.Format("2006010215040500")
	b := make([]byte, 17)
	copy(b, s)
	return b
}

func le32(b []byte, v uint32) {
	b[0], b[1], b[2], b[3] = byte(v), byte(v>>8), byte(v>>16), byte(v>>24)
}

func be32(b []byte, v uint32) {
	b[0], b[1], b[2], b[3] = byte(v>>24), byte(v>>16), byte(v>>8), byte(v)
}
