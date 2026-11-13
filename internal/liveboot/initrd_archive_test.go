package liveboot

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
)

type archiveTestEntry struct {
	name   string
	data   []byte
	fields [13]uint32
}

func archiveRegular(name, data string) archiveTestEntry {
	return archiveTestEntry{name: name, data: []byte(data), fields: [13]uint32{1, 0100644, 0, 0, 1}}
}

func archiveDirectory(name string) archiveTestEntry {
	e := archiveRegular(name, "")
	e.fields[1], e.fields[4] = 0040755, 2
	return e
}

func archiveSymlink(name, target string) archiveTestEntry {
	e := archiveRegular(name, target)
	e.fields[1] = 0120777
	return e
}

func archiveMember(e archiveTestEntry) []byte {
	var b bytes.Buffer
	e.fields[6], e.fields[11] = uint32(len(e.data)), uint32(len(e.name)+1)
	b.WriteString("070701")
	for _, v := range e.fields {
		fmt.Fprintf(&b, "%08x", v)
	}
	b.WriteString(e.name)
	b.WriteByte(0)
	for b.Len()%4 != 0 {
		b.WriteByte(0)
	}
	b.Write(e.data)
	for b.Len()%4 != 0 {
		b.WriteByte(0)
	}
	return b.Bytes()
}

func archiveFixture(entries ...archiveTestEntry) []byte {
	var result []byte
	for _, e := range entries {
		result = append(result, archiveMember(e)...)
	}
	return append(result, archiveMember(archiveTestEntry{name: "TRAILER!!!", fields: [13]uint32{4: 1}})...)
}

func archiveField(b []byte, field int, value uint32) []byte {
	copy(b[6+field*8:14+field*8], fmt.Sprintf("%08x", value))
	return b
}

func archiveReject(t *testing.T, b []byte, options InitrdArchiveOptions) {
	t.Helper()
	inventory, err := ReadInitrdArchive(bytes.NewReader(b), options)
	if err == nil || inventory != nil {
		t.Fatalf("returned inventory on invalid archive: %#v, %v", inventory, err)
	}
}

func TestInitrdArchiveInventory(t *testing.T) {
	file := archiveRegular("./usr/bin/helper", "observed helper\x00bytes")
	file.fields[1], file.fields[2], file.fields[3] = 0100755, 12, 34
	payload := archiveFixture(archiveDirectory("."), archiveDirectory("usr"), archiveDirectory("usr/bin"), file, archiveRegular("empty", ""), archiveSymlink("lib", "usr/lib"))
	payload = append(payload, make([]byte, 512-len(payload)%512)...)
	inventory, err := ReadInitrdArchive(&archiveChunkReader{r: bytes.NewReader(payload), chunk: 7}, InitrdArchiveOptions{RetainPaths: []string{"usr/bin/helper", "empty"}})
	if err != nil {
		t.Fatal(err)
	}
	if !inventory.validated || inventory.Size() != int64(len(payload)) || inventory.SHA256() != fmt.Sprintf("%x", sha256.Sum256(payload)) {
		t.Fatal("incorrect full archive receipt")
	}
	entries := inventory.Entries()
	helper := entries["usr/bin/helper"]
	if helper.Type != InitrdRegular || helper.Mode != 0100755 || helper.UID != 12 || helper.GID != 34 || helper.Size != int64(len(file.data)) || helper.SHA256 != fmt.Sprintf("%x", sha256.Sum256(file.data)) {
		t.Fatalf("incorrect regular metadata: %#v", helper)
	}
	if entries["lib"].Type != InitrdSymlink || entries["lib"].LinkTarget != "usr/lib" || entries["."].Type != InitrdDirectory {
		t.Fatal("incorrect symlink/directory inventory")
	}
	contents := inventory.Contents()
	if len(contents) != 2 || !bytes.Equal(contents["usr/bin/helper"], file.data) {
		t.Fatal("wrong retained content")
	}
	if content, ok := contents["empty"]; !ok || content == nil || len(content) != 0 {
		t.Fatal("retained empty file lost")
	}
	contents["usr/bin/helper"][0] ^= 1
	delete(entries, "usr/bin/helper")
	if !bytes.Equal(inventory.Contents()["usr/bin/helper"], file.data) || len(inventory.Entries()) != 6 {
		t.Fatal("accessors expose mutable inventory")
	}
	zero := new(InitrdInventory)
	if zero.Size() != 0 || zero.SHA256() != "" || len(zero.Entries()) != 0 || len(zero.Contents()) != 0 {
		t.Fatal("unvalidated inventory exposed")
	}
}

