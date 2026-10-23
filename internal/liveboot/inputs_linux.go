//go:build linux

package liveboot

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// ReadInput uses descriptor-relative, no-follow opens for every path component.
// The caller's directory must itself be a regular directory, not a symlink.
// Files are bounded before and during reading and their identity is checked
// afterward; the immutable digest is checked on the bytes actually returned.
func ReadInput(directory string, in Input) ([]byte, error) {
	if err := in.validate(); err != nil {
		return nil, err
	}
	fd, err := openDirectory(directory)
	if err != nil {
		return nil, err
	}
	defer func() { syscall.Close(fd) }()
	parts := strings.Split(in.Path, "/")
	for _, part := range parts[:len(parts)-1] {
		next, err := syscall.Openat(fd, part, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
		if err != nil {
			return nil, err
		}
		syscall.Close(fd)
		fd = next
	}
	fileFD, err := syscall.Openat(fd, parts[len(parts)-1], syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fileFD), in.Path)
	defer f.Close()
	before, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !before.Mode().IsRegular() || before.Size() != in.Size {
		return nil, fmt.Errorf("input must be a regular file of the pinned size")
	}
	data, err := io.ReadAll(io.LimitReader(f, in.Size+1))
	if err != nil {
		return nil, err
	}
	after, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !os.SameFile(before, after) || before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) || int64(len(data)) != in.Size {
		return nil, fmt.Errorf("input changed while reading")
	}
	sum := sha256.Sum256(data)
	if hex.EncodeToString(sum[:]) != in.SHA256 {
		return nil, fmt.Errorf("input SHA-256 mismatch")
	}
	return data, nil
}

// openDirectory refuses symlinks in the preparation directory path too.
func openDirectory(directory string) (int, error) {
	absolute, err := filepath.Abs(directory)
	if err != nil {
		return -1, err
	}
	fd, err := syscall.Open("/", syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_CLOEXEC, 0)
	if err != nil {
		return -1, err
	}
	for _, part := range strings.Split(strings.TrimPrefix(absolute, "/"), "/") {
		if part == "" {
			continue
		}
		next, err := syscall.Openat(fd, part, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
		syscall.Close(fd)
		if err != nil {
			return -1, err
		}
		fd = next
	}
	return fd, nil
}
