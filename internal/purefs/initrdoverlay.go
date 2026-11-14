package purefs

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"io/fs"
	"path"
	"strings"

	"github.com/klauspost/compress/zstd"
	"github.com/ulikunitz/xz"

	"github.com/tuna-os/tacklebox/internal/oci"
)

// BuildInitrdOverlay produces a newc cpio archive that, PREPENDED to an
// image's stock initramfs, turns it into a tbox-live initramfs — the
// same effect as the dracut rebuild PrepareInitramfs does in a
// container, computed as pure tree work instead. Segment order matters:
// the kernel unpacks raw cpio segments first and allows ONE trailing
// compressed segment — raw-after-compressed fails ("invalid magic at
// start of compressed archive"). The overlay introduces only new paths,
// so extraction order doesn't change the result. It carries:
//
//   - the tbox dracut module scripts at their RUNTIME hook paths (both
//     the /usr/lib and /var/lib hookdir generations of dracut)
//   - the systemd generator + tbox-root unit and its wants/requires links
//   - the kernel modules the live path needs (erofs, squashfs, loop,
//     overlay, isofs, sr_mod, cdrom), lifted from the image tree and
//     decompressed — tbox-live-root insmods them when modprobe has no
//     modules.dep entry for them
//
// modules is the embedded src/dracut tree (tacklebox.DracutModules).
func BuildInitrdOverlay(root *oci.Node, store oci.BlobStore, kver string, modules fs.FS) ([]byte, error) {
	w := newCpioWriter()
	if err := overlayScripts(w, modules); err != nil {
		return nil, err
	}
	// Kernel modules out of the image tree, decompressed so insmod never
	// depends on kernel-side decompression support.
	wanted := map[string]bool{
		"erofs": true, "squashfs": true, "loop": true, "overlay": true,
		"isofs": true, "sr_mod": true, "cdrom": true,
	}
	modRoot := root.Lookup("usr/lib/modules/" + kver)
	if modRoot == nil {
		return nil, fmt.Errorf("no modules tree for kernel %s", kver)
	}
	found := 0
	err := modRoot.Walk(func(p string, n *oci.Node) error {
		if n.Type != oci.TypeFile {
			return nil
		}
		base := path.Base(p)
		name := strings.TrimSuffix(strings.TrimSuffix(strings.TrimSuffix(base, ".zst"), ".gz"), ".xz")
		if !strings.HasSuffix(name, ".ko") || !wanted[strings.TrimSuffix(name, ".ko")] {
			return nil
		}
		rc, err := store.Open(n.Ref)
		if err != nil {
			return err
		}
		defer rc.Close()
		var body []byte
		switch {
		case strings.HasSuffix(base, ".zst"):
			zr, err := zstd.NewReader(rc)
			if err != nil {
				return err
			}
			body, err = io.ReadAll(zr)
			zr.Close()
			if err != nil {
				return err
			}
		case strings.HasSuffix(base, ".gz"):
			gr, err := gzip.NewReader(rc)
			if err != nil {
				return err
			}
			body, err = io.ReadAll(gr)
			gr.Close()
			if err != nil {
				return err
			}
		case strings.HasSuffix(base, ".xz"):
			// Debian compresses EVERY module with xz, so skipping these
			// (as this did while there was no decoder wired up) dropped all
			// seven wanted modules and left found==0 — a live root that
			// cannot insmod erofs or isofs. ulikunitz/xz is pure Go and was
			// already in the module graph via go-diskfs, so this costs a
			// direct require and nothing else.
			xr, err := xz.NewReader(rc)
			if err != nil {
				return err
			}
			body, err = io.ReadAll(xr)
			if err != nil {
				return err
			}
		default:
			body, err = io.ReadAll(rc)
			if err != nil {
				return err
			}
		}
		// Name the extracted module by its decompressed form — tbox-live-root
		// insmods these directly, and insmod does not decompress.
		dst := "usr/lib/modules/" + kver + "/" +
			strings.TrimSuffix(strings.TrimSuffix(strings.TrimSuffix(p, ".zst"), ".gz"), ".xz")
		w.file(dst, 0o644, body)
		found++
		return nil
	})
	if err != nil {
		return nil, err
	}
	if found == 0 {
		// Not fatal: fully-builtin kernels exist. The live boot will tell.
		fmt.Println("!!! initrd overlay: no loadable fs/device modules found (builtin kernel?)")
	}
	return w.finish(), nil
}

// BuildInitrdOverlayScriptsOnly is the tree-less overlay for DDI input
// (tacklebox#172): there is no unpacked image to harvest kernel modules
// from, and none is needed — a mkosi A/B image's initrd already carries
// the storage modules that mount its EROFS root on the installed
// system. Only the tbox live scripts/units are prepended.
func BuildInitrdOverlayScriptsOnly(modules fs.FS) ([]byte, error) {
	w := newCpioWriter()
	if err := overlayScripts(w, modules); err != nil {
		return nil, err
	}
	return w.finish(), nil
}