func TestInitrdArchiveFormatsAndTrailer(t *testing.T) {
	valid := archiveFixture(archiveRegular("file", "data"))
	tests := map[string][]byte{
		"empty":            nil,
		"gzip":             {0x1f, 0x8b, 8},
		"xz":               {0xfd, '7', 'z', 'X', 'Z', 0},
		"old ASCII":        append([]byte("070707"), valid[6:]...),
		"CRC":              append([]byte("070702"), valid[6:]...),
		"leading zeros":    append([]byte{0, 0, 0, 0}, valid...),
		"missing trailer":  archiveMember(archiveRegular("file", "data")),
		"concatenated":     append(append([]byte{}, valid...), valid...),
		"padded concat":    append(append(append([]byte{}, valid...), 0, 0, 0, 0), valid...),
		"trailing garbage": append(append([]byte{}, valid...), 0, 0, 1),
		"trailer payload":  archiveMember(archiveTestEntry{name: "TRAILER!!!", data: []byte("x")}),
		"symlink trailer":  archiveMember(archiveTestEntry{name: "TRAILER!!!", fields: [13]uint32{1: 0120777, 4: 1}}),
	}
	for name, b := range tests {
		t.Run(name, func(t *testing.T) { archiveReject(t, b, InitrdArchiveOptions{}) })
	}
	for cut := range valid {
		t.Run(fmt.Sprintf("truncated-%d", cut), func(t *testing.T) {
			archiveReject(t, valid[:cut], InitrdArchiveOptions{})
		})
	}
	for _, padding := range []int{0, 1, 3, 4, 511, 512} {
		if _, err := ReadInitrdArchive(bytes.NewReader(append(append([]byte{}, valid...), make([]byte, padding)...)), InitrdArchiveOptions{}); err != nil {
			t.Fatalf("rejected %d zero padding bytes: %v", padding, err)
		}
	}
}

func TestInitrdArchiveMalformedHeadersAndPadding(t *testing.T) {
	for field := 0; field < 13; field++ {
		for _, char := range []byte{'g', '-', '+', ' ', '\x00'} {
			t.Run(fmt.Sprintf("field-%d-%02x", field, char), func(t *testing.T) {
				b := archiveFixture(archiveRegular("file", "x"))
				b[6+field*8] = char
				archiveReject(t, b, InitrdArchiveOptions{})
			})
		}
	}
	for _, tc := range []struct {
		name  string
		field int
		value uint32
	}{
		{"checksum", 12, 1}, {"empty name", 11, 0}, {"NUL-only name", 11, 1}, {"huge name", 11, 4097},
		{"huge body", 6, ^uint32(0)}, {"zero links", 4, 0}, {"high mode bits", 1, 0xffffffff},
		{"rdev major", 9, 1}, {"rdev minor", 10, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			archiveReject(t, archiveField(archiveFixture(archiveRegular("file", "x")), tc.field, tc.value), InitrdArchiveOptions{})
		})
	}
	for _, mutate := range []func([]byte){
		func(b []byte) { b[114] = 'x' }, // filename terminator
		func(b []byte) { b[115] = 1 },   // filename padding
		func(b []byte) { b[117] = 1 },   // payload padding
	} {
		b := archiveFixture(archiveRegular("file", "x"))
		mutate(b)
		archiveReject(t, b, InitrdArchiveOptions{})
	}
	upper := archiveFixture(archiveRegular("file", "x"))
	for i := 6; i < 110; i++ {
		if upper[i] >= 'a' && upper[i] <= 'f' {
			upper[i] -= 'a' - 'A'
		}
	}
	if _, err := ReadInitrdArchive(bytes.NewReader(upper), InitrdArchiveOptions{}); err != nil {
		t.Fatal("valid uppercase hexadecimal rejected:", err)
	}
}

