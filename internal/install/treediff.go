package install

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"syscall"
)

// syscallStat aliases the platform stat so owner/rdev comparisons read
// cleanly. Linux-only, like the rest of the build pipeline.
type syscallStat = syscall.Stat_t

// TreeDiff computes a file-level diff of envDir against baseDir and
// materializes it at outDir as an overlayfs delta layer:
//
//   - entries new in env, or differing in content/metadata/type, are
//     copied into outDir
//   - entries present in base but absent from env become overlayfs
//     whiteouts (char 0:0 device nodes)
//
// Stacking outDir as a lowerdir above the base at boot
// (lowerdir=delta:base) reproduces the env's rootfs exactly. Diffing at
// file level (rather than reusing OCI layer diffs) means no opaque-dir
// xattrs are ever needed — a recreated directory decomposes into
// per-entry whiteouts and copies — so the delta squashes and mounts
// with no xattr support at all.
//
// Regular-file comparison is size, mode, uid/gid, then full byte
// compare. Byte-comparing files whose metadata matches is the honest
// check: layer-extracted mtimes are stable across images sharing a
// base, but trusting metadata alone would silently corrupt an env the
// first time two images disagreed about a same-size file.
//
// Hardlink groups are not reconstructed (each link is copied
// independently); mksquashfs's duplicate detection collapses the
// content again. xattrs are not copied — the rootless image mounts the
// live pipeline reads from don't expose security.* xattrs anyway (the
// same reason live boots run with enforcing=0).
//
// exclude lists top-level-relative paths (e.g. "var/lib/containers/
// storage") pruned from BOTH sides — mirrors the mksquashfs -e list so
// the delta never resurrects or whiteouts content the base squash
// excluded.
//
// Runs inside `podman unshare` in production (whiteout mknod is
// permitted in a user namespace); mkWhiteout is a variable so tests
// can run unprivileged.
func TreeDiff(baseDir, envDir, outDir string, exclude []string) error {
	excluded := make(map[string]bool, len(exclude))
	for _, e := range exclude {
		excluded[filepath.Clean(e)] = true
	}
	// Materialize outDir even for an empty diff — mksquashfs of the
	// delta must always have a tree to pack.
	if err := os.MkdirAll(outDir, 0755); err != nil {
		return err
	}
	d := &treeDiffer{base: baseDir, env: envDir, out: outDir, excluded: excluded}
	return d.diffDir(".")
}

// mkWhiteout creates an overlayfs whiteout at path. Swapped by tests:
// mknod of a 0:0 char device needs root or a user namespace.
var mkWhiteout = func(path string) error {
	return syscall.Mknod(path, syscall.S_IFCHR, 0)
}

type treeDiffer struct {
	base, env, out string
	excluded       map[string]bool
}

// diffDir diffs one directory (rel, relative to the tree roots) that
// exists as a directory in BOTH trees. Children are merged by name.
func (d *treeDiffer) diffDir(rel string) error {
	baseEntries, err := readDirNames(filepath.Join(d.base, rel))
	if err != nil {
		return fmt.Errorf("read base dir %s: %w", rel, err)
	}
	envEntries, err := readDirNames(filepath.Join(d.env, rel))
	if err != nil {
		return fmt.Errorf("read env dir %s: %w", rel, err)
	}

	names := map[string]bool{}
	for _, n := range baseEntries {
		names[n] = true
	}
	for _, n := range envEntries {
		names[n] = true
	}
	sorted := make([]string, 0, len(names))
	for n := range names {
		sorted = append(sorted, n)
	}
	sort.Strings(sorted)

	for _, name := range sorted {
		childRel := filepath.Join(rel, name)
		if d.excluded[childRel] {
			continue
		}
		bInfo, bErr := os.Lstat(filepath.Join(d.base, childRel))
		eInfo, eErr := os.Lstat(filepath.Join(d.env, childRel))
		switch {
		case bErr == nil && eErr != nil:
			// Deleted in env → whiteout.
			if err := d.whiteout(childRel); err != nil {
				return err
			}
		case bErr != nil && eErr == nil:
			// New in env → copy subtree.
			if err := d.copyEntry(childRel, eInfo); err != nil {
				return err
			}
		case bErr == nil && eErr == nil:
			if err := d.diffEntry(childRel, bInfo, eInfo); err != nil {
				return err
			}
		default:
			return fmt.Errorf("lstat %s: base: %v, env: %v", childRel, bErr, eErr)
		}
	}
	return nil
}

