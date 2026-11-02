package liveboot

import (
	"bytes"
	"encoding/binary"
	"fmt"
)

// HasBoardMapping validates the static DTB and CHID-table linkage only. Actual
// firmware SMBIOS matching and selection require a boot observation.
func HasBoardMapping(loader []byte) error {
	f, err := ARM64PE(loader)
	if err != nil {
		return err
	}
	dtbMatch, hwidMatch := false, false
	hwidCount := 0
	for _, s := range f.Sections {
		if s.Name != ".dtbauto" && s.Name != ".hwids" {
			continue
		}
		if s.VirtualSize == 0 || s.VirtualSize > s.Size {
			return fmt.Errorf("incomplete board section")
		}
		data := loader[int(s.Offset):int(s.Offset+s.VirtualSize)]
		if s.Name == ".dtbauto" {
			compatibles, err := rootCompatible(data)
			if err != nil {
				return err
			}
			// systemd devicetree_match_by_compatible compares the first
			// root-compatible string, not any later fallback compatible.
			if len(compatibles) > 0 && compatibles[0] == Board {
				dtbMatch = true
			}
		} else {
			hwidCount++
			if hwidCount != 1 {
				return fmt.Errorf("duplicate hardware table")
			}
			terminated := false
			for off := 0; off+4 <= len(data); off += 28 {
				if off/28 >= 4096 {
					return fmt.Errorf("hardware table exceeds record limit")
				}
				descriptor := binary.LittleEndian.Uint32(data[off:])
				if descriptor == 0 {
					terminated = true
					break
				}
				if off+28 > len(data) || (descriptor != 0x1000001c && descriptor != 0x2000001c) {
					return fmt.Errorf("invalid hardware table record")
				}
				compatibleOffset := binary.LittleEndian.Uint32(data[off+24:])
				if compatibleOffset == 0 {
					continue
				}
				value, err := cstring(data, int(compatibleOffset))
				if err != nil {
					return err
				}
				if descriptor == 0x1000001c && value == Board {
					hwidMatch = true
				}
			}
			if !terminated {
				return fmt.Errorf("unterminated hardware table")
			}
		}
	}
	if !dtbMatch || !hwidMatch {
		return fmt.Errorf("missing static X13s DTB/hardware-ID mapping")
	}
	return nil
}

func cstring(data []byte, offset int) (string, error) {
	if offset < 0 || offset >= len(data) {
		return "", fmt.Errorf("string outside section")
	}
	end := offset + 256
	if end > len(data) {
		end = len(data)
	}
	n := bytes.IndexByte(data[offset:end], 0)
	if n < 0 {
		return "", fmt.Errorf("unterminated string")
	}
	return string(data[offset : offset+n]), nil
}
func rootCompatible(data []byte) ([]string, error) {
	if len(data) < 40 || binary.BigEndian.Uint32(data) != 0xd00dfeed {
		return nil, fmt.Errorf("invalid DTB header")
	}
	word := func(off int) uint64 { return uint64(binary.BigEndian.Uint32(data[off:])) }
	total, structure, stringsOffset, stringsSize, structureSize := word(4), word(8), word(12), word(32), word(36)
	if total > uint64(len(data)) || total < 40 || structure < 40 || structure%4 != 0 || structureSize%4 != 0 || structure+structureSize > stringsOffset || stringsOffset < 40 || structure+structureSize > total || stringsOffset+stringsSize > total {
		return nil, fmt.Errorf("DTB spans outside payload")
	}
	st, stringsData := data[structure:structure+structureSize], data[stringsOffset:stringsOffset+stringsSize]
	depth, roots := 0, 0
	var compatible []string
	tokens := 0
	for pos := 0; pos+4 <= len(st); {
		tokens++
		if tokens > 65536 {
			return nil, fmt.Errorf("DTB exceeds token limit")
		}
		token := binary.BigEndian.Uint32(st[pos:])
		pos += 4
		switch token {
		case 1:
			name, err := cstring(st, pos)
			if err != nil {
				return nil, err
			}
			pos = (pos + len(name) + 1 + 3) &^ 3
			if depth == 0 {
				if name != "" {
					return nil, fmt.Errorf("nonempty DTB root name")
				}
				roots++
			}
			depth++
		case 2:
			depth--
			if depth < 0 {
				return nil, fmt.Errorf("unbalanced DTB")
			}
		case 3:
			if pos+8 > len(st) || depth == 0 {
				return nil, fmt.Errorf("invalid DTB property")
			}
			size, nameOffset := uint64(binary.BigEndian.Uint32(st[pos:])), int(binary.BigEndian.Uint32(st[pos+4:]))
			pos += 8
			if uint64(pos)+size > uint64(len(st)) {
				return nil, fmt.Errorf("DTB property outside structure")
			}
			name, err := cstring(stringsData, nameOffset)
			if err != nil {
				return nil, err
			}
			value := st[pos : uint64(pos)+size]
			pos = (pos + int(size) + 3) &^ 3
			if depth == 1 && name == "compatible" {
				if compatible != nil || len(value) == 0 || value[len(value)-1] != 0 {
					return nil, fmt.Errorf("invalid DTB compatible")
				}
				if len(value) > 4096 {
					return nil, fmt.Errorf("DTB compatible list too large")
				}
				for offset := 0; offset < len(value); {
					if len(compatible) >= 32 {
						return nil, fmt.Errorf("too many DTB compatibles")
					}
					v, err := cstring(value, offset)
					if err != nil {
						return nil, err
					}
					if v == "" {
						return nil, fmt.Errorf("empty DTB compatible")
					}
					compatible = append(compatible, v)
					offset += len(v) + 1
				}
			}
		case 4:
		case 9:
			if depth != 0 || roots != 1 {
				return nil, fmt.Errorf("unbalanced DTB end")
			}
			return compatible, nil
		default:
			return nil, fmt.Errorf("invalid DTB token")
		}
	}
	return nil, fmt.Errorf("missing DTB end")
}
