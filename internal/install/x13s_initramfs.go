package install

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	tacklebox "github.com/tuna-os/tacklebox"
	"github.com/tuna-os/tacklebox/internal/liveboot"
	"github.com/tuna-os/tacklebox/internal/runner"
)

// X13sInitramfsModules are the dracut modules of an experimental X13s live
// initrd: the generic live root, the per-env pivot and the ADSP profile.
var X13sInitramfsModules = []string{"tbox-live", "tbox-root", "tbox-x13s-adsp"}

// x13sADSPFirmware are the remoteproc firmware names that the
// 70tbox-x13s-adsp module installs. The two lists must stay equal.
var x13sADSPFirmware = []string{"qcadsp8280.mbn", "qccdsp8280.mbn", "qcslpi8280.mbn"}

// X13sInitramfs is a freshly emitted X13s live initrd and what the build
// container observed about its sources. The observations feed
// liveboot.InitrdIdentity; the initrd itself must still pass
// liveboot.OrchestrateX13sLive before media can reference it.
type X13sInitramfs struct {
	// Path is the uncompressed single-newc initrd on the host.
	Path                string
	KernelConfigSHA256  string
	KernelPackageBuild  string
	SourceDateEpoch     int64
	ModuleSourcesSHA256 string
	BuildPolicySHA256   string
	// Sources pins the ADSP firmware and the qcom_q6v5_pas module at the
	// paths the initrd must hold them, with the image's bytes.
	Sources []liveboot.Input
}

// PrepareX13sInitramfs always rebuilds the initrd of image for exactly
// kver, with the X13s profile, uncompressed and without early microcode,
// so liveboot's single-newc reader can audit the bytes the media will
// reference. There is no cache: the audit is the only acceptance path.
func PrepareX13sInitramfs(image, kver string) (*X13sInitramfs, error) {
	if kver == "" || strings.ContainsAny(kver, "\x00\r\n /\\'\"$`") {
		return nil, fmt.Errorf("invalid X13s kernel release %q", kver)
	}
	initramfsSerialise.Lock()
	defer initramfsSerialise.Unlock()

	prefix, _, err := podmanForImage(image)
	if err != nil {
		return nil, fmt.Errorf("resolve image ID for %s: %w", image, err)
	}
	moduleDirs, err := materializeDracutModules()
	if err != nil {
		return nil, err
	}
	defer func() {
		for _, d := range moduleDirs {
			_ = os.RemoveAll(d)
		}
	}()
	outDir, err := tempBuildDir("", "tbox-x13s-initramfs-*")
	if err != nil {
		return nil, fmt.Errorf("mktemp: %w", err)
	}
	defer func() { _ = os.RemoveAll(filepath.Dir(outDir)) }()

	script := x13sInitramfsScript(kver)
	fmt.Printf(">>> [initramfs] %s: X13s profile for kernel %s (uncompressed, no early microcode)\n", image, kver)
	runArgs := append(prefix[1:],
		"run", "--rm", "--privileged",
		"--security-opt", "label=disable",
		"--log-driver", "k8s-file",
		"-v", outDir+":/tbox-out",
	)
	for name, dir := range moduleDirs {
		runArgs = append(runArgs,
			"-v", dir+":/usr/lib/dracut/modules.d/"+embeddedDracutModules[name]+":ro")
	}
	runArgs = append(runArgs, "--entrypoint", "/bin/sh", image, "-c", script)
	out, err := runner.Output(prefix[0], runArgs...)
	if err != nil {
		return nil, fmt.Errorf("X13s initramfs for %s failed: %w", image, err)
	}
	result, err := parseX13sInitramfsOutput(string(out), kver)
	if err != nil {
		return nil, fmt.Errorf("X13s initramfs for %s: %w", image, err)
	}
	policy := sha256.Sum256([]byte(script))
	result.BuildPolicySHA256 = hex.EncodeToString(policy[:])
	result.ModuleSourcesSHA256 = embeddedModulesDigest()

	destDir := filepath.Join(stagingRoot, "x13s-initramfs")
	if err := runner.Run("sudo", "mkdir", "-p", destDir); err != nil {
		return nil, err
	}
	id := sha256.Sum256([]byte(image + "|" + kver))
	result.Path = filepath.Join(destDir, hex.EncodeToString(id[:8])+".img")
	if err := runner.Run("sudo", "mv", filepath.Join(outDir, "initramfs.img"), result.Path); err != nil {
		return nil, fmt.Errorf("move X13s initramfs into staging: %w", err)
	}
	if err := runner.Run("sudo", "chmod", "0644", result.Path); err != nil {
		return nil, err
	}
	return result, nil
}

