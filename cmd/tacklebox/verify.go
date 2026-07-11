package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/spf13/cobra"
	"github.com/tuna-os/tacklebox/internal/runner"
)

var verifyCmd = &cobra.Command{
	Use:   "verify PATH",
	Short: "Sanity-check a tacklebox-built image or ISO",
	Long: `Verify a built artifact is well-formed.

Auto-detects the artifact type:
  - .iso suffix → ISO9660 verification (no sudo required; uses xorriso)
  - everything else → raw block image (uses sudo + losetup + mount)

Checks performed (both types):
  1. Bootloader entries directory exists and is non-empty.
  2. Each BLS entry's linux= and initrd= paths resolve to real files.
  3. Per-env content is distinct — catches the bootc cross-env collision
     observed on 2026-05-11 where two stateroots ended up with the same
     ostree commit hash. For ISO targets, hashes the per-env squashfs.
     For block targets, scans ostree/composefs deployment hashes.

Exits 0 on success, 1 on any check failure (with a list of failures).
`,
	Args:         cobra.ExactArgs(1),
	SilenceUsage: true,
	RunE:         runVerify,
}

func init() {
	rootCmd.AddCommand(verifyCmd)
}

type checkResult struct {
	name string
	err  error // nil = pass
}

func (r checkResult) String() string {
	if r.err == nil {
		return "  ✓ " + r.name
	}
	return "  ✗ " + r.name + ": " + r.err.Error()
}

func runVerify(cmd *cobra.Command, args []string) error {
	path := args[0]
	if _, err := os.Stat(path); err != nil {
		return fmt.Errorf("stat %s: %w", path, err)
	}

	var results []checkResult
	if strings.HasSuffix(strings.ToLower(path), ".iso") {
		fmt.Printf(">>> Verifying ISO: %s\n", path)
		results = verifyIso(path)
	} else {
		fmt.Printf(">>> Verifying block image: %s\n", path)
		results = verifyBlock(path)
	}

	var failed int
	for _, r := range results {
		fmt.Println(r)
		if r.err != nil {
			failed++
		}
	}
	if failed > 0 {
		return fmt.Errorf("%d/%d checks failed", failed, len(results))
	}
	fmt.Printf(">>> All %d checks passed\n", len(results))
	return nil
}

