package verify

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// WalkFsRel returns a set of paths under root, relative to root with
// no leading slash. Match the FAT helper's semantics so checkBLSResolves
// can use the same lookup map type.
func WalkFsRel(root string) map[string]bool {
	files := map[string]bool{}
	filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			// Skip unreadable subdirs (some bootc deploy dirs are mode 0700);
			// we don't need them for BLS entry resolution.
			return nil
		}
		if !info.IsDir() {
			rel, _ := filepath.Rel(root, p)
			files[rel] = true
		}
		return nil
	})
	return files
}

func sudoCat(r CommandRunner, p string) (string, error) {
	out, err := r.Output("sudo", "cat", p)
	if err != nil {
		return "", err
	}
	return string(out), nil
}

func readDirSudo(r CommandRunner, dir string) ([]string, error) {
	if entries, err := os.ReadDir(dir); err == nil {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		sort.Strings(names)
		return names, nil
	}
	out, err := r.Output("sudo", "ls", "-1", dir)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, line := range strings.Split(string(out), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			names = append(names, line)
		}
	}
	sort.Strings(names)
	return names, nil
}
