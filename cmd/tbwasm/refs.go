package main

import (
	"fmt"
	"strconv"
	"strings"
)

// splitRef splits an <repo>:<tag> image reference on the last colon that
// precedes any slash, so a registry:port prefix in repo (e.g.
// localhost:5000/foo:latest) is not mistaken for the tag separator.
func splitRef(s string) (repo, tag string, ok bool) {
	i := -1
	for j := len(s) - 1; j >= 0; j-- {
		if s[j] == ':' {
			i = j
			break
		}
		if s[j] == '/' {
			break
		}
	}
	if i <= 0 || i == len(s)-1 {
		return "", "", false
	}
	return s[:i], s[i+1:], true
}

// formatArenaRef encodes an opfsArena blob location as "<prefix>:<off>:<len>".
func formatArenaRef(prefix string, off, length int64) string {
	return fmt.Sprintf("%s:%d:%d", prefix, off, length)
}

// parseArenaRef decodes a ref produced by formatArenaRef.
func parseArenaRef(ref string) (prefix string, off, length int64, err error) {
	parts := strings.Split(ref, ":")
	if len(parts) != 3 {
		return "", 0, 0, fmt.Errorf("bad arena ref %q", ref)
	}
	off, err = strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		return "", 0, 0, fmt.Errorf("bad arena ref %q: offset: %w", ref, err)
	}
	length, err = strconv.ParseInt(parts[2], 10, 64)
	if err != nil {
		return "", 0, 0, fmt.Errorf("bad arena ref %q: length: %w", ref, err)
	}
	return parts[0], off, length, nil
}
