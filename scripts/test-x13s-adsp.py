#!/usr/bin/env python3
"""Run the real live shell scripts against isolated, simulated boot state.

Mount, modprobe, udev and sleep are stubs. Absolute boot paths are relocated.
Only the block-node predicate is replaced by a regular-file predicate so no
mknod, actual device, mount, firmware load, or root privilege is needed.
This tests a software-order barrier; it cannot establish physical USB safety.
"""
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import unittest

REPO = Path(__file__).resolve().parents[1]
SCRIPTS = REPO / 'src/dracut/90tbox-live'
FW = 'qcom/sc8280xp/LENOVO/21BX/qcadsp8280.mbn'


class X13sADSP(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix='tbox-x13s-')
        self.root = Path(self.temp.name)
        for p in ['bin', 'run/initramfs/live/LiveOS', 'lib/firmware/' + str(Path(FW).parent),
                  'etc/udev/rules.d', 'etc/tacklebox', 'dev/disk/by-label', 'proc',
                  'sys/firmware/devicetree/base',
                  'sys/bus/platform/devices/3000000.remoteproc/remoteproc/remoteproc3',
                  'sys/bus/platform/drivers/qcom_q6v5_pas']:
            (self.root / p).mkdir(parents=True, exist_ok=True)
        self.selector = self.root / 'dev/disk/by-label/TEST'
        self.disk = self.root / 'dev/sda'
        self.disk.touch()
        self.selector.symlink_to(self.disk)
        (self.root / 'run/initramfs/live/LiveOS/rootfs.sfs').write_text('fake rootfs')
        (self.root / 'proc/mounts').touch()
        (self.root / 'proc/cmdline').write_text('tacklebox.x13s-adsp.timeout=3\n')
        (self.root / 'proc/uptime').write_text('10.00 0.00\n')
        self.required = self.root / 'etc/tacklebox/x13s-adsp-required'
        self.required.touch()
        self.compatible = self.root / 'sys/firmware/devicetree/base/compatible'
        self.compatible.write_bytes(b'lenovo,thinkpad-x13s\0qcom,sc8280xp\0')
        self.node = self.root / 'sys/bus/platform/devices/3000000.remoteproc'
        (self.node / 'modalias').write_text('of:NremoteprocT(null)Cqcom,sc8280xp-adsp-pas\n')
        (self.node / 'driver').symlink_to(self.root / 'sys/bus/platform/drivers/qcom_q6v5_pas')
        self.rproc = self.node / 'remoteproc/remoteproc3'
        (self.rproc / 'firmware').write_text(FW + '\n')
        self.set_state('offline')
        self.firmware = self.root / 'lib/firmware' / FW
        self.firmware.write_text('simulated firmware bytes; never loaded')
        self.marker = self.root / 'run/tbox-x13s-adsp-pre-udev-loaded'
        self.done = self.root / 'run/tacklebox-live-done'
        self.env = dict(os.environ, TRACE=str(self.root / 'trace'), FIXROOT=str(self.root),
                        PATH=f'{self.root}/bin:/usr/bin:/bin', FALLBACK='0', MODPROBE_FAIL='0',
                        BECOME_RUNNING_AT='0', REENUMERATE_AT='0', SETTLE_FAIL='0',
                        RESOLVE_RETARGET_AT='0', SETTLE_CRASH='0', SETTLE_ADVANCE='0')
        self.stub('modprobe', '''printf 'modprobe %s\\n' "$*" >> "$TRACE"
[ "$1" != qcom_q6v5_pas ] || [ "$MODPROBE_FAIL" = 0 ]''')
        self.stub('udevadm', '''printf 'udevadm %s\\n' "$*" >> "$TRACE"
if [ "$1" = settle ]; then
    if [ "$SETTLE_ADVANCE" != 0 ]; then
        read -r now rest < "$FIXROOT/proc/uptime"
        now=${now%%.*}
        printf '%s.00 0.00\\n' "$((now + SETTLE_ADVANCE))" > "$FIXROOT/proc/uptime"
    fi
    if [ "$SETTLE_CRASH" = 1 ]; then
        printf 'crashed\\n' > "$FIXROOT/sys/bus/platform/devices/3000000.remoteproc/remoteproc/remoteproc3/state"
    fi
    [ "$SETTLE_FAIL" = 0 ] || exit 1
fi''')
        self.stub('mount', 'printf "mount %s\\n" "$*" >> "$TRACE"')
        self.stub('initqueue', 'printf "queued %s\\n" "$*" >> "$TRACE"')
        self.stub('sleep', '''printf 'poll-sleep %s\\n' "$*" >> "$TRACE"
read -r now rest < "$FIXROOT/proc/uptime"
now=${now%%.*}
now=$((now + 1))
printf '%s.00 0.00\\n' "$now" > "$FIXROOT/proc/uptime"
if [ "$BECOME_RUNNING_AT" != 0 ] && [ "$now" -ge "$BECOME_RUNNING_AT" ]; then
    printf 'running\\n' > "$FIXROOT/sys/bus/platform/devices/3000000.remoteproc/remoteproc/remoteproc3/state"
fi
if [ "$REENUMERATE_AT" != 0 ] && [ "$now" -ge "$REENUMERATE_AT" ]; then
    : > "$FIXROOT/dev/sdb"
    /bin/ln -sfn "$FIXROOT/dev/sdb" "$FIXROOT/dev/disk/by-label/TEST"
    /bin/rm -f "$FIXROOT/dev/sda"
fi''')
        self.stub('readlink', '''case "$2" in
    "$FIXROOT"/dev/disk/*)
        n=0
        [ ! -f "$FIXROOT/resolve-count" ] || read -r n < "$FIXROOT/resolve-count"
        n=$((n + 1))
        printf '%s\\n' "$n" > "$FIXROOT/resolve-count"
        printf 'resolve %s %s\\n' "$n" "$2" >> "$TRACE"
        if [ "$RESOLVE_RETARGET_AT" != 0 ] && [ "$n" -ge "$RESOLVE_RETARGET_AT" ]; then
            : > "$FIXROOT/dev/sdb"
            /bin/ln -sfn "$FIXROOT/dev/sdb" "$FIXROOT/dev/disk/by-label/TEST"
            /bin/rm -f "$FIXROOT/dev/sda"
        fi ;;
esac
exec /usr/bin/readlink "$@"''')
        self.stub('mkdir', '''if [ "$FALLBACK" = 1 ]; then
    for a in "$@"; do case "$a" in "$FIXROOT"/hooks/initqueue*) exit 1;; esac; done
fi
exec /bin/mkdir "$@"''')
        (self.root / 'lib/dracut-lib.sh').write_text('''getarg() {
    key=${1%=}
    for arg in $(cat "$FIXROOT/proc/cmdline"); do
        case "$arg" in "$key="*) printf '%s' "${arg#*=}"; return 0;; esac
    done
    return 1
}
wait_for_dev() { printf 'wait-for-dev %s\\n' "$*" >> "$TRACE"; }
die() { echo "$*" >&2; exit 1; }
warn() { echo "$*" >&2; }
''')
        for source, dest in [('parse-tbox-live.sh', 'parse.sh'),
                             ('tbox-live-root.sh', 'tbox-live-root'),
                             ('tbox-live-generator.sh', 'generator.sh'),
                             ('tbox-x13s-adsp-pre-udev.sh', 'pre-udev.sh'),
                             ('tbox-x13s-adsp.sh', 'lib/tbox-x13s-adsp.sh')]:
            text = (SCRIPTS / source).read_text()
            if source == 'tbox-x13s-adsp.sh':
                predicate = '[ -b "$tbox_x13s_resolved_device" ]'
                self.assertIn(predicate, text)
                text = text.replace(predicate, '[ -f "$tbox_x13s_resolved_device" ]')
            target = self.root / dest
            target.write_text(self.relocate(text))
            target.chmod(0o755)
        self.write_device()

    def tearDown(self):
        self.temp.cleanup()

    def relocate(self, text):
        for old in ['/run/', '/sys/', '/etc/tacklebox/', '/etc/udev/', '/dev/disk/',
                    '/proc/mounts', '/proc/uptime', '/proc/cmdline', '/lib/dracut-lib.sh',
                    '/lib/tbox-x13s-adsp.sh', '/lib/firmware', '/usr/lib/firmware']:
            text = text.replace(old, str(self.root) + old)
        return text.replace('/sbin/tbox-live-root', str(self.root / 'tbox-live-root'))

    def stub(self, name, body):
        path = self.root / 'bin' / name
        path.write_text('#!/bin/sh\n' + body + '\n')
        path.chmod(0o755)

    def write_device(self, value=None):
        (self.root / 'run/tbox-live-root.dev').write_text(str(value or self.selector))

    def set_state(self, state):
        (self.rproc / 'state').write_text(state + '\n')

    def trace(self):
        path = self.root / 'trace'
        return path.read_text().splitlines() if path.exists() else []

    def mounts(self):
        return [line for line in self.trace() if line.startswith('mount ')]

    def shell(self, script, **env):
        return subprocess.run(['/bin/sh', '-c', script], env=dict(self.env, **env),
                              text=True, capture_output=True, timeout=5)

    def hook(self, **env):
        result = self.shell(f'. {self.root}/lib/dracut-lib.sh; . {self.root}/pre-udev.sh', **env)
        self.assertEqual(result.returncode, 0, result.stderr)

    def cmdline(self, fallback=False):
        result = self.shell(f'''. {self.root}/lib/dracut-lib.sh
root=tbox:CDLABEL=TEST
hookdir={self.root}/hooks
. {self.root}/parse.sh
''', FALLBACK=str(int(fallback)))
        self.assertEqual(result.returncode, 0, result.stderr)

    def live(self, wait=False, **env):
        return subprocess.run([str(self.root / 'tbox-live-root')] + (['--wait'] if wait else []),
                              env=dict(self.env, **env), capture_output=True, text=True, timeout=5)

    def ready(self):
        self.hook()
        self.assertTrue(self.marker.exists())
        self.set_state('running')

    def assert_blocked(self, wait=False, **env):
        result = self.live(wait, **env)
        self.assertNotEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertEqual(self.mounts(), [], result.stdout + result.stderr)
        self.assertFalse(self.done.exists())
        return result

    def test_cmdline_fallback_cannot_mount_before_pre_udev_or_running(self):
        self.cmdline(True)
        self.assertEqual(self.mounts(), [])
        self.hook()
        self.assertTrue(self.marker.exists())
        self.assert_blocked()
        self.set_state('running')
        self.assertEqual(self.live().returncode, 0)
        self.assertLess(self.trace().index('modprobe qcom_q6v5_pas'),
                        next(i for i, line in enumerate(self.trace()) if line.startswith('mount ')))

    def test_normal_initqueue_defers_async_boot(self):
        self.cmdline()
        self.assertTrue(any(line.startswith('queued ') for line in self.trace()))
        self.assertEqual(self.mounts(), [])
        self.hook()
        self.assert_blocked()
        self.set_state('running')
        self.assertEqual(self.live().returncode, 0)

    def test_wait_succeeds_on_observed_running_without_fixed_ready_delay(self):
        self.hook()
        result = self.live(True, BECOME_RUNNING_AT='12')
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(len([line for line in self.trace() if line.startswith('poll-sleep ')]), 2)

    def test_wait_already_ready_never_sleeps(self):
        self.ready()
        self.assertEqual(self.live(True).returncode, 0)
        self.assertFalse(any(line.startswith('poll-sleep ') for line in self.trace()))

    def test_wait_deadline_does_not_create_readiness(self):
        self.hook()
        result = self.assert_blocked(True)
        self.assertIn('not running', result.stderr)
        self.assertEqual(len([line for line in self.trace() if line.startswith('poll-sleep ')]), 3)

    def test_wait_running_observed_only_at_deadline_stays_blocked(self):
        self.hook()
        self.assert_blocked(True, BECOME_RUNNING_AT='13')

    def test_settle_crossing_deadline_cannot_mount(self):
        self.ready()
        result = self.assert_blocked(True, SETTLE_ADVANCE='3')
        self.assertIn('deadline expired', result.stderr)

    def test_nonwait_never_sleeps_or_settles(self):
        self.hook()
        self.assert_blocked()
        self.assertFalse(any(line.startswith(('poll-sleep ', 'udevadm settle')) for line in self.trace()))

    def test_failed_modprobe_leaves_no_success_marker(self):
        self.marker.touch()
        self.hook(MODPROBE_FAIL='1')
        self.assertFalse(self.marker.exists())
        self.set_state('running')
        self.assert_blocked()

    def test_missing_firmware_bytes_rejects_correct_sysfs_name(self):
        self.firmware.unlink()
        self.hook()
        self.assertFalse(self.marker.exists())
        self.marker.touch()
        self.set_state('running')
        self.assert_blocked(True)

    def test_empty_firmware_rejected(self):
        self.firmware.write_bytes(b'')
        self.hook()
        self.assertFalse(self.marker.exists())
        self.assert_blocked()

    def test_compressed_firmware_bytes_accepted(self):
        for suffix in ['.xz', '.zst']:
            with self.subTest(suffix=suffix):
                source = self.firmware.with_name(self.firmware.name + suffix)
                self.firmware.rename(source)
                self.ready()
                self.assertEqual(self.live().returncode, 0)
                self.done.unlink()
                source.rename(self.firmware)

    def test_broken_firmware_symlink_rejected(self):
        self.firmware.unlink()
        self.firmware.symlink_to(self.root / 'absent-firmware')
        self.hook()
        self.assertFalse(self.marker.exists())
        self.assert_blocked()

    def test_resolved_firmware_symlink_accepted(self):
        target = self.root / 'lib/firmware/firmware-by-hash'
        self.firmware.rename(target)
        self.firmware.symlink_to(target)
        self.ready()
        self.assertEqual(self.live().returncode, 0)

    def test_missing_marker_blocks_running_adsp(self):
        self.set_state('running')
        self.assert_blocked()

    def test_done_marker_does_not_bypass_readiness(self):
        self.done.touch()
        self.set_state('running')
        result = self.live()
        self.assertEqual(result.returncode, 75)
        self.assertEqual(self.mounts(), [])

    def test_missing_or_wrong_model_blocks(self):
        for model in [b'', b'qcom,other\0', b'prefix-lenovo,thinkpad-x13s\0']:
            with self.subTest(model=model):
                self.compatible.write_bytes(model)
                self.hook()
                self.assertFalse(self.marker.exists())
                self.assert_blocked()

    def test_missing_platform_adsp_blocks(self):
        self.ready()
        (self.node / 'modalias').unlink()
        self.assert_blocked()

    def test_compatible_substring_is_not_the_adsp_device(self):
        self.ready()
        (self.node / 'modalias').write_text('of:NremoteprocT(null)Cqcom,sc8280xp-adsp-pas-wrong\n')
        self.assert_blocked()

    def test_ambiguous_platform_adsp_blocks(self):
        self.ready()
        other = self.node.with_name('4000000.remoteproc')
        other.mkdir()
        (other / 'modalias').write_text('of:NremoteprocT(null)Cqcom,sc8280xp-adsp-pas\n')
        self.assert_blocked()

    def test_missing_driver_binding_blocks(self):
        self.ready()
        (self.node / 'driver').unlink()
        self.assert_blocked()

    def test_wrong_driver_binding_blocks(self):
        self.ready()
        (self.node / 'driver').unlink()
        target = self.root / 'sys/bus/platform/drivers/wrong'
        target.mkdir()
        (self.node / 'driver').symlink_to(target)
        self.assert_blocked()

    def test_missing_remoteproc_blocks(self):
        self.ready()
        shutil.rmtree(self.rproc)
        self.assert_blocked()

    def test_ambiguous_remoteproc_blocks(self):
        self.ready()
        self.rproc.with_name('remoteproc4').mkdir()
        self.assert_blocked()

    def test_wrong_firmware_attribute_blocks(self):
        self.ready()
        (self.rproc / 'firmware').write_text('wrong.mbn\n')
        self.assert_blocked()

    def test_missing_firmware_attribute_blocks(self):
        self.ready()
        (self.rproc / 'firmware').unlink()
        self.assert_blocked()

    def test_crashed_or_missing_state_blocks(self):
        self.ready()
        self.set_state('crashed')
        self.assert_blocked()
        (self.rproc / 'state').unlink()
        self.assert_blocked()

    def test_changed_state_after_settle_blocks(self):
        self.ready()
        self.assert_blocked(True, SETTLE_CRASH='1')

    def test_unsettled_udev_is_bounded(self):
        self.ready()
        self.assert_blocked(True, SETTLE_FAIL='1')
        self.assertEqual(len([line for line in self.trace() if line.startswith('poll-sleep ')]), 3)

    def test_stable_selector_re_resolved_after_wait_and_reenumeration(self):
        self.hook()
        result = self.live(True, BECOME_RUNNING_AT='12', REENUMERATE_AT='11')
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn(str(self.root / 'dev/sdb'), self.mounts()[0])
        self.assertNotIn(str(self.root / 'dev/sda'), self.mounts()[0])

    def test_stable_selector_re_resolved_at_mount_boundary(self):
        self.ready()
        result = self.live(RESOLVE_RETARGET_AT='2')
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn(str(self.root / 'dev/sdb'), self.mounts()[0])

    def test_numbered_device_selector_is_rejected(self):
        self.ready()
        self.write_device(self.disk)
        result = self.assert_blocked(True)
        self.assertIn('stable', result.stderr)
        self.assertFalse(any(line.startswith('poll-sleep ') for line in self.trace()))

    def test_selector_path_traversal_is_rejected(self):
        self.ready()
        self.write_device(str(self.root) + '/dev/disk/by-label/../../sda')
        result = self.assert_blocked(True)
        self.assertIn('stable', result.stderr)

    def test_selector_must_be_a_symlink(self):
        self.ready()
        self.selector.unlink()
        self.selector.touch()
        self.assert_blocked()

    def test_missing_medium_has_bounded_wait(self):
        self.ready()
        self.disk.unlink()
        result = self.assert_blocked(True)
        self.assertIn('medium has not appeared', result.stderr)

    def test_repeated_success_is_idempotent(self):
        self.ready()
        self.assertEqual(self.live().returncode, 0)
        mounts = list(self.mounts())
        self.assertEqual(self.live().returncode, 0)
        self.assertEqual(self.mounts(), mounts)

    def test_invalid_timeouts_block_without_polling(self):
        self.hook()
        for value in ['0', '301', 'bad', '-1', '99999999999999999999']:
            with self.subTest(value=value):
                (self.root / 'proc/cmdline').write_text('tacklebox.x13s-adsp.timeout=' + value)
                result = self.assert_blocked(True)
                self.assertIn('invalid X13s ADSP timeout', result.stderr)
        self.assertFalse(any(line.startswith('poll-sleep ') for line in self.trace()))

    def test_timeout_leading_zero_is_decimal(self):
        self.hook()
        (self.root / 'proc/cmdline').write_text('tacklebox.x13s-adsp.timeout=008\n')
        result = self.assert_blocked(True)
        self.assertNotIn('invalid X13s ADSP timeout', result.stderr)
        self.assertEqual(len([line for line in self.trace() if line.startswith('poll-sleep ')]), 8)

    def test_timeout_upper_bound_300_is_accepted(self):
        self.ready()
        (self.root / 'proc/cmdline').write_text('tacklebox.x13s-adsp.timeout=300\n')
        result = self.live(True)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertFalse(any(line.startswith('poll-sleep ') for line in self.trace()))

    def test_invalid_uptime_blocks(self):
        self.hook()
        (self.root / 'proc/uptime').write_text('invalid\n')
        result = self.assert_blocked(True)
        self.assertIn('monotonic', result.stderr)

    def test_karg_opt_in_without_requirement_file(self):
        self.required.unlink()
        (self.root / 'proc/cmdline').write_text('tacklebox.x13s-adsp=required\n')
        self.hook()
        self.assertTrue(self.marker.exists())
        self.assert_blocked()

    def test_missing_helper_fails_closed_for_file_and_karg(self):
        (self.root / 'lib/tbox-x13s-adsp.sh').unlink()
        result = self.assert_blocked()
        self.assertIn('helper is absent', result.stderr)
        self.required.unlink()
        (self.root / 'proc/cmdline').write_text('tacklebox.x13s-adsp=required\n')
        self.assert_blocked()

    def test_generic_boot_unchanged_without_opt_in_or_helper(self):
        self.required.unlink()
        self.hook()
        self.assertFalse(self.marker.exists())
        self.assertNotIn('modprobe qcom_q6v5_pas', self.trace())
        (self.root / 'lib/tbox-x13s-adsp.sh').unlink()
        self.assertEqual(self.live().returncode, 0)
        self.assertTrue(self.mounts())

    def test_generic_cmdline_fallback_still_works(self):
        self.required.unlink()
        self.cmdline(True)
        self.assertTrue(self.mounts())

    def test_generator_orders_prepare_and_sysroot(self):
        early = self.root / 'generator-early'
        env = dict(self.env, root='tbox:CDLABEL=TEST')
        result = subprocess.run(['/bin/sh', str(self.root / 'generator.sh'),
                                 str(self.root / 'generator-normal'), str(early),
                                 str(self.root / 'generator-late')],
                                env=env, capture_output=True, text=True, timeout=5)
        self.assertEqual(result.returncode, 0, result.stderr)
        prepare = (early / 'tbox-live-prepare.service').read_text()
        self.assertIn('After=systemd-udevd.service dracut-initqueue.service', prepare)
        self.assertIn(' --wait', prepare)
        mount = (early / 'sysroot.mount').read_text()
        self.assertIn('Requires=tbox-live-prepare.service', mount)
        self.assertIn('After=dracut-initqueue.service tbox-live-prepare.service', mount)

    def test_module_installs_real_helper_hook_and_binaries(self):
        result = subprocess.run(['bash', '-c', f'''
moddir={SCRIPTS}
systemdutildir=/usr/lib/systemd
inst_hook() {{ echo hook "$@"; }}
inst_script() {{ echo script "$@"; }}
inst_multiple() {{ echo binaries "$@"; }}
dracut_module_included() {{ return 0; }}
dracut_need_initqueue() {{ echo initqueue; }}
. {SCRIPTS}/module-setup.sh
install
'''], capture_output=True, text=True, check=True)
        self.assertIn('hook pre-udev 30 ' + str(SCRIPTS / 'tbox-x13s-adsp-pre-udev.sh'), result.stdout)
        self.assertIn('/lib/tbox-x13s-adsp.sh', result.stdout)
        self.assertIn('binaries cat grep modprobe readlink rm sed sleep tr udevadm', result.stdout)

    def test_runtime_does_not_blacklist_or_write_remoteproc_state(self):
        for filename in ['tbox-x13s-adsp.sh', 'tbox-x13s-adsp-pre-udev.sh']:
            text = (SCRIPTS / filename).read_text()
            self.assertNotIn('blacklist', text)
            self.assertNotIn('install qcom_q6v5_pas', text)
            self.assertNotRegex(text, r'>\s*["\']?[^\n]*?/state')


if __name__ == '__main__':
    unittest.main(verbosity=2)