// verifyIso checks an ISO without requiring sudo. Uses xorriso to extract
// the BLS entries (from /EFI/efi.img via mtools, since BLS lives inside
// the FAT) and to list /LiveOS for per-env squashfs hashing.
func verifyIso(path string) []checkResult {
	var results []checkResult

	// Extract /EFI/efi.img from the ISO into a scratch tmp file so we can
	// poke at it with mtools (which needs an actual file).
	tmpEsp, err := extractFromIso(path, "/EFI/efi.img")
	if err != nil {
		return append(results, checkResult{"ESP image extractable from ISO", err})
	}
	defer os.Remove(tmpEsp)
	results = append(results, checkResult{"ESP image extractable from ISO", nil})

	// Read BLS entries from the FAT ESP via mtools.
	entries, err := readBLSEntriesFromFat(tmpEsp)
	if err != nil {
		return append(results, checkResult{"read BLS entries from ESP", err})
	}
	if len(entries) == 0 {
		return append(results, checkResult{"BLS entries present", fmt.Errorf("no entries under /loader/entries/")})
	}
	results = append(results, checkResult{
		fmt.Sprintf("BLS entries present (%d)", len(entries)), nil,
	})

	// Each entry's linux/initrd must resolve inside the FAT.
	fatFiles, err := listFatFiles(tmpEsp)
	if err != nil {
		return append(results, checkResult{"enumerate ESP files", err})
	}
	for _, e := range entries {
		results = append(results, checkBLSResolves(e, fatFiles, "ESP"))
	}

	// Combined-squashfs layout (shared_store.dedup): every entry points
	// at the same squashimg and per-env distinctness lives in the
	// tacklebox.root= pivot instead of separate squashfs files.
	results = append(results, checkCombinedPivots(entries)...)

	// Per-env squashfs distinctness. We hash the first 1 MiB of each
	// /LiveOS/<env>.rootfs.sfs (full hash would mean reading 5+ GB through
	// xorriso for a multi-env ISO; the first MiB diverges the moment
	// content does, which is what we care about for collision detection).
	// Also note if store.squashfs.img is present (offline payloads built).
	sfsList, err := listIsoDir(path, "/LiveOS")
	if err != nil {
		return append(results, checkResult{"list /LiveOS in ISO", err})
	}
	sfsSet := make(map[string]bool, len(sfsList))
	for _, name := range sfsList {
		sfsSet[name] = true
	}

	// Every BLS entry must reference a squashfs image that exists.
	// A missing file means the ISO will fail at boot even if verify passes.
	for _, e := range entries {
		if img := blsSquashimg(e.options); img != "" {
			if !sfsSet[img] {
				results = append(results, checkResult{
					"squashfs referenced by BLS entry",
					fmt.Errorf("%s references %s but file not found in /LiveOS", e.name, img),
				})
			}
		}
		if delta := blsOption(e.options, "tacklebox.live.delta"); delta != "" {
			if !sfsSet[delta] {
				results = append(results, checkResult{
					"delta squashfs referenced by BLS entry",
					fmt.Errorf("%s references %s but file not found in /LiveOS", e.name, delta),
				})
			}
		}
	}
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
		// combined.rootfs.sfs (combined layout) and base.rootfs.sfs (delta
		// layout) are shared stores used by all environments; they are not
		// per-env files so a hash collision check is meaningless. Skip them
		// to avoid extracting the full (multi-GB) file to /tmp during
		// verification. Per-env <env>.delta.sfs files ARE hashed: two envs
		// with identical deltas booted identical content.
		if name == "combined.rootfs.sfs" || name == "base.rootfs.sfs" {
			continue
		}
		h, err := hashIsoFilePrefix(path, "/LiveOS/"+name, 1<<20)
		if err != nil {
			results = append(results, checkResult{"hash /LiveOS/" + name, err})
			continue
		}
		hashes[h] = append(hashes[h], name)
	}
	var sfsCollisions int
	for h, names := range hashes {
		if len(names) > 1 {
			sfsCollisions++
			results = append(results, checkResult{
				"per-env squashfs distinct",
				fmt.Errorf("%d envs share content hash %s: %v", len(names), h[:12], names),
			})
		}
	}
	if sfsCollisions == 0 && len(hashes) > 0 {
		results = append(results, checkResult{
			fmt.Sprintf("per-env squashfs distinct (%d unique)", len(hashes)), nil,
		})
	}
	if hasOfflineStore {
		results = append(results, checkResult{"offline store present (LiveOS/store.squashfs.img)", nil})
	}

	return results
}

