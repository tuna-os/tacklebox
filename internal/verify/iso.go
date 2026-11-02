package verify

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// ISOReader defines the transport contract for inspecting an ISO9660 image.
type ISOReader interface {
	// ExtractESP extracts the ESP image (e.g. /EFI/efi.img) from the ISO into a local file.
	// Returns the path to the extracted file, an optional cleanup function, and any error.
	ExtractESP() (string, func(), error)

	// ReadBLSEntries lists and parses BLS entries from the extracted ESP.
	ReadBLSEntries(espPath string) ([]BLSEntry, error)

	// ListESPFiles returns relative file paths in the ESP.
	ListESPFiles(espPath string) (map[string]bool, error)

	// ListLiveOS lists file names under /LiveOS in the ISO.
	ListLiveOS() ([]string, error)

	// HashFilePrefix computes SHA-256 hex of the first n bytes of an ISO file path.
	HashFilePrefix(path string, n int64) (string, error)
}

// XorrisoISOReader implements ISOReader using xorriso and mtools.
type XorrisoISOReader struct {
	ISOPath string
	Runner  CommandRunner
}

// NewXorrisoISOReader creates an ISOReader backed by xorriso and mtools commands.
func NewXorrisoISOReader(isoPath string, r CommandRunner) *XorrisoISOReader {
	if r == nil {
		r = DefaultRunner
	}
	return &XorrisoISOReader{
		ISOPath: isoPath,
		Runner:  r,
	}
}

func (r *XorrisoISOReader) ExtractESP() (string, func(), error) {
	tmp, err := r.extractFromIso("/EFI/efi.img")
	if err != nil {
		return "", nil, err
	}
	cleanup := func() {
		_ = os.Remove(tmp)
	}
	return tmp, cleanup, nil
}

func (r *XorrisoISOReader) extractFromIso(isoPath string) (string, error) {
	tmp, err := os.CreateTemp("", "tbx-verify-*"+filepath.Ext(isoPath))
	if err != nil {
		return "", err
	}
	tmp.Close()
	if err := r.Runner.Run("xorriso", "-indev", r.ISOPath, "-osirrox", "on", "-extract", isoPath, tmp.Name()); err != nil {
		os.Remove(tmp.Name())
		return "", fmt.Errorf("xorriso extract %s: %w", isoPath, err)
	}
	return tmp.Name(), nil
}

func (r *XorrisoISOReader) ReadBLSEntries(fatImg string) ([]BLSEntry, error) {
	os.Setenv("MTOOLS_SKIP_CHECK", "1")
	out, err := r.Runner.Output("mdir", "-i", fatImg, "-b", "::/loader/entries")
	if err != nil {
		return nil, err
	}
	var entries []BLSEntry
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || !strings.HasSuffix(line, ".conf") {
			continue
		}
		body, err := r.Runner.Output("mtype", "-i", fatImg, line)
		if err != nil {
			continue
		}
		e := ParseBLSEntry(string(body))
		e.Name = filepath.Base(line)
		entries = append(entries, e)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name < entries[j].Name })
	return entries, nil
}

func (r *XorrisoISOReader) ListESPFiles(fatImg string) (map[string]bool, error) {
	os.Setenv("MTOOLS_SKIP_CHECK", "1")
	out, err := r.Runner.Output("mdir", "-i", fatImg, "-b", "-/", "::/")
	if err != nil {
		return nil, err
	}
	files := map[string]bool{}
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		files[strings.TrimPrefix(line, "::/")] = true
	}
	return files, nil
}

func (r *XorrisoISOReader) ListLiveOS() ([]string, error) {
	out, err := r.Runner.Output("xorriso", "-indev", r.ISOPath, "-ls", "/LiveOS")
	if err != nil {
		return nil, err
	}
	var names []string
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "'") || !strings.HasSuffix(line, "'") {
			continue
		}
		names = append(names, strings.Trim(line, "'"))
	}
	return names, nil
}

func (r *XorrisoISOReader) HashFilePrefix(isoPath string, n int64) (string, error) {
	tmp, err := r.extractFromIso(isoPath)
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp)

	f, err := os.Open(tmp)
	if err != nil {
		return "", err
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.CopyN(h, f, n); err != nil && err != io.EOF {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// VerifyISO checks an ISO artifact using the default host reader.
func VerifyISO(path string) []CheckResult {
	return VerifyISOReader(NewXorrisoISOReader(path, DefaultRunner))
}

// VerifyISOReader verifies an ISO layout through an injected ISOReader.
func VerifyISOReader(reader ISOReader) []CheckResult {
	var results []CheckResult

	tmpEsp, cleanup, err := reader.ExtractESP()
	if err != nil {
		return append(results, CheckResult{Name: "ESP image extractable from ISO", Err: err})
	}
	if cleanup != nil {
		defer cleanup()
	}
	results = append(results, CheckResult{Name: "ESP image extractable from ISO", Err: nil})

	entries, err := reader.ReadBLSEntries(tmpEsp)
	if err != nil {
		return append(results, CheckResult{Name: "read BLS entries from ESP", Err: err})
	}
	if len(entries) == 0 {
		return append(results, CheckResult{Name: "BLS entries present", Err: fmt.Errorf("no entries under /loader/entries/")})
	}
	results = append(results, CheckResult{
		Name: fmt.Sprintf("BLS entries present (%d)", len(entries)),
		Err:  nil,
	})

	fatFiles, err := reader.ListESPFiles(tmpEsp)
	if err != nil {
		return append(results, CheckResult{Name: "enumerate ESP files", Err: err})
	}
	for _, e := range entries {
		results = append(results, CheckBLSResolves(e, fatFiles, "ESP"))
	}

	results = append(results, CheckCombinedPivots(entries)...)

	sfsList, err := reader.ListLiveOS()
	if err != nil {
		return append(results, CheckResult{Name: "list /LiveOS in ISO", Err: err})
	}
	sfsSet := make(map[string]bool, len(sfsList))
	for _, name := range sfsList {
		sfsSet[name] = true
	}

	results = append(results, CheckSquashfsReferences(entries, sfsSet)...)

	var hasOfflineStore bool
	hashes := map[string][]string{}
	for _, name := range sfsList {
		if name == "store.squashfs.img" {
			hasOfflineStore = true
			continue
		}
		if !strings.HasSuffix(name, ".rootfs.sfs") && !strings.HasSuffix(name, ".delta.sfs") {
			continue
		}
		if name == "combined.rootfs.sfs" || name == "base.rootfs.sfs" {
			continue
		}
		h, err := reader.HashFilePrefix("/LiveOS/"+name, 1<<20)
		if err != nil {
			results = append(results, CheckResult{Name: "hash /LiveOS/" + name, Err: err})
			continue
		}
		hashes[h] = append(hashes[h], name)
	}

	results = append(results, CheckSquashfsDistinctness(hashes)...)

	if hasOfflineStore {
		results = append(results, CheckResult{Name: "offline store present (LiveOS/store.squashfs.img)", Err: nil})
	}

	return results
}