// overlayScripts writes the tbox dracut module scripts, the systemd
// generator + tbox-root unit and its wants/requires links — the
// tree-independent half of the overlay. modules is the embedded
// src/dracut tree (tacklebox.DracutModules).
func overlayScripts(w *cpioWriter, modules fs.FS) error {
	script := func(embedded string) ([]byte, error) {
		b, err := fs.ReadFile(modules, embedded)
		if err != nil {
			return nil, fmt.Errorf("embedded %s: %w", embedded, err)
		}
		return b, nil
	}

	// dracut hook + script installs (mirrors 90tbox-live/95tbox-root
	// module-setup.sh, at the paths the built initramfs would contain).
	type inst struct {
		src  string
		dsts []string
		mode uint32
	}
	insts := []inst{
		{"src/dracut/90tbox-live/parse-tbox-live.sh", []string{
			"usr/lib/dracut/hooks/cmdline/30-parse-tbox-live.sh",
			"var/lib/dracut/hooks/cmdline/30-parse-tbox-live.sh",
		}, 0o755},
		// usr/bin, NEVER sbin/ or usr/sbin/: the overlay is unpacked FIRST,
		// and a merged-usr stock initramfs (Fedora: dracut >= 111 mirrors
		// the host's /sbin -> usr/sbin and /usr/sbin -> bin) then tries to
		// create those names as symlinks. The kernel cannot replace a
		// non-empty directory with a symlink, so a real sbin/ made here
		// survives, holds only tbox-live-root, and hides /sbin/initqueue
		// and everything else dracut put there: the boot died in
		// emergency on "/sbin/initqueue: No such file or directory" and a
		// finished-hook that never cleared (bonito-rawhide, iso-builder run
		// 43570696690). usr/bin is a real directory in every layout.
		// parse-tbox-live.sh and the generator locate the script by name.
		{"src/dracut/90tbox-live/tbox-live-root.sh", []string{"usr/bin/tbox-live-root"}, 0o755},
		{"src/dracut/90tbox-live/tbox-live-generator.sh", []string{
			"usr/lib/systemd/system-generators/tbox-live-generator",
		}, 0o755},
		{"src/dracut/95tbox-root/tbox-root-mount.sh", []string{"usr/bin/tbox-root-mount.sh"}, 0o755},
		{"src/dracut/95tbox-root/tbox-root.service", []string{
			"usr/lib/systemd/system/tbox-root.service",
		}, 0o644},
	}
	for _, in := range insts {
		b, err := script(in.src)
		if err != nil {
			return err
		}
		for _, d := range in.dsts {
			w.file(d, in.mode, b)
		}
	}
	w.symlink("usr/lib/systemd/system/initrd-root-fs.target.wants/tbox-root.service",
		"../tbox-root.service")
	w.symlink("usr/lib/systemd/system/ostree-prepare-root.service.requires/tbox-root.service",
		"../tbox-root.service")
	return nil
}

// ── minimal newc cpio writer ────────────────────────────────────────────────

type cpioWriter struct {
	buf  bytes.Buffer
	dirs map[string]bool
	ino  int
}

func newCpioWriter() *cpioWriter {
	return &cpioWriter{dirs: map[string]bool{}}
}

func (w *cpioWriter) header(name string, mode uint32, size int) {
	w.ino++
	fmt.Fprintf(&w.buf, "070701%08X%08X%08X%08X%08X%08X%08X%08X%08X%08X%08X%08X%08X",
		w.ino, mode, 0, 0, 1, 0, size, 0, 0, 0, 0, len(name)+1, 0)
	w.buf.WriteString(name)
	w.buf.WriteByte(0)
	w.pad4()
}

func (w *cpioWriter) pad4() {
	for w.buf.Len()%4 != 0 {
		w.buf.WriteByte(0)
	}
}

func (w *cpioWriter) mkdirAll(dir string) {
	if dir == "." || dir == "/" || dir == "" || w.dirs[dir] {
		return
	}
	w.mkdirAll(path.Dir(dir))
	w.dirs[dir] = true
	w.header(dir, 0o040755, 0)
}

func (w *cpioWriter) file(name string, mode uint32, body []byte) {
	w.mkdirAll(path.Dir(name))
	w.header(name, 0o100000|mode, len(body))
	w.buf.Write(body)
	w.pad4()
}

func (w *cpioWriter) symlink(name, target string) {
	w.mkdirAll(path.Dir(name))
	w.header(name, 0o120777, len(target))
	w.buf.WriteString(target)
	w.pad4()
}

func (w *cpioWriter) finish() []byte {
	w.header("TRAILER!!!", 0, 0)
	// pad the whole segment to 512 so a following reader finds clean
	// alignment (the kernel skips zero padding between segments)
	for w.buf.Len()%512 != 0 {
		w.buf.WriteByte(0)
	}
	return w.buf.Bytes()
}