// parseX13sInitramfsOutput reads the observation lines x13sInitramfsScript
// prints. Every observation is mandatory; a missing or repeated one fails.
func parseX13sInitramfsOutput(out, kver string) (*X13sInitramfs, error) {
	r := &X13sInitramfs{}
	seen := map[string]bool{}
	for _, line := range strings.Split(out, "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok || !strings.HasPrefix(key, "TBOX_X13S_") {
			continue
		}
		if key != "TBOX_X13S_PIN" {
			if seen[key] {
				return nil, fmt.Errorf("repeated observation %s", key)
			}
			seen[key] = true
		}
		switch key {
		case "TBOX_X13S_KCONFIG":
			r.KernelConfigSHA256 = value
		case "TBOX_X13S_KPKG":
			r.KernelPackageBuild = value
		case "TBOX_X13S_EPOCH":
			epoch, err := strconv.ParseInt(value, 10, 64)
			if err != nil || epoch < 0 {
				return nil, fmt.Errorf("invalid kernel build time %q", value)
			}
			r.SourceDateEpoch = epoch
		case "TBOX_X13S_PIN":
			fields := strings.Fields(value)
			if len(fields) != 3 {
				return nil, fmt.Errorf("malformed source pin %q", value)
			}
			size, err := strconv.ParseInt(fields[0], 10, 64)
			if err != nil {
				return nil, fmt.Errorf("malformed source pin size %q", value)
			}
			r.Sources = append(r.Sources, liveboot.Input{Path: fields[2], Size: size, SHA256: fields[1]})
		}
	}
	for _, key := range []string{"TBOX_X13S_KCONFIG", "TBOX_X13S_KPKG", "TBOX_X13S_EPOCH"} {
		if !seen[key] {
			return nil, fmt.Errorf("missing observation %s", key)
		}
	}
	if len(r.Sources) != len(x13sADSPFirmware)+1 {
		return nil, fmt.Errorf("expected %d source pins (ADSP firmware and qcom_q6v5_pas), got %d", len(x13sADSPFirmware)+1, len(r.Sources))
	}
	modulePrefix := "usr/lib/modules/" + kver + "/"
	modules := 0
	for _, pin := range r.Sources {
		if strings.HasPrefix(pin.Path, modulePrefix) {
			modules++
		}
	}
	if modules != 1 {
		return nil, fmt.Errorf("qcom_q6v5_pas pin must be under %s", modulePrefix)
	}
	return r, nil
}