func TestInitrdArchivePathCollisions(t *testing.T) {
	for _, name := range []string{"", "/file", "../file", "a/../file", "a/../../file", "file\x00other", "file\x00", "a\\file", ".", "file/", "file/.", strings.Repeat("a/", 64) + "file"} {
		t.Run(fmt.Sprintf("%q", name), func(t *testing.T) {
			archiveReject(t, archiveFixture(archiveRegular(name, "x")), InitrdArchiveOptions{})
		})
	}
	for _, pair := range [][2]string{{"file", "file"}, {"file", "./file"}, {"a/file", "a//file"}, {"a/file", "a/./file"}, {"a", "a/."}} {
		archiveReject(t, archiveFixture(archiveRegular(pair[0], ""), archiveRegular(pair[1], "")), InitrdArchiveOptions{})
	}
	for _, ancestor := range []archiveTestEntry{archiveRegular("a", ""), archiveSymlink("a", "elsewhere")} {
		child := archiveRegular("a/b/c", "x")
		archiveReject(t, archiveFixture(ancestor, child), InitrdArchiveOptions{})
		archiveReject(t, archiveFixture(child, ancestor), InitrdArchiveOptions{})
	}
	if _, err := ReadInitrdArchive(bytes.NewReader(archiveFixture(archiveDirectory("./a/"), archiveRegular("a//file", "x"))), InitrdArchiveOptions{}); err != nil {
		t.Fatal("safe normalized paths rejected:", err)
	}
	archiveReject(t, archiveFixture(archiveRegular("a/file", "x"), archiveDirectory("a")), InitrdArchiveOptions{})
	archiveReject(t, archiveFixture(archiveRegular("missing/file", "x")), InitrdArchiveOptions{})
}

func TestInitrdArchiveNodeTypes(t *testing.T) {
	for _, mode := range []uint32{0, 0010644, 0020600, 0060600, 0140600} {
		e := archiveRegular("node", "")
		e.fields[1] = mode
		archiveReject(t, archiveFixture(e), InitrdArchiveOptions{})
	}
	d := archiveDirectory("dir")
	d.data = []byte("x")
	archiveReject(t, archiveFixture(d), InitrdArchiveOptions{})
	for _, target := range []string{"", "a\x00b", strings.Repeat("x", 4096)} {
		archiveReject(t, archiveFixture(archiveSymlink("link", target)), InitrdArchiveOptions{})
	}
	link := archiveSymlink("link", "target")
	link.fields[4] = 2
	archiveReject(t, archiveFixture(link), InitrdArchiveOptions{})
	for _, target := range []string{"../../outside", "/absolute/target", "target"} {
		got, err := ReadInitrdArchive(bytes.NewReader(archiveFixture(archiveSymlink("link", target))), InitrdArchiveOptions{})
		if err != nil || got.Entries()["link"].LinkTarget != target {
			t.Fatal("opaque symlink target was resolved or rejected:", err)
		}
	}
}

func TestInitrdArchiveDracutDevices(t *testing.T) {
	for name, device := range map[string][2]uint32{"dev/null": {1, 3}, "dev/kmsg": {1, 11}, "dev/console": {5, 1}, "dev/random": {1, 8}, "dev/urandom": {1, 9}} {
		t.Run(name, func(t *testing.T) {
			e := archiveRegular(name, "")
			e.fields[1], e.fields[9], e.fields[10] = 0020600, device[0], device[1]
			inventory, err := ReadInitrdArchive(bytes.NewReader(archiveFixture(archiveDirectory("dev"), e)), InitrdArchiveOptions{})
			if err != nil {
				t.Fatal(err)
			}
			got := inventory.Entries()[name]
			if got.Type != InitrdCharDevice || got.RDevMajor != device[0] || got.RDevMinor != device[1] || got.Size != 0 {
				t.Fatal("incorrect metadata-only device record")
			}
			for _, mutate := range []func(*archiveTestEntry){
				func(e *archiveTestEntry) { e.fields[9]++ },
				func(e *archiveTestEntry) { e.fields[10]++ },
				func(e *archiveTestEntry) { e.fields[4] = 2 },
				func(e *archiveTestEntry) { e.fields[1] = 0060600 },
				func(e *archiveTestEntry) { e.data = []byte("x") },
				func(e *archiveTestEntry) { e.name = "dev/unapproved" },
			} {
				bad := e
				mutate(&bad)
				archiveReject(t, archiveFixture(archiveDirectory("dev"), bad), InitrdArchiveOptions{})
			}
		})
	}
	archiveReject(t, archiveFixture(archiveSymlink("link", strings.Repeat("x", 20))), InitrdArchiveOptions{Limits: InitrdArchiveLimits{MaxNameBytes: 24}})
}

