package tacklebox

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestX13sDracutArchitecture(t *testing.T) {
	for _, tc := range []struct {
		name, arch, uname string
		status            int
	}{{"native fallback", "", "aarch64", 255}, {"explicit target", "aarch64", "x86_64", 255}, {"wrong target", "x86_64", "aarch64", 1}} {
		t.Run(tc.name, func(t *testing.T) {
			script := `source src/dracut/70tbox-x13s-adsp/module-setup.sh; uname(){ echo "$TEST_UNAME"; }; check`
			cmd := exec.Command("bash", "-c", script)
			cmd.Env = append(os.Environ(), "DRACUT_ARCH="+tc.arch, "TEST_UNAME="+tc.uname)
			err := cmd.Run()
			if err == nil {
				t.Fatal("module unexpectedly auto-included")
			}
			exit, ok := err.(*exec.ExitError)
			if !ok || exit.ExitCode() != tc.status {
				t.Fatalf("%v", err)
			}
		})
	}
}
func TestX13sMandatoryModuleInstall(t *testing.T) {
	for _, tc := range []struct {
		name         string
		noKernel     string
		moduleStatus int
		wantOK       bool
	}{{"required success", "no", 0, true}, {"required failure", "no", 7, false}, {"no kernel rejected", "yes", 0, false}} {
		t.Run(tc.name, func(t *testing.T) {
			init := t.TempDir()
			if err := os.MkdirAll(filepath.Join(init, "etc/tacklebox"), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(init, "etc/tacklebox/x13s-adsp-required"), []byte("lenovo,thinkpad-x13s\n"), 0600); err != nil {
				t.Fatal(err)
			}
			script := `source src/dracut/70tbox-x13s-adsp/module-setup.sh
instmods(){ [[ "$*" == '-c qcom_q6v5_pas' && -z "$hostonly" ]] || return 99; return "$TEST_MODULE_STATUS"; }
installkernel`
			cmd := exec.Command("bash", "-c", script)
			cmd.Env = append(os.Environ(), "initdir="+init, "no_kernel="+tc.noKernel, "TEST_MODULE_STATUS="+strconv.Itoa(tc.moduleStatus))
			err := cmd.Run()
			if (err == nil) != tc.wantOK {
				t.Fatalf("%v", err)
			}
		})
	}
}
func TestX13sMandatoryFirmwareInstall(t *testing.T) {
	for _, tc := range []struct {
		name, suffix             string
		missing, empty, copyFail bool
		noCompression            bool
		wantOK                   bool
	}{
		{name: "plain", wantOK: true}, {name: "xz", suffix: ".xz", wantOK: true}, {name: "zstd", suffix: ".zst", wantOK: true}, {name: "missing", missing: true}, {name: "empty", empty: true}, {name: "sidecar", suffix: ".sig"}, {name: "copy failure", copyFail: true},
		{name: "xz unsupported", suffix: ".xz", noCompression: true}, {name: "zstd unsupported", suffix: ".zst", noCompression: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, init := t.TempDir(), t.TempDir()
			mods := t.TempDir()
			config := []byte("CONFIG_FW_LOADER_COMPRESS_XZ=y\nCONFIG_FW_LOADER_COMPRESS_ZSTD=y\n")
			if tc.noCompression {
				config = []byte("# unsupported\n")
			}
			if err := os.WriteFile(filepath.Join(mods, "config"), config, 0600); err != nil {
				t.Fatal(err)
			}
			dir := filepath.Join(root, "usr/lib/firmware/qcom/sc8280xp/LENOVO/21BX")
			if err := os.MkdirAll(dir, 0700); err != nil {
				t.Fatal(err)
			}
			for i, name := range []string{"qcadsp8280.mbn", "qccdsp8280.mbn", "qcslpi8280.mbn"} {
				if tc.missing && i == 0 {
					continue
				}
				data := []byte("fixture firmware")
				if tc.empty && i == 0 {
					data = nil
				}
				if err := os.WriteFile(filepath.Join(dir, name+tc.suffix), data, 0600); err != nil {
					t.Fatal(err)
				}
			}
			script := `source src/dracut/70tbox-x13s-adsp/module-setup.sh
inst_simple(){ [[ "$COPY_FAIL" != 1 ]] || return 7; mkdir -p "$initdir/$(dirname "$2")"; cp "$1" "$initdir/$2"; }
inst_multiple(){ [[ "$*" == 'modprobe grep tr readlink' ]]; }
inst_dir(){ mkdir -p "$initdir/$1"; }
derror(){ echo "$*" >&2; }
dfatal(){ echo "$*" >&2; }
instmods(){ [[ "$*" == '-c qcom_q6v5_pas' ]]; }
module_install(){ install; }
module_installkernel(){ installkernel; }
source testdata/x13s/dracut108-install-control-flow.sh`
			fail := "0"
			if tc.copyFail {
				fail = "1"
			}
			cmd := exec.Command("bash", "-c", script)
			cmd.Env = append(os.Environ(), "dracutsysrootdir="+root, "initdir="+init, "COPY_FAIL="+fail, "srcmods="+mods)
			out, err := cmd.CombinedOutput()
			if (err == nil) != tc.wantOK {
				t.Fatalf("%v: %s", err, out)
			}
			marker, readErr := os.ReadFile(filepath.Join(init, "etc/tacklebox/x13s-adsp-required"))
			if tc.wantOK {
				if readErr != nil || strings.TrimSpace(string(marker)) != "lenovo,thinkpad-x13s" {
					t.Fatalf("missing requirement: %v", readErr)
				}
			} else if readErr == nil {
				t.Fatal("requirement marker created after failed preparation")
			}
		})
	}
}

func TestX13sInstallFailurePropagatesThroughKernelHook(t *testing.T) {
	init := t.TempDir()
	root := t.TempDir()
	script := `source src/dracut/70tbox-x13s-adsp/module-setup.sh
instmods(){ return 0; }
derror(){ :; }
dfatal(){ :; }
module_install(){ install; }
module_installkernel(){ installkernel; }
source testdata/x13s/dracut108-install-control-flow.sh`
	cmd := exec.Command("bash", "-c", script)
	cmd.Env = append(os.Environ(), "initdir="+init, "dracutsysrootdir="+root)
	if out, err := cmd.CombinedOutput(); err == nil {
		t.Fatalf("failed firmware preparation did not fail kernel hook: %s", out)
	}
}

func TestX13sNoKernelMainLoopCannotBypassRequirements(t *testing.T) {
	script := `source src/dracut/70tbox-x13s-adsp/module-setup.sh
no_kernel=yes
initdir=/unused-test-path
module_install(){ install; }
module_installkernel(){ installkernel; }
dfatal(){ :; }
source testdata/x13s/dracut108-install-control-flow.sh`
	if err := exec.Command("bash", "-c", script).Run(); err == nil {
		t.Fatal("no-kernel mode bypassed mandatory profile inputs")
	}
}