// verifyBlock checks a raw disk image (loop image or /dev/* dump). Uses
// sudo + losetup + mount because GPT/ext4 don't have widely-installed
// non-root readers.
func verifyBlock(path string) []checkResult {
	var results []checkResult

	out, err := runner.Output("sudo", "losetup", "--find", "--show", "--partscan", "--read-only", path)
	if err != nil {
		return append(results, checkResult{"losetup --partscan --read-only", err})
	}
	loop := strings.TrimSpace(string(out))
	defer runner.Run("sudo", "losetup", "-d", loop)
	results = append(results, checkResult{"losetup --partscan --read-only", nil})

	espMnt, err := os.MkdirTemp("", "tbx-verify-esp-")
	if err != nil {
		return append(results, checkResult{"mkdtemp ESP", err})
	}
	defer os.RemoveAll(espMnt)
	storeMnt, err := os.MkdirTemp("", "tbx-verify-store-")
	if err != nil {
		return append(results, checkResult{"mkdtemp STORE", err})
	}
	defer os.RemoveAll(storeMnt)

	// Mount ESP (p1) and STORE (p2) read-only. STORE may need noload
	// because tacklebox umounts cleanly at Finalize but the loop snapshot
	// the user gives us might still have an unjournaled state.
	if err := runner.Run("sudo", "mount", "-o", "ro", loop+"p1", espMnt); err != nil {
		return append(results, checkResult{"mount ESP read-only", err})
	}
	defer runner.Run("sudo", "umount", espMnt)
	results = append(results, checkResult{"mount ESP read-only", nil})

	if err := runner.Run("sudo", "mount", "-o", "ro,noload", loop+"p2", storeMnt); err != nil {
		results = append(results, checkResult{"mount STORE read-only", err})
		// Continue — we can still verify the BLS entries.
	} else {
		defer runner.Run("sudo", "umount", storeMnt)
		results = append(results, checkResult{"mount STORE read-only", nil})
	}

	entriesDir := filepath.Join(espMnt, "loader", "entries")
	entryFiles, err := readDirSudo(entriesDir)
	if err != nil {
		return append(results, checkResult{"BLS entries dir readable", err})
	}
	if len(entryFiles) == 0 {
		return append(results, checkResult{"BLS entries present", fmt.Errorf("no entries under %s", entriesDir)})
	}
	results = append(results, checkResult{
		fmt.Sprintf("BLS entries present (%d)", len(entryFiles)), nil,
	})

	for _, fname := range entryFiles {
		body, err := sudoCat(filepath.Join(entriesDir, fname))
		if err != nil {
			results = append(results, checkResult{"read " + fname, err})
			continue
		}
		entry := parseBLSEntry(body)
		entry.name = fname
		// Resolve linux/initrd against the ESP root.
		fatFiles := walkFsRel(espMnt)
		results = append(results, checkBLSResolves(entry, fatFiles, "ESP"))
	}

	// Per-env distinctness. For block targets the on-disk deployment
	// hash dir is <store>/tbox-install/<env>/ostree/deploy/<env>/deploy/<hash>.<index>.
	// (ostree/boot.1/ is a runtime-only symlink farm — empty on a
	// freshly-built image.) The hash is the bootc-install content
	// fingerprint; two envs sharing one is the cross-env collision bug.
	tbxDir := filepath.Join(storeMnt, "tbox-install")
	envs, err := readDirSudo(tbxDir)
	if err == nil && len(envs) > 1 {
		envHashes := map[string][]string{}
		for _, env := range envs {
			deployRoot := filepath.Join(tbxDir, env, "ostree", "deploy", env, "deploy")
			ds, err := readDirSudo(deployRoot)
			if err != nil || len(ds) == 0 {
				continue
			}
			// Strip the .<index> suffix so two deployments of the same
			// commit in one stateroot count as one hash.
			h := ds[0]
			if dot := strings.LastIndex(h, "."); dot >= 0 {
				h = h[:dot]
			}
			envHashes[h] = append(envHashes[h], env)
		}
		var collisions int
		for h, names := range envHashes {
			if len(names) > 1 {
				collisions++
				// Same-image envs legitimately share commits (e.g. add
				// installs centos-add from the same image as centos).
				// Report as info, not a failure.
				results = append(results, checkResult{
					fmt.Sprintf("per-env ostree commit distinct (note: %d envs share commit %s: %v — expected for same-image adds)", len(names), h[:12], names),
					nil,
				})
			}
		}
		if collisions == 0 && len(envHashes) > 0 {
			results = append(results, checkResult{
				fmt.Sprintf("per-env ostree commit distinct (%d unique across %d envs)", len(envHashes), len(envs)), nil,
			})
		}
	}

	// Offline store squashfs present?
	if _, err := readDirSudo(storeMnt); err == nil {
		offlinePath := filepath.Join(storeMnt, "tbox-containers.squashfs")
		if _, err := os.Stat(offlinePath); err == nil {
			results = append(results, checkResult{"offline store present (tbox-containers.squashfs)", nil})
		}
	}

	return results
}

// blsEntry is a minimal parse — linux + initrd for resolution checks,
// options for the combined-squashfs pivot checks.
type blsEntry struct {
	name    string
	linux   string
	initrd  string
	options string
}