// x13sInitramfsScript observes the X13s sources inside the image and
// emits the initrd. It refuses an image whose kernel, module and ADSP
// firmware do not come from one kernel build, and it pins dracut to the
// form liveboot.ReadInitrdArchive accepts: one uncompressed newc archive.
func x13sInitramfsScript(kver string) string {
	firmware := strings.Join(x13sADSPFirmware, " ")
	return fmt.Sprintf(`set -eu
kver='%s'
moddir="/usr/lib/modules/$kver"
fail() { echo "X13s initramfs: $*" >&2; exit 1; }
[ "$(uname -m)" = aarch64 ] || fail "image is not aarch64"
[ -f "$moddir/vmlinuz" ] && [ -f "$moddir/modules.dep" ] && [ -f "$moddir/config" ] || fail "image has no complete kernel $kver"
for tool in dracut rpm modinfo sha256sum stat readlink; do
  command -v "$tool" >/dev/null 2>&1 || fail "image lacks $tool"
done
kpkg="$(rpm -qf --qf '%%{SOURCERPM}\n' "$moddir/vmlinuz")" || fail "no package owns $moddir/vmlinuz"
epoch="$(rpm -qf --qf '%%{BUILDTIME}\n' "$moddir/vmlinuz")"
pin() {
  real="$(readlink -f "$1")"
  [ -f "$real" ] || fail "missing source $1"
  echo "TBOX_X13S_PIN=$(stat -c %%s "$real") $(sha256sum "$real" | cut -d' ' -f1) $2"
}
mod="$(readlink -f "$(modinfo -k "$kver" -n qcom_q6v5_pas)")" || fail "kernel $kver has no qcom_q6v5_pas module"
case "$mod" in "$moddir"/*) ;; *) fail "qcom_q6v5_pas is outside $moddir: $mod" ;; esac
[ "$(rpm -qf --qf '%%{SOURCERPM}\n' "$mod")" = "$kpkg" ] || fail "qcom_q6v5_pas is not from $kpkg"
case "$(modinfo -k "$kver" -F vermagic qcom_q6v5_pas)" in "$kver "*) ;; *) fail "qcom_q6v5_pas vermagic does not name $kver" ;; esac
echo "TBOX_X13S_KCONFIG=$(sha256sum "$moddir/config" | cut -d' ' -f1)"
echo "TBOX_X13S_KPKG=$kpkg"
echo "TBOX_X13S_EPOCH=$epoch"
pin "$mod" "${mod#/}"
for name in %s; do
  found=""
  for suffix in "" .xz .zst; do
    logical="usr/lib/firmware/qcom/sc8280xp/LENOVO/21BX/$name$suffix"
    if [ -f "/$logical" ] && [ -s "/$logical" ]; then pin "/$logical" "$logical"; found=1; break; fi
  done
  [ -n "$found" ] || fail "image lacks firmware qcom/sc8280xp/LENOVO/21BX/$name"
done
%s
omit="$omit squash squash-lib squash-squashfs squash-erofs"
DRACUT_ARCH=aarch64 DRACUT_NO_MKNOD=1 SOURCE_DATE_EPOCH="$epoch" \
  dracut --force --no-hostonly --no-hostonly-cmdline --reproducible \
  --no-compress --no-early-microcode --nohardlink --nostrip --no-uefi \
  --omit "$omit" --add "%s" --kver "$kver" /tbox-out/initramfs.img
echo "TBOX_INITRAMFS=X13s profile for $kver, uncompressed, omitted:$omit"
`, kver, firmware, dracutPrelude, strings.Join(X13sInitramfsModules, " "))
}

// ImageArchitecture returns the OCI architecture ("amd64", "arm64") of a
// pulled image, from whichever podman store holds it.
func ImageArchitecture(image string) (string, error) {
	prefix, _, err := podmanForImage(image)
	if err != nil {
		return "", err
	}
	args := append(prefix[1:], "image", "inspect", "--format", "{{.Architecture}}", image)
	out, err := runner.Output(prefix[0], args...)
	if err != nil {
		return "", fmt.Errorf("inspect architecture of %s: %w", image, err)
	}
	return strings.TrimSpace(string(out)), nil
}

// MaterializeX13sLiveSafety writes the embedded X13s live safety script to
// a temporary directory and returns its path. The caller removes the
// directory after the live customization has run.
func MaterializeX13sLiveSafety() (string, error) {
	dir, err := os.MkdirTemp("", "tbox-x13s-safety-*")
	if err != nil {
		return "", err
	}
	data, err := tacklebox.X13sLiveSafety.ReadFile("src/live/x13s-safety.sh")
	if err != nil {
		_ = os.RemoveAll(dir)
		return "", fmt.Errorf("embedded X13s live safety script: %w", err)
	}
	if err := os.Chmod(dir, 0o755); err != nil {
		_ = os.RemoveAll(dir)
		return "", err
	}
	path := filepath.Join(dir, "x13s-safety.sh")
	if err := os.WriteFile(path, data, 0o755); err != nil {
		_ = os.RemoveAll(dir)
		return "", err
	}
	return path, nil
}