// diffEntry handles a path present in both trees.
func (d *treeDiffer) diffEntry(rel string, bInfo, eInfo os.FileInfo) error {
	bMode, eMode := bInfo.Mode(), eInfo.Mode()

	// Type change: the env entry in the upper layer shadows the base one
	// (overlayfs: a non-dir upper hides a lower dir and vice versa), so a
	// plain copy suffices — no whiteout needed.
	if bMode.Type() != eMode.Type() {
		return d.copyEntry(rel, eInfo)
	}

	switch {
	case eMode.IsDir():
		// Same dir on both sides: recurse. Copy the dir entry itself only
		// when its metadata changed (the merged view takes dir attrs from
		// the uppermost layer that has it).
		if !sameMeta(bInfo, eInfo) {
			if err := d.copyEntry(rel, eInfo); err != nil {
				return err
			}
		}
		return d.diffDir(rel)
	case eMode.Type() == os.ModeSymlink:
		bTarget, err := os.Readlink(filepath.Join(d.base, rel))
		if err != nil {
			return fmt.Errorf("readlink base %s: %w", rel, err)
		}
		eTarget, err := os.Readlink(filepath.Join(d.env, rel))
		if err != nil {
			return fmt.Errorf("readlink env %s: %w", rel, err)
		}
		if bTarget == eTarget && sameOwner(bInfo, eInfo) {
			return nil
		}
		return d.copyEntry(rel, eInfo)
	case eMode.IsRegular():
		if bInfo.Size() == eInfo.Size() && sameMeta(bInfo, eInfo) {
			same, err := sameContent(filepath.Join(d.base, rel), filepath.Join(d.env, rel))
			if err != nil {
				return err
			}
			if same {
				return nil
			}
		}
		return d.copyEntry(rel, eInfo)
	default:
		// Device/fifo/socket: copy when metadata or device number differ.
		if sameMeta(bInfo, eInfo) && rdev(bInfo) == rdev(eInfo) {
			return nil
		}
		return d.copyEntry(rel, eInfo)
	}
}

// whiteout writes an overlayfs whiteout for rel into the delta.
func (d *treeDiffer) whiteout(rel string) error {
	if err := d.ensureParents(rel); err != nil {
		return err
	}
	if err := mkWhiteout(filepath.Join(d.out, rel)); err != nil {
		return fmt.Errorf("whiteout %s: %w", rel, err)
	}
	return nil
}

// copyEntry copies the env entry at rel (recursively for directories)
// into the delta, preserving mode, ownership, and mtime.
func (d *treeDiffer) copyEntry(rel string, info os.FileInfo) error {
	if err := d.ensureParents(rel); err != nil {
		return err
	}
	src := filepath.Join(d.env, rel)
	dst := filepath.Join(d.out, rel)

	switch info.Mode().Type() {
	case os.ModeDir:
		if err := os.Mkdir(dst, 0700); err != nil && !os.IsExist(err) {
			return fmt.Errorf("mkdir %s: %w", rel, err)
		}
		entries, err := readDirNames(src)
		if err != nil {
			return fmt.Errorf("read dir %s: %w", rel, err)
		}
		for _, name := range entries {
			childRel := filepath.Join(rel, name)
			if d.excluded[childRel] {
				continue
			}
			childInfo, err := os.Lstat(filepath.Join(d.env, childRel))
			if err != nil {
				return fmt.Errorf("lstat %s: %w", childRel, err)
			}
			if err := d.copyEntry(childRel, childInfo); err != nil {
				return err
			}
		}
	case os.ModeSymlink:
		target, err := os.Readlink(src)
		if err != nil {
			return fmt.Errorf("readlink %s: %w", rel, err)
		}
		_ = os.Remove(dst) // re-copy after a type change
		if err := os.Symlink(target, dst); err != nil {
			return fmt.Errorf("symlink %s: %w", rel, err)
		}
		return lchown(dst, info) // no chmod/utimes on symlinks
	case 0: // regular file
		if err := copyFileContents(src, dst); err != nil {
			return fmt.Errorf("copy %s: %w", rel, err)
		}
	default: // device / fifo / socket
		_ = os.Remove(dst)
		mode := uint32(info.Mode().Perm())
		switch info.Mode().Type() {
		case os.ModeDevice:
			mode |= syscall.S_IFBLK
		case os.ModeDevice | os.ModeCharDevice, os.ModeCharDevice:
			mode |= syscall.S_IFCHR
		case os.ModeNamedPipe:
			mode |= syscall.S_IFIFO
		case os.ModeSocket:
			// Sockets are meaningless in an image; skip silently.
			return nil
		}
		if err := syscall.Mknod(dst, mode, int(rdev(info))); err != nil {
			return fmt.Errorf("mknod %s: %w", rel, err)
		}
	}
	return applyMeta(dst, info)
}