func parseBLSEntry(body string) blsEntry {
	var e blsEntry
	for _, line := range strings.Split(body, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		switch fields[0] {
		case "linux":
			e.linux = fields[1]
		case "initrd":
			e.initrd = fields[1]
		case "options":
			e.options = strings.Join(fields[1:], " ")
		}
	}
	return e
}

// blsOption pulls a single key=value off an options line ("" if absent).
func blsOption(options, key string) string {
	for _, f := range strings.Fields(options) {
		if strings.HasPrefix(f, key+"=") {
			return strings.TrimPrefix(f, key+"=")
		}
	}
	return ""
}

// blsSquashimg extracts the squashfs image referenced by a live BLS
// entry. tacklebox.live.squashimg is what tacklebox writes today
// (tbox-live module); rd.live.squashimg is the dmsquash-live spelling
// kept so `tacklebox verify` still validates ISOs built before the
// tbox-live switch (tuna-os/tacklebox#90).
func blsSquashimg(options string) string {
	if v := blsOption(options, "tacklebox.live.squashimg"); v != "" {
		return v
	}
	return blsOption(options, "rd.live.squashimg")
}

// checkCombinedPivots validates the shared-squashfs layouts: when
// multiple BLS entries point at ONE squashimg, each must be
// distinguishable at boot — via tacklebox.root= (combined layout: the
// tbox-root subtree pivot) or tacklebox.live.delta= (delta layout: the
// stacked delta squashfs). Two entries sharing a distinguisher would
// boot identical content — the shared-store analogue of the
// squashfs-hash collision. In the delta layout exactly one entry (the
// base env) legitimately has neither: it boots the base squashfs as-is.
// Returns nil for per-env-squashimg ISOs.
func checkCombinedPivots(entries []blsEntry) []checkResult {
	squashimgs := map[string]bool{}
	for _, e := range entries {
		if v := blsSquashimg(e.options); v != "" {
			squashimgs[v] = true
		}
	}
	if len(squashimgs) != 1 || len(entries) < 2 {
		return nil
	}

	var results []checkResult
	distinguishers := map[string][]string{}
	var bare []string
	deltaLayout := false
	for _, e := range entries {
		if blsOption(e.options, "tacklebox.live.delta") != "" {
			deltaLayout = true
		}
	}
	for _, e := range entries {
		d := blsOption(e.options, "tacklebox.root")
		if d == "" {
			d = blsOption(e.options, "tacklebox.live.delta")
		}
		if d == "" {
			bare = append(bare, e.name)
			continue
		}
		distinguishers[d] = append(distinguishers[d], e.name)
	}
	// Combined layout: every entry needs the pivot. Delta layout: exactly
	// one bare entry (the base env, booting the base squashfs as-is) is
	// legitimate.
	maxBare := 0
	if deltaLayout {
		maxBare = 1
	}
	if len(bare) > maxBare {
		results = append(results, checkResult{
			"shared squashfs entries distinguishable",
			fmt.Errorf("entries carry neither tacklebox.root= nor tacklebox.live.delta=: %v", bare),
		})
	}
	collisions := 0
	for d, names := range distinguishers {
		if len(names) > 1 {
			collisions++
			results = append(results, checkResult{
				"shared squashfs per-env distinguisher distinct",
				fmt.Errorf("%d entries share %s: %v", len(names), d, names),
			})
		}
	}
	if collisions == 0 && len(bare) <= maxBare {
		results = append(results, checkResult{
			fmt.Sprintf("combined squashfs: %d entries pivot to %d distinct env roots", len(entries), len(distinguishers)+len(bare)), nil,
		})
	}
	return results
}

func checkBLSResolves(e blsEntry, files map[string]bool, label string) checkResult {
	name := "entry " + e.name + " linux/initrd resolve in " + label
	if e.linux == "" || e.initrd == "" {
		return checkResult{name, fmt.Errorf("missing linux= or initrd= line")}
	}
	if !files[strings.TrimPrefix(e.linux, "/")] {
		return checkResult{name, fmt.Errorf("linux %s not present", e.linux)}
	}
	if !files[strings.TrimPrefix(e.initrd, "/")] {
		return checkResult{name, fmt.Errorf("initrd %s not present", e.initrd)}
	}
	return checkResult{name, nil}
}