func TestInitrdArchiveHardlinks(t *testing.T) {
	for _, dataPosition := range []int{-1, 0, 1} {
		for _, links := range []uint32{2, 3} {
			t.Run(fmt.Sprintf("data-%d-nlink-%d", dataPosition, links), func(t *testing.T) {
				a, b := archiveRegular("first", ""), archiveRegular("last", "")
				a.fields[4], b.fields[4] = links, links
				want := []byte{}
				if dataPosition != -1 {
					want = []byte("one shared body")
					if dataPosition == 0 {
						a.data = want
					} else {
						b.data = want
					}
				}
				got, err := ReadInitrdArchive(bytes.NewReader(archiveFixture(a, b)), InitrdArchiveOptions{RetainPaths: []string{"first", "last"}})
				if err != nil {
					t.Fatal(err)
				}
				for _, name := range []string{"first", "last"} {
					if got.entries[name].Size != int64(len(want)) || got.entries[name].SHA256 != fmt.Sprintf("%x", sha256.Sum256(want)) || !bytes.Equal(got.contents[name], want) {
						t.Fatal("hardlink member did not resolve:", name)
					}
				}
			})
		}
	}
	for name, mutate := range map[string]func(*archiveTestEntry){
		"second payload": func(e *archiveTestEntry) { e.data = []byte("same") },
		"mode":           func(e *archiveTestEntry) { e.fields[1] = 0100600 },
		"uid":            func(e *archiveTestEntry) { e.fields[2] = 4 },
		"gid":            func(e *archiveTestEntry) { e.fields[3] = 4 },
		"link count":     func(e *archiveTestEntry) { e.fields[4] = 3 },
		"single count":   func(e *archiveTestEntry) { e.fields[4] = 1 },
	} {
		t.Run(name, func(t *testing.T) {
			a, b := archiveRegular("a", "same"), archiveRegular("b", "")
			a.fields[4], b.fields[4] = 2, 2
			mutate(&b)
			archiveReject(t, archiveFixture(a, b), InitrdArchiveOptions{})
		})
	}
	a, b, c := archiveRegular("a", "same"), archiveRegular("b", ""), archiveRegular("c", "")
	a.fields[4], b.fields[4], c.fields[4] = 2, 2, 2
	archiveReject(t, archiveFixture(a, b, c), InitrdArchiveOptions{})
	single := archiveRegular("single", "")
	archiveReject(t, archiveFixture(single, a), InitrdArchiveOptions{})
	// Device numbers distinguish inode namespaces. Other node types do not
	// collide with regular-file hardlink keys, matching Linux's type-aware key.
	b.fields[7], b.data = 4, []byte("different device")
	if _, err := ReadInitrdArchive(bytes.NewReader(archiveFixture(archiveDirectory("."), a, b)), InitrdArchiveOptions{}); err != nil {
		t.Fatal("independent inode namespaces rejected:", err)
	}
}

