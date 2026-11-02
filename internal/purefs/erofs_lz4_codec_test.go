package purefs

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"math/rand/v2"
	"testing"

	"github.com/pierrec/lz4/v4"
)

func TestErofsLZ4RawGroups(t *testing.T) {
	var reused erofsLZ4Encoder
	stream := sha256.New()
	for _, size := range []int{1, 4096, 4097, 8192, 8193, 65535, 65536} {
		for _, random := range []bool{false, true} {
			t.Run(fmt.Sprintf("bytes=%d/random=%t", size, random), func(t *testing.T) {
				body := bytes.Repeat([]byte("x"), size)
				if random {
					rng := rand.New(rand.NewPCG(1, 2))
					for i := range body {
						body[i] = byte(rng.Uint32())
					}
				}
				original := bytes.Clone(body)
				data, compressed, err := reused.encode(body)
				if err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(body, original) {
					t.Fatal("encoder changed the source body")
				}
				var fresh erofsLZ4Encoder
				want, wantCompressed, err := fresh.encode(body)
				if err != nil || compressed != wantCompressed || !bytes.Equal(data, want) {
					t.Fatal("reused compressor changed independent-block output")
				}
				stream.Write(data)
				if !compressed {
					if !bytes.Equal(data, body) {
						t.Fatal("plain fallback changed content")
					}
					if !random && size > blkSize {
						t.Fatal("compressible multi-block group remained plain")
					}
					return
				}
				if len(data)%blkSize != 0 || len(data) >= (size+blkSize-1)/blkSize*blkSize {
					t.Fatal("compressed group does not save allocated blocks")
				}
				unpadded := bytes.TrimLeft(data, "\x00")
				if len(data)-len(unpadded) >= blkSize {
					t.Fatal("first physical block contains only padding")
				}
				decoded := make([]byte, size)
				if n, err := lz4.UncompressBlock(unpadded, decoded); err != nil || n != size || !bytes.Equal(decoded, body) {
					t.Fatalf("raw LZ4 whole-body roundtrip: n=%d err=%v", n, err)
				}
			})
		}
	}
	t.Logf("native/WASM raw LZ4 group SHA256=%x", stream.Sum(nil))
	for _, size := range []int{0, erofsLZ4InputBytes + 1} {
		if _, _, err := reused.encode(make([]byte, size)); err == nil {
			t.Fatalf("accepted invalid group size %d", size)
		}
	}
}

func TestErofsLZ4FullIndexes(t *testing.T) {
	le := binary.LittleEndian
	for _, size := range []int{4097, 8192, 8193, 65535, 65536} {
		count := (size + blkSize - 1) / blkSize
		index := make([]byte, count*8)
		if err := putErofsFullGroup(index, size, 1234, blkSize, true); err != nil {
			t.Fatal(err)
		}
		if le.Uint16(index) != erofsIndexHead || le.Uint32(index[4:]) != 1234 {
			t.Fatal("invalid HEAD address/type")
		}
		full := size / blkSize
		for j := 1; j < full; j++ {
			entry := index[j*8:]
			wantBack := j
			if j == 1 {
				wantBack = erofsIndexBlockCount | 1
			}
			if le.Uint16(entry) != erofsIndexNonhead || le.Uint16(entry[4:]) != uint16(wantBack) || le.Uint16(entry[6:]) != uint16(full-j) {
				t.Fatalf("size %d NONHEAD %d has wrong count/distance", size, j)
			}
		}
		if tail := size % blkSize; tail != 0 {
			entry := index[full*8:]
			if le.Uint16(entry) != 0 || le.Uint16(entry[2:]) != uint16(tail) || le.Uint32(entry[4:]) != 0 {
				t.Fatalf("size %d lacks exact EOF boundary index", size)
			}
		}
		if err := putErofsFullGroup(index, size, 4321, size, false); err != nil {
			t.Fatal(err)
		}
		for j := range count {
			entry := index[j*8:]
			if le.Uint16(entry) != 0 || le.Uint16(entry[2:]) != 0 || le.Uint32(entry[4:]) != uint32(4321+j) {
				t.Fatal("plain index changed block address or offset")
			}
		}
	}
	if err := putErofsFullGroup(make([]byte, 8), 4097, 0, 4096, true); err == nil {
		t.Fatal("accepted truncated index buffer")
	}
}

func BenchmarkErofsLZ4Group(b *testing.B) {
	for _, random := range []bool{false, true} {
		b.Run(fmt.Sprintf("random=%t", random), func(b *testing.B) {
			var encoder erofsLZ4Encoder
			for i := range encoder.input {
				encoder.input[i] = 'x'
			}
			if random {
				rng := rand.New(rand.NewPCG(1, 2))
				for i := range encoder.input {
					encoder.input[i] = byte(rng.Uint32())
				}
			}
			b.SetBytes(erofsLZ4InputBytes)
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				if _, _, err := encoder.encode(encoder.input[:]); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func TestErofsLZ4MultiPhysicalGroup(t *testing.T) {
	rng := rand.New(rand.NewPCG(17, 19))
	seed := make([]byte, 8192)
	for i := range seed {
		seed[i] = byte(rng.Uint32())
	}
	body := bytes.Repeat(seed, 8)
	var encoder erofsLZ4Encoder
	data, compressed, err := encoder.encode(body)
	if err != nil || !compressed || len(data) <= blkSize || len(data) >= len(body) {
		t.Fatalf("multi-physical fixture: compressed=%t bytes=%d err=%v", compressed, len(data), err)
	}
	decoded := make([]byte, len(body))
	if n, err := lz4.UncompressBlock(bytes.TrimLeft(data, "\x00"), decoded); err != nil || n != len(body) || !bytes.Equal(decoded, body) {
		t.Fatalf("multi-physical whole-body decode: n=%d err=%v", n, err)
	}
	index := make([]byte, 16*8)
	if err := putErofsFullGroup(index, len(body), 42, len(data), true); err != nil {
		t.Fatal(err)
	}
	if got := binary.LittleEndian.Uint16(index[8+4:]); got != erofsIndexBlockCount|uint16(len(data)/blkSize) {
		t.Fatalf("CBLKCNT=%x physical blocks=%d", got, len(data)/blkSize)
	}
}