// extractFromIso pipes a single file out of an ISO via xorriso into a
// tmp file. Returns the tmp path; caller must Remove.
func extractFromIso(iso, isoPath string) (string, error) {
	tmp, err := os.CreateTemp("", "tbx-verify-*"+filepath.Ext(isoPath))
	if err != nil {
		return "", err
	}
	tmp.Close()
	// xorriso -extract maps an ISO path to a host path.
	if err := runner.Run("xorriso", "-indev", iso, "-osirrox", "on", "-extract", isoPath, tmp.Name()); err != nil {
		os.Remove(tmp.Name())
		return "", fmt.Errorf("xorriso extract %s: %w", isoPath, err)
	}
	return tmp.Name(), nil
}

// readBLSEntriesFromFat lists /loader/entries/ inside a FAT image and
// parses each entry. Uses mtools so no mount/sudo is needed.
func readBLSEntriesFromFat(fatImg string) ([]blsEntry, error) {
	os.Setenv("MTOOLS_SKIP_CHECK", "1")
	out, err := runner.Output("mdir", "-i", fatImg, "-b", "::/loader/entries")
	if err != nil {
		return nil, err
	}
	var entries []blsEntry
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || !strings.HasSuffix(line, ".conf") {
			continue
		}
		// `mdir -b` returns paths starting with ::/ — pass to mtype.
		body, err := runner.Output("mtype", "-i", fatImg, line)
		if err != nil {
			continue
		}
		e := parseBLSEntry(string(body))
		e.name = filepath.Base(line)
		entries = append(entries, e)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].name < entries[j].name })
	return entries, nil
}

// listFatFiles returns a set of FAT-image-relative file paths (no leading
// slash) so checkBLSResolves can do O(1) lookups.
func listFatFiles(fatImg string) (map[string]bool, error) {
	os.Setenv("MTOOLS_SKIP_CHECK", "1")
	out, err := runner.Output("mdir", "-i", fatImg, "-b", "-/", "::/")
	if err != nil {
		return nil, err
	}
	files := map[string]bool{}
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		// mdir -b yields ::/path/file; strip the ::/ prefix.
		files[strings.TrimPrefix(line, "::/")] = true
	}
	return files, nil
}

// walkFsRel returns a set of paths under root, relative to root with
// no leading slash. Match the FAT helper's semantics so checkBLSResolves
// can use the same lookup map type.
func walkFsRel(root string) map[string]bool {
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

// listIsoDir uses xorriso to enumerate one directory inside an ISO.
func listIsoDir(iso, dir string) ([]string, error) {
	out, err := runner.Output("xorriso", "-indev", iso, "-ls", dir)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		// `xorriso -ls` quotes names: 'foo.sfs'
		if !strings.HasPrefix(line, "'") || !strings.HasSuffix(line, "'") {
			continue
		}
		names = append(names, strings.Trim(line, "'"))
	}
	return names, nil
}

// hashIsoFilePrefix reads the first n bytes of an ISO-internal file and
// returns the SHA-256 hex. Uses xorriso -extract to a tmp file (xorriso
// can't pipe arbitrary files to stdout cleanly) — we accept the I/O cost
// because verify is a one-shot check, not a hot path.
func hashIsoFilePrefix(iso, isoPath string, n int64) (string, error) {
	tmp, err := extractFromIso(iso, isoPath)
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

// sudoCat reads a file via sudo so we don't need to chmod root-owned
// mount trees during verify.
func sudoCat(p string) (string, error) {
	out, err := runner.Output("sudo", "cat", p)
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// readDirSudo lists a directory via sudo (the mount points are root-owned
// when we mount them). Falls back to os.ReadDir for the unprivileged
// path so unit tests can drive parts of this without sudo.
func readDirSudo(dir string) ([]string, error) {
	if entries, err := os.ReadDir(dir); err == nil {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		sort.Strings(names)
		return names, nil
	}
	out, err := runner.Output("sudo", "ls", "-1", dir)
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