// ensureParents materializes rel's ancestor directories in the delta,
// cloning each level's metadata from the env tree.
func (d *treeDiffer) ensureParents(rel string) error {
	dir := filepath.Dir(rel)
	if dir == "." {
		return nil
	}
	// Walk down from the top so each level gets env's attrs.
	parts := splitPath(dir)
	cur := ""
	for _, p := range parts {
		cur = filepath.Join(cur, p)
		dst := filepath.Join(d.out, cur)
		if _, err := os.Lstat(dst); err == nil {
			continue
		}
		info, err := os.Lstat(filepath.Join(d.env, cur))
		if err != nil {
			return fmt.Errorf("lstat env parent %s: %w", cur, err)
		}
		if err := os.Mkdir(dst, 0700); err != nil && !os.IsExist(err) {
			return fmt.Errorf("mkdir parent %s: %w", cur, err)
		}
		if err := applyMeta(dst, info); err != nil {
			return err
		}
	}
	return nil
}

func splitPath(p string) []string {
	var parts []string
	for p != "." && p != "/" && p != "" {
		parts = append([]string{filepath.Base(p)}, parts...)
		p = filepath.Dir(p)
	}
	return parts
}

func readDirNames(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	names := make([]string, len(entries))
	for i, e := range entries {
		names[i] = e.Name()
	}
	return names, nil
}

func sameMeta(a, b os.FileInfo) bool {
	return a.Mode() == b.Mode() && sameOwner(a, b)
}

func sameOwner(a, b os.FileInfo) bool {
	sa, aok := a.Sys().(*syscallStat)
	sb, bok := b.Sys().(*syscallStat)
	if !aok || !bok {
		return true // non-unix stat: nothing to compare
	}
	return sa.Uid == sb.Uid && sa.Gid == sb.Gid
}

func rdev(fi os.FileInfo) uint64 {
	if st, ok := fi.Sys().(*syscallStat); ok {
		return uint64(st.Rdev)
	}
	return 0
}

// sameContent byte-compares two files.
func sameContent(a, b string) (bool, error) {
	fa, err := os.Open(a)
	if err != nil {
		return false, err
	}
	defer fa.Close()
	fb, err := os.Open(b)
	if err != nil {
		return false, err
	}
	defer fb.Close()

	const chunk = 256 * 1024
	bufA := make([]byte, chunk)
	bufB := make([]byte, chunk)
	for {
		na, errA := io.ReadFull(fa, bufA)
		nb, errB := io.ReadFull(fb, bufB)
		if na != nb || !bytes.Equal(bufA[:na], bufB[:nb]) {
			return false, nil
		}
		if errA == io.EOF || errA == io.ErrUnexpectedEOF {
			if errB == io.EOF || errB == io.ErrUnexpectedEOF {
				return true, nil
			}
			return false, nil
		}
		if errA != nil {
			return false, errA
		}
		if errB != nil {
			return false, errB
		}
	}
}

func copyFileContents(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	_ = os.Remove(dst) // re-copy after a type change
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// applyMeta stamps mode, ownership, and mtime onto a copied entry.
// chown failures outside a user namespace (tests) are tolerated only
// when the target owner already matches.
func applyMeta(path string, info os.FileInfo) error {
	if err := os.Chmod(path, info.Mode().Perm()); err != nil {
		return fmt.Errorf("chmod %s: %w", path, err)
	}
	if err := lchown(path, info); err != nil {
		return err
	}
	if err := os.Chtimes(path, info.ModTime(), info.ModTime()); err != nil {
		return fmt.Errorf("chtimes %s: %w", path, err)
	}
	return nil
}

func lchown(path string, info os.FileInfo) error {
	st, ok := info.Sys().(*syscallStat)
	if !ok {
		return nil
	}
	if err := os.Lchown(path, int(st.Uid), int(st.Gid)); err != nil {
		// Unprivileged runs (unit tests) can't chown; that's fine when
		// the file is already ours.
		if cur, lerr := os.Lstat(path); lerr == nil {
			if cst, ok := cur.Sys().(*syscallStat); ok && cst.Uid == st.Uid && cst.Gid == st.Gid {
				return nil
			}
		}
		return fmt.Errorf("chown %s: %w", path, err)
	}
	return nil
}