func TestInitrdArchiveLimitsAndRetention(t *testing.T) {
	payload := archiveFixture(archiveRegular("file", "data"))
	for name, options := range map[string]InitrdArchiveOptions{
		"negative":        {Limits: InitrdArchiveLimits{MaxArchiveBytes: -1}},
		"raise ceiling":   {Limits: InitrdArchiveLimits{MaxArchiveBytes: 1<<30 + 1}},
		"negative count":  {Limits: InitrdArchiveLimits{MaxEntries: -1}},
		"raise count":     {Limits: InitrdArchiveLimits{MaxEntries: 100001}},
		"entry bytes":     {Limits: InitrdArchiveLimits{MaxEntryBytes: 3}},
		"archive bytes":   {Limits: InitrdArchiveLimits{MaxArchiveBytes: int64(len(payload) - 1)}},
		"name bytes":      {Limits: InitrdArchiveLimits{MaxNameBytes: 4}},
		"retained bytes":  {RetainPaths: []string{"file"}, Limits: InitrdArchiveLimits{MaxRetainedBytes: 3}},
		"missing retain":  {RetainPaths: []string{"missing"}},
		"invalid retain":  {RetainPaths: []string{"../file"}},
		"alias duplicate": {RetainPaths: []string{"file", "./file"}},
		"root retain":     {RetainPaths: []string{"."}},
	} {
		t.Run(name, func(t *testing.T) { archiveReject(t, payload, options) })
	}
	archiveReject(t, archiveFixture(archiveRegular("a", ""), archiveRegular("b", "")), InitrdArchiveOptions{Limits: InitrdArchiveLimits{MaxEntries: 1}})
	archiveReject(t, archiveFixture(archiveDirectory("dir")), InitrdArchiveOptions{RetainPaths: []string{"dir"}})
	archiveReject(t, archiveFixture(archiveSymlink("link", "target")), InitrdArchiveOptions{RetainPaths: []string{"link"}})
	if got, err := ReadInitrdArchive(bytes.NewReader(payload), InitrdArchiveOptions{Limits: InitrdArchiveLimits{MaxArchiveBytes: int64(len(payload)), MaxEntryBytes: 4, MaxEntries: 1}}); err != nil || len(got.Contents()) != 0 {
		t.Fatal("exact limits or no-retention contract failed:", err)
	}
	archiveReject(t, append(append([]byte{}, payload...), 0), InitrdArchiveOptions{Limits: InitrdArchiveLimits{MaxArchiveBytes: int64(len(payload))}})
	a := archiveRegular("hardlink", "data")
	a.fields[4] = 2
	archiveReject(t, archiveFixture(a), InitrdArchiveOptions{Limits: InitrdArchiveLimits{MaxHardlinkBytes: 3}})
	b := archiveRegular("second", "")
	b.fields[4] = 2
	archiveReject(t, archiveFixture(a, b), InitrdArchiveOptions{RetainPaths: []string{"hardlink", "second"}, Limits: InitrdArchiveLimits{MaxRetainedBytes: 7}})
	c, d := archiveRegular("other-data", "more"), archiveRegular("other-alias", "")
	c.fields[0], d.fields[0], c.fields[4], d.fields[4] = 2, 2, 2, 2
	archiveReject(t, archiveFixture(a, c, b, d), InitrdArchiveOptions{Limits: InitrdArchiveLimits{MaxHardlinkBytes: 4}})
	if _, err := ReadInitrdArchive(bytes.NewReader(archiveFixture(a, b, c, d)), InitrdArchiveOptions{Limits: InitrdArchiveLimits{MaxHardlinkBytes: 4}}); err != nil {
		t.Fatal("completed hardlink-group buffer not released:", err)
	}
}

type archiveChunkReader struct {
	r     io.Reader
	chunk int
}

func (r *archiveChunkReader) Read(p []byte) (int, error) {
	if len(p) > r.chunk {
		p = p[:r.chunk]
	}
	return r.r.Read(p)
}

type archiveErrorReader struct{}

func (archiveErrorReader) Read([]byte) (int, error) { return 0, errors.New("injected reader error") }

type archiveIdleReader struct{}

func (archiveIdleReader) Read([]byte) (int, error) { return 0, nil }

func TestInitrdArchiveReaderErrors(t *testing.T) {
	for _, reader := range []io.Reader{nil, archiveErrorReader{}, archiveIdleReader{}, io.MultiReader(bytes.NewReader(archiveFixture()), archiveErrorReader{})} {
		inventory, err := ReadInitrdArchive(reader, InitrdArchiveOptions{})
		if err == nil || inventory != nil {
			t.Fatal("reader error did not fail closed")
		}
	}
	// Exercise the exact-byte boundary in a header/body and make sure a
	// ReadFull call cannot hide the stream's quota error.
	b := archiveFixture(archiveRegular("file", strings.Repeat("x", 100)))
	for limit := 1; limit < len(b); limit++ {
		archiveReject(t, b, InitrdArchiveOptions{Limits: InitrdArchiveLimits{MaxArchiveBytes: int64(limit)}})
	}
}

func FuzzInitrdArchive(f *testing.F) {
	f.Add(archiveFixture())
	f.Add(archiveFixture(archiveRegular("file", "data"), archiveSymlink("lib", "usr/lib")))
	f.Add([]byte("070702"))
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > 1<<20 {
			t.Skip()
		}
		got, err := ReadInitrdArchive(bytes.NewReader(data), InitrdArchiveOptions{Limits: InitrdArchiveLimits{MaxArchiveBytes: 1 << 20, MaxEntryBytes: 1 << 16, MaxEntries: 128, MaxNameBytes: 1 << 16, MaxRetainedBytes: 1 << 16, MaxHardlinkBytes: 1 << 16}})
		if err != nil && got != nil {
			t.Fatal("partial inventory escaped on error")
		}
		if err == nil && (!got.validated || got.Size() != int64(len(data))) {
			t.Fatal("incomplete successful observation")
		}
	})
}
