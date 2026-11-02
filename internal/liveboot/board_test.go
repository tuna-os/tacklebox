package liveboot

import (
	"encoding/binary"
	"strings"
	"testing"
)

func dtb(board string) []byte {
	st := make([]byte, 0)
	put := func(v uint32) { b := make([]byte, 4); binary.BigEndian.PutUint32(b, v); st = append(st, b...) }
	put(1)
	put(0)
	put(3)
	put(uint32(len(board) + 1))
	put(0)
	st = append(st, []byte(board)...)
	st = append(st, 0)
	for len(st)%4 != 0 {
		st = append(st, 0)
	}
	put(2)
	put(9)
	strings := []byte("compatible\x00")
	h := make([]byte, 40)
	for off, v := range map[int]uint32{0: 0xd00dfeed, 4: uint32(40 + len(st) + len(strings)), 8: 40, 12: uint32(40 + len(st)), 20: 17, 24: 16, 32: uint32(len(strings)), 36: uint32(len(st))} {
		binary.BigEndian.PutUint32(h[off:], v)
	}
	return append(append(h, st...), strings...)
}
func hwids(board string) []byte {
	b := make([]byte, 56)
	binary.LittleEndian.PutUint32(b, 0x1000001c)
	binary.LittleEndian.PutUint32(b[24:], 56)
	return append(append(b, []byte(board)...), 0)
}
func TestStaticBoardMapping(t *testing.T) {
	for _, tc := range []struct {
		name, dtb, hwids string
		ok               bool
	}{
		{"matching", Board, Board, true}, {"different DTB", "qcom,other", Board, false},
		{"fallback compatible is not selected", "qcom,other\x00" + Board, Board, false}, {"different CHID table", Board, "qcom,other", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := HasBoardMapping(syntheticPE(section{".dtbauto", dtb(tc.dtb)}, section{".hwids", hwids(tc.hwids)}))
			if (err == nil) != tc.ok {
				t.Fatalf("%v", err)
			}
		})
	}
	if err := HasBoardMapping(syntheticPE(section{".linux", []byte("kernel")})); err == nil {
		t.Fatal("accepted absent board mapping")
	}
	for _, bad := range [][]byte{nil, []byte("not a dtb"), dtb(Board)[:40]} {
		if _, err := rootCompatible(bad); err == nil {
			t.Fatal("accepted malformed DTB")
		}
	}
}
func TestGRUBWords(t *testing.T) {
	for _, s := range []string{"root=tbox:CDLABEL=TUNA_X13S", "tacklebox.live.path=/LiveOS/gnome-x13s.rootfs.sfs", "console=ttyAMA0,115200"} {
		if !safeWord(s) {
			t.Fatal(s)
		}
	}
	for _, s := range []string{"", "$(reboot)", "x;reboot", "a b", "a\nb", "'quoted'", "${prefix}", "a\\b"} {
		if safeWord(s) {
			t.Fatal(s)
		}
	}
}

func TestDTBRejectsStubIncompatibleStructure(t *testing.T) {
	for name, change := range map[string]func([]byte){
		"root name":           func(b []byte) { b[44] = 'x' },
		"unaligned structure": func(b []byte) { binary.BigEndian.PutUint32(b[8:], 41) },
	} {
		t.Run(name, func(t *testing.T) {
			b := dtb(Board)
			change(b)
			if _, err := rootCompatible(b); err == nil {
				t.Fatal("accepted DTB rejected by selected stub")
			}
		})
	}
}

func TestBoardStringsAreBounded(t *testing.T) {
	b := []byte(strings.Repeat("x", 256) + "\x00")
	if _, err := cstring(b, 0); err == nil {
		t.Fatal("accepted unbounded table string")
	}
}

func TestCompatibleListIsBounded(t *testing.T) {
	for _, value := range []string{strings.Repeat("a\x00", 40), strings.Repeat("\x00", 8192)} {
		if _, err := rootCompatible(dtb(value)); err == nil {
			t.Fatal("accepted oversized or empty compatible list")
		}
	}
}
