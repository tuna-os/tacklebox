#!/usr/bin/env python3
"""Offline regressions for the fixture producer. Never downloads or runs containers."""
import ast
import base64
import copy
import hashlib
import io
import json
import lzma
import os
from pathlib import Path
import stat
import tempfile
import textwrap
import time
from types import SimpleNamespace
import unittest
from unittest import mock


DIRECTORY = Path(__file__).resolve().parent
SHELL = (DIRECTORY / "build.sh").read_text()
PYTHON = SHELL.split("<<'PY'\n", 1)[1].rsplit("\nPY", 1)[0]
TREE = ast.parse(PYTHON)
# Remove exactly the final dispatch, leaving only imports/constants/functions.
# Fail if its shape changes rather than accidentally executing install/prepare.
assert isinstance(TREE.body[-1], ast.Try)
assert len(TREE.body[-1].body) == 1 and isinstance(TREE.body[-1].body[0], ast.If)
TREE.body.pop()
CODE = compile(TREE, "build.sh:embedded-python", "exec")
NAMESPACE = {}
with mock.patch.dict(os.environ, {"TBOX_FIXTURE_SCRIPT_DIR": str(DIRECTORY)}):
    exec(CODE, NAMESPACE)

# Exact rpmkeys stdout from run 39377874335, job 118480012249 (RPM6.0.2).
RPM6_RECEIPT = (
    "/fixture/rpms/dracut-108-8.fc44.aarch64.rpm:\n"
    "    Header OpenPGP V4 RSA/SHA256 signature, key fingerprint: "
    "36f612dcf27f7d1a48a835e4dbfcf71c6d9f90a6: OK\n"
    "    Header SHA256 digest: OK\n"
    "    Payload SHA256 digest: OK\n"
)
# Retained 3494 native Ubuntu RPM4 receipt; this compatibility grammar remains.
RPM4_RECEIPT = (
    "/home/runner/work/_temp/x13s-handoff/kernel-uki-dtbloader.rpm:\n"
    "    Header V4 RSA/SHA256 Signature, key ID 6d9f90a6: OK\n"
    "    Header SHA256 digest: OK\n"
    "    Header SHA1 digest: OK\n"
    "    Payload SHA256 digest: OK\n"
    "    MD5 digest: OK\n"
)
APPROVED_KEY_ROW = (
    "36f612dcf27f7d1a48a835e4dbfcf71c6d9f90a6 "
    "Fedora (44) <fedora-44-primary@fedoraproject.org> public key\n"
)
# Extracted from verified dracut-108-8.fc44 /usr/bin/dracut, lines 2484/2609.
# File SHA256: 9399a7041414a13b213ca55b8ef4d943b8dafb2f7b5545997fbf443d5177d660.
# The normal archive uses line 2609; line 2512 is the disabled early-cpio path.
FEDORA108_OWNER_DEFAULT = '[[ $EUID != 0 ]] && cpio_owner="0:0"'
FEDORA108_CPIO_COMMAND = (
    'cpio -o ${CPIO_REPRODUCIBLE:+--reproducible} --null '
    '${cpio_owner:+-R "$cpio_owner"} -H newc --quiet'
)


def keyring_observations(db, keys=""):
    return {
        "version": (0, "RPM version 6.0.2\n"),
        "keyring-macros": (0, f"_dbpath={db}\n_keyring=rpmdb\n_keyringpath={db}/pubkeys/\n"
                          "_pkgverify_level=digest\n_pkgverify_flags=0x20100\n"),
        "key-list": (0, keys),
    }


def closure_observations():
    prefix = "usr/lib/modules/" + NAMESPACE["KVER"] + "/kernel/"
    modules = [prefix + "dependency.ko", prefix + "qcom_q6v5_pas.ko"]
    raw = "".join("insmod /" + name + "\n" for name in modules) + "builtin builtin_a\nbuiltin builtin_b\n"
    expected = {"modules": [{"archive_path": path, "name": name, "depends": deps,
                             "softdep": [], "weakdep": []}
                            for path, name, deps in zip(modules, ["dependency", "qcom_q6v5_pas"],
                                                       [[], ["dependency"]])],
                "builtins": ["builtin_a", "builtin_b"], "initrd": {"sha256": "b" * 64},
                "module_metadata": [],
                "modprobe": {"seed": "qcom_q6v5_pas", "args": ["modprobe", "--show-depends", "qcom_q6v5_pas"],
                            "lines": raw.splitlines(),
                            "stdout_sha256": hashlib.sha256(raw.encode()).hexdigest()}}
    receipt = {"schema_version": 1, "kernel_release": NAMESPACE["KVER"],
               "initrd_sha256": "b" * 64, "initrd_size": 78459904, "files": []}
    data = json.dumps(expected).encode()
    receipt.update(expected_size=len(data), expected_sha256=hashlib.sha256(data).hexdigest())
    return expected, receipt, modules, raw


def dependency_metadata_fixture(changes=None):
    items = []
    for scope in ("source", "emitted"):
        for name in ("modules.softdep", "modules.weakdep"):
            data = (changes or {}).get((scope, name), b"# controlled offline rule fixture\n")
            items.append({"scope": scope, "name": name, "path": scope + "/" + name,
                          "size": len(data), "sha256": hashlib.sha256(data).hexdigest(),
                          "base64": base64.b64encode(data).decode()})
    return items


def run8_dependency_metadata():
    # Exact four rule records from native attempt8 verification/closure-comparison.json.
    # Evidence ZIP SHA256 a98a6eae56f551ed2bb691a40a2e0ecd8bc183839a2157d28daefbcaa01e61c2.
    records = [
        ('source', 'modules.softdep',
         'source/usr/lib/modules/7.2.11-200.fc44.aarch64/modules.softdep',
         951, '40ecf69225c99e9980bf34b815215c94d2c97459e1d72fc7861c1b5400f76027',
         (
             b'# Soft dependencies extracted from modules themselves.\n'
             b'softdep cifs gcm\n'
             b'softdep cifs ccm\n'
             b'softdep cifs aead2\n'
             b'softdep cifs aes\n'
             b'softdep cifs nls\n'
             b'softdep lrw pre: ecb\n'
             b'softdep gpio_mlxbf3 pre: pinctrl-mlxbf3\n'
             b'softdep ipmi_msghandler post: ipmi_devintf\n'
             b'softdep gcc_sdm845 pre: rpmhpd\n'
             b'softdep dtpm pre: panfrost cpufreq-dt\n'
             b'softdep host1x post: tegra-drm\n'
             b'softdep lima pre: governor_simpleondemand\n'
             b'softdep panfrost pre: governor_simpleondemand\n'
             b'softdep cxl_acpi pre: cxl_port\n'
             b'softdep r8169 pre: realtek\n'
             b'softdep ohci_pci pre: ehci_pci\n'
             b'softdep ohci_platform pre: ehci_platform\n'
             b'softdep uhci_hcd pre: ehci_pci\n'
             b'softdep max31760 pre: regmap_i2c\n'
             b'softdep ufshcd_core pre: governor_simpleondemand\n'
             b'softdep qat_4xxx pre: crypto-intel_qat\n'
             b'softdep qat_420xx pre: crypto-intel_qat\n'
             b'softdep vfio post: vfio_iommu_type1 vfio_iommu_spapr_tce\n'
             b'softdep act_mpls post: mpls_gso\n'
             b'softdep xt_LOG pre: nf_log_syslog\n'
             b'softdep xt_NFLOG pre: nfnetlink_log\n'
             b'softdep xt_TRACE pre: nf_log_syslog\n'
         )),
        ('source', 'modules.weakdep',
         'source/usr/lib/modules/7.2.11-200.fc44.aarch64/modules.weakdep',
         55, 'a1fffe1059d8150b5d402b3f284f507025a8d4b5881810cb17b3fda8b8ab9304',
         (
             b'# Weak dependencies extracted from modules themselves.\n'
         )),
        ('emitted', 'modules.softdep',
         'lib/modules/7.2.11-200.fc44.aarch64/modules.softdep',
         656, '89ecc83d1d672cc9ec5d090a10237b2944651d01ab49888f5e6ba3cfc1903918',
         (
             b'# Soft dependencies extracted from modules themselves.\n'
             b'softdep cifs gcm\n'
             b'softdep cifs ccm\n'
             b'softdep cifs aead2\n'
             b'softdep cifs aes\n'
             b'softdep cifs nls\n'
             b'softdep lrw pre: ecb\n'
             b'softdep gpio_mlxbf3 pre: pinctrl-mlxbf3\n'
             b'softdep ipmi_msghandler post: ipmi_devintf\n'
             b'softdep gcc_sdm845 pre: rpmhpd\n'
             b'softdep dtpm pre: panfrost cpufreq-dt\n'
             b'softdep host1x post: tegra-drm\n'
             b'softdep panfrost pre: governor_simpleondemand\n'
             b'softdep ohci_pci pre: ehci_pci\n'
             b'softdep ohci_platform pre: ehci_platform\n'
             b'softdep uhci_hcd pre: ehci_pci\n'
             b'softdep max31760 pre: regmap_i2c\n'
             b'softdep ufshcd_core pre: governor_simpleondemand\n'
             b'softdep qat_4xxx pre: crypto-intel_qat\n'
             b'softdep qat_420xx pre: crypto-intel_qat\n'
         )),
        ('emitted', 'modules.weakdep',
         'lib/modules/7.2.11-200.fc44.aarch64/modules.weakdep',
         55, 'a1fffe1059d8150b5d402b3f284f507025a8d4b5881810cb17b3fda8b8ab9304',
         (
             b'# Weak dependencies extracted from modules themselves.\n'
         )),
    ]
    return [{"scope": scope, "name": name, "path": path, "size": size, "sha256": sha,
             "base64": base64.b64encode(data).decode()}
            for scope, name, path, size, sha, data in records]


def run7_closure_observations():
    # Exact paths/names/depends and observed ordering from native run7 evidence
    # ZIP SHA256 f1af2efcd1262d0465e6d99f7b2e967efc95c63d8905ee0daddc50526bcf8230.
    # Index bytes were not uploaded in run7; rule fixtures are separately marked.
    rows = [
        ("rpmsg/rpmsg_core", []),
        ("soc/qcom/qmi_helpers", []),
        ("soc/qcom/smem", []),
        ("rpmsg/qcom_smd", ["smem", "rpmsg_core"]),
        ("soc/qcom/qcom_aoss", []),
        ("rpmsg/qcom_glink", ["rpmsg_core"]),
        ("rpmsg/qcom_glink_smem", ["smem", "qcom_glink"]),
        ("remoteproc/qcom_common", ["smem", "qcom_glink", "qcom_glink_smem", "qcom_smd"]),
        ("soc/qcom/mdt_loader", []),
        ("remoteproc/qcom_sysmon", ["qmi_helpers", "rpmsg_core"]),
        ("remoteproc/qcom_q6v5", ["qcom_aoss", "qcom_sysmon", "smem"]),
        ("remoteproc/qcom_pil_info", []),
        ("remoteproc/qcom_q6v5_pas",
         ["qcom_common", "qcom_q6v5", "mdt_loader", "qcom_sysmon", "smem", "qcom_pil_info"]),
    ]
    modules = [{"archive_path": f"usr/lib/modules/{NAMESPACE['KVER']}/kernel/drivers/{path}.ko.xz",
                "name": path.split("/")[-1], "depends": deps, "softdep": [], "weakdep": []}
               for path, deps in rows]
    raw = "".join("insmod //" + item["archive_path"].removeprefix("usr/") + " \n" for item in modules)
    expected = {"modules": modules, "builtins": [], "modprobe": {
        "seed": "qcom_q6v5_pas", "lines": raw.splitlines(),
        "stdout_sha256": hashlib.sha256(raw.encode()).hexdigest()}}
    observed = [modules[n]["archive_path"] for n in (0, 1, 2, 3, 4, 9, 10, 5, 6, 7, 8, 11, 12)]
    return expected, observed


class FixtureProducerTests(unittest.TestCase):
    def test_reviewed_pin_shape_and_container_base(self):
        pins = json.loads((DIRECTORY / "pins.json").read_text())
        self.assertEqual(pins["architecture"], "aarch64")
        self.assertEqual(pins["kernel_release"], "7.2.11-200.fc44.aarch64")
        self.assertEqual(pins["source_date_epoch"], 1792078574)
        self.assertEqual(pins["fedora44_fingerprint"],
                         "36F612DCF27F7D1A48A835E4DBFCF71C6D9F90A6")
        self.assertEqual(pins["max_initrd_bytes"], 1 << 30)
        self.assertFalse(pins["transitive_dependencies_pinned"])
        self.assertIn("FROM " + pins["base_image"] + "\n",
                      (DIRECTORY / "Containerfile").read_text())
        self.assertEqual([p["name"] for p in pins["packages"]],
                         ["dracut", "kernel-core", "kernel-modules-core", "qcom-firmware"])
        self.assertEqual(sum(p["size"] for p in pins["packages"]), 250824456)
        for pin in pins["packages"]:
            self.assertRegex(pin["sha256"], r"^[0-9a-f]{64}$")
            self.assertTrue(pin["url"].startswith(
                "https://dl.fedoraproject.org/pub/fedora/linux/updates/44/Everything/aarch64/"))
            self.assertTrue(pin["url"].endswith(
                f"/{pin['name']}-{pin['version']}-{pin['release']}.{pin['arch']}.rpm"))

    def test_signature_requires_approved_explicit_header(self):
        good = ("    Header V4 RSA/SHA256 Signature, key ID 6d9f90a6: OK\n"
                "    Payload SHA256 digest: OK\n")
        check = NAMESPACE["signed_rpm"]
        self.assertTrue(check(good, 0))
        self.assertTrue(check(good.replace("6d9f90a6",
            "36F612DCF27F7D1A48A835E4DBFCF71C6D9F90A6"), 0))
        for text, code in [
            ("Payload SHA256 digest: OK\n", 0),  # rpmkeys may exit zero unsigned.
            (good, 1),
            (good.replace("6d9f90a6", "12345678"), 0),
            (good + "NOKEY", 0),
            (good + "NOT OK", 0),
            (good.replace("Header V4", "Header V5"), 0),
            (good.replace(": OK", ": BAD"), 0),
        ]:
            with self.subTest(text=text, code=code):
                self.assertFalse(check(text, code))

    def test_actual_rpm6_and_rpm4_receipts_and_strict_negatives(self):
        check = NAMESPACE["signed_rpm"]
        self.assertTrue(check(RPM6_RECEIPT, 0))
        self.assertTrue(check(RPM4_RECEIPT, 0))
        fingerprint = NAMESPACE["PINS"]["fedora44_fingerprint"].lower()
        for receipt in (RPM4_RECEIPT, RPM6_RECEIPT):
            self.assertFalse(check(receipt, 1))
            signature = next(line for line in receipt.splitlines() if "signature" in line.lower())
            for altered in (
                receipt.replace(signature + "\n", ""),  # unsigned/digest-only
                receipt + signature + "\n",             # duplicate signature
                receipt + "Header future-format signature: OK\n",
                receipt + "\0",
                receipt.replace("RSA/SHA256", "RSA/SHA512"),
                receipt.replace("V4", "V5"),
                receipt.replace(": OK", ": BAD"),
                receipt.replace(": OK", ": NOKEY"),
                receipt.replace(": OK", ": NOTTRUSTED"),
                receipt.replace(": OK", ": NOTFOUND"),
                receipt.replace(": OK", ": UNKNOWN"),
                receipt.replace("Payload SHA256 digest: OK", "Payload SHA256 digest: UNKNOWN"),
                receipt + "warning: unsupported verification state\n",
            ):
                with self.subTest(receipt=altered):
                    self.assertFalse(check(altered, 0))
        for altered in (
            RPM4_RECEIPT + RPM6_RECEIPT,  # mixed known grammars
            RPM6_RECEIPT.replace(fingerprint, "0" + fingerprint[1:]),  # same short ID
            RPM6_RECEIPT.replace(fingerprint, fingerprint[-16:]),
            RPM6_RECEIPT.replace(fingerprint, fingerprint + "0"),
            RPM6_RECEIPT.replace("key fingerprint:", "key ID"),
            RPM4_RECEIPT.replace("6d9f90a6", "12345678"),
            RPM4_RECEIPT.replace("6d9f90a6", fingerprint[-12:]),  # arbitrary suffix forbidden
        ):
            with self.subTest(receipt=altered):
                self.assertFalse(check(altered, 0))

    def test_signature_diagnostics_preserve_failure_and_bound_output(self):
        receipt = ("Header OpenPGP V4 RSA/SHA256 signature, key fingerprint: "
                   "36f612dcf27f7d1a48a835e4dbfcf71c6d9f90a6: OK\n")
        with self.assertRaises(ValueError) as caught:
            NAMESPACE["signature_failure"]("dracut", 0, receipt, "RPM version 6\n")
        message = str(caught.exception)
        self.assertIn(receipt, message)
        self.assertIn("rpmkeys exit=0", message)
        self.assertIn("RPM version 6", message)
        with self.assertRaises(ValueError) as caught:
            NAMESPACE["signature_failure"]("dracut", 1, "X" * 30000 + receipt, "Y" * 30000)
        self.assertLess(len(str(caught.exception).encode()), 13000)
        self.assertIn(receipt, str(caught.exception))

    def test_keyring_diagnostics_are_read_only_and_scoped(self):
        calls = []
        def diagnostic(command, path, **kwargs):
            calls.append((command, path, kwargs))
            observation = keyring_observations(db)
            name = ("version" if command[-1] == "--version" else
                    "key-list" if command[-1] == "--list" else "keyring-macros")
            return observation[name]
        with tempfile.TemporaryDirectory() as directory, \
                mock.patch.dict(NAMESPACE, {"log_run": diagnostic}):
            db, evidence = Path(directory) / "keys", Path(directory)
            result = NAMESPACE["rpm_keyring_diagnostics"](db, evidence, "before-import")
        self.assertEqual(len(calls), 3)
        self.assertIn("before-import key-list exit=0", result)
        for command, _, options in calls:
            self.assertFalse(options["check"])
            self.assertEqual(options["maximum"], 16384)
            self.assertNotIn("--import", command)
            self.assertNotIn("--delete", command)
            self.assertNotIn("--showrc", command)
        self.assertEqual(calls[1][0][1:3], ["--dbpath", db])
        self.assertEqual(calls[2][0], ["rpmkeys", "--dbpath", db, "--list"])
        for macro in ("_dbpath", "_keyring", "_keyringpath", "_pkgverify_level", "_pkgverify_flags"):
            self.assertIn("%{" + macro + "}", calls[1][0][-1])

    def test_keyring_isolation_requires_empty_then_single_approved_key(self):
        db = Path("/fixture/rpmkeys")
        validate = NAMESPACE["validate_rpm_keyring"]
        validate(db, "before-import", keyring_observations(db))
        validate(db, "after-import", keyring_observations(db, APPROVED_KEY_ROW))
        cases = [
            ("before-import", keyring_observations(db, APPROVED_KEY_ROW)),
            ("after-import", keyring_observations(db)),
            ("after-import", keyring_observations(db, APPROVED_KEY_ROW * 2)),
            ("after-import", keyring_observations(db, APPROVED_KEY_ROW.replace("36f612dc", "00000000"))),
            ("after-import", keyring_observations(db, APPROVED_KEY_ROW.replace("public key", "secret key"))),
        ]
        for field, value in (
            ("version", "RPM version 6.1.0\n"),
            ("keyring-macros", keyring_observations(db)["keyring-macros"][1].replace("_keyring=rpmdb", "_keyring=fs")),
            ("keyring-macros", keyring_observations(db)["keyring-macros"][1].replace(str(db), "/usr/lib/sysimage/rpm")),
            ("keyring-macros", keyring_observations(db)["keyring-macros"][1].replace("pubkeys/", "other/")),
            ("keyring-macros", keyring_observations(db)["keyring-macros"][1].replace("0x20100", "0")),
            ("keyring-macros", keyring_observations(db)["keyring-macros"][1] + "_keyring=rpmdb\n"),
        ):
            observation = keyring_observations(db, APPROVED_KEY_ROW)
            observation[field] = (0, value)
            cases.append(("after-import", observation))
        for field in ("version", "keyring-macros", "key-list"):
            observation = keyring_observations(db, APPROVED_KEY_ROW)
            observation[field] = (1, observation[field][1])
            cases.append(("after-import", observation))
        for phase, observation in cases:
            with self.subTest(phase=phase, observation=observation), self.assertRaises(ValueError):
                validate(db, phase, observation)

    def test_digest_size_limit_and_nonfile(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "data"
            path.write_bytes(b"abc")
            self.assertEqual(NAMESPACE["digest"](path, 3), {
                "size": 3, "sha256": hashlib.sha256(b"abc").hexdigest()})
            with self.assertRaises(ValueError):
                NAMESPACE["digest"](path, 2)
            with self.assertRaises(ValueError):
                NAMESPACE["digest"](Path(directory), 3)

    def test_initrd_handoff_preserves_private_mode_identity_and_digest(self):
        with tempfile.TemporaryDirectory() as directory:
            out = Path(directory)
            image = out / "initrd.img"
            image.write_bytes(b"070701fixture")
            image.chmod(0o600)
            before = image.stat()
            with mock.patch("os.fchown", wraps=os.fchown) as chown:
                result = NAMESPACE["handoff_initrd"](out)
            self.assertEqual(result, NAMESPACE["digest"](image))
            after = image.stat()
            self.assertEqual((before.st_dev, before.st_ino, before.st_size),
                             (after.st_dev, after.st_ino, after.st_size))
            self.assertEqual((after.st_uid, after.st_gid), (out.stat().st_uid, out.stat().st_gid))
            self.assertEqual(stat.S_IMODE(after.st_mode), 0o600)
            self.assertEqual(stat.S_IMODE(out.stat().st_mode), 0o700)
            self.assertEqual(chown.call_count, 1)
            self.assertEqual(chown.call_args.args[1:], (out.stat().st_uid, out.stat().st_gid))

    def test_initrd_handoff_root_to_runner_on_exact_descriptor(self):
        # No privileges needed: simulate only kernel ownership observations and
        # fchown; all open/read/path/inode checks use a real owned temporary file.
        with tempfile.TemporaryDirectory() as directory:
            out = Path(directory)
            image = out / "initrd.img"
            image.write_bytes(b"070701fixture")
            image.chmod(0o600)
            original_fstat, changed = os.fstat, []
            runner = (12345, 12346)
            def observed(fd):
                value = original_fstat(fd)
                fields = {name: getattr(value, name) for name in dir(value) if name.startswith("st_")}
                fields["st_uid"], fields["st_gid"] = (runner if stat.S_ISDIR(value.st_mode) or changed
                                                     else (0, 0))
                return SimpleNamespace(**fields)
            def chown(fd, uid, gid):
                self.assertEqual(original_fstat(fd).st_ino, image.stat().st_ino)
                self.assertEqual((uid, gid), runner)
                changed.append(fd)
            original_stat = os.stat
            def path_stat(path, **kwargs):
                value = original_stat(path, **kwargs)
                if Path(path) == out:
                    fields = {name: getattr(value, name) for name in dir(value) if name.startswith("st_")}
                    fields["st_uid"], fields["st_gid"] = runner
                    return SimpleNamespace(**fields)
                return value
            with mock.patch("os.fstat", side_effect=observed), \
                    mock.patch("os.stat", side_effect=path_stat), \
                    mock.patch("os.geteuid", return_value=0), \
                    mock.patch("os.fchown", side_effect=chown):
                result = NAMESPACE["handoff_initrd"](out)
            self.assertEqual(len(changed), 1)
            self.assertEqual(result, NAMESPACE["digest"](image))
            self.assertEqual(stat.S_IMODE(image.stat().st_mode), 0o600)

    def test_initrd_handoff_rejects_symlinks_and_path_escape(self):
        with tempfile.TemporaryDirectory() as directory:
            base = Path(directory)
            out = base / "out"
            out.mkdir(mode=0o700)
            outside = base / "other.img"
            outside.write_bytes(b"070701outside")
            outside.chmod(0o600)
            image = out / "initrd.img"
            image.symlink_to(outside)
            alias = base / "alias"
            alias.symlink_to(out, target_is_directory=True)
            with mock.patch("os.fchown", side_effect=AssertionError("ownership changed")):
                for path in (out, alias, out / ".." / "out", Path("relative")):
                    with self.subTest(path=path), self.assertRaises((ValueError, OSError)):
                        NAMESPACE["handoff_initrd"](path)
                image.unlink()
                os.link(outside, image)
                with self.assertRaises(ValueError):
                    NAMESPACE["handoff_initrd"](out)
            self.assertEqual(outside.read_bytes(), b"070701outside")
            self.assertEqual(stat.S_IMODE(outside.stat().st_mode), 0o600)

    def test_initrd_handoff_rejects_nonprivate_nonregular_and_unbounded(self):
        with tempfile.TemporaryDirectory() as directory:
            out = Path(directory)
            image = out / "initrd.img"
            image.write_bytes(b"070701fixture")
            image.chmod(0o600)
            with mock.patch("os.fchown", side_effect=AssertionError("ownership changed")):
                out.chmod(0o755)
                with self.assertRaises(ValueError):
                    NAMESPACE["handoff_initrd"](out)
                out.chmod(0o700)
                image.chmod(0o644)
                with self.assertRaises(ValueError):
                    NAMESPACE["handoff_initrd"](out)
                image.chmod(0o600)
                with mock.patch.dict(NAMESPACE, {"LIMIT": 6}), self.assertRaises(ValueError):
                    NAMESPACE["handoff_initrd"](out)
                image.write_bytes(b"wrong!")
                with self.assertRaises(ValueError):
                    NAMESPACE["handoff_initrd"](out)
                image.unlink()
                image.mkdir(mode=0o700)
                with self.assertRaises(ValueError):
                    NAMESPACE["handoff_initrd"](out)

    def test_initrd_handoff_rejects_path_replacement_before_chown(self):
        with tempfile.TemporaryDirectory() as directory:
            out = Path(directory)
            image = out / "initrd.img"
            image.write_bytes(b"070701fixture")
            image.chmod(0o600)
            original_read, replaced = os.read, []
            def replacing_read(fd, size):
                value = original_read(fd, size)
                if not replaced:
                    image.rename(out / "original.img")
                    image.write_bytes(b"070701fixture")
                    image.chmod(0o600)
                    replaced.append(True)
                return value
            with mock.patch("os.read", side_effect=replacing_read), \
                    mock.patch("os.fchown", side_effect=AssertionError("ownership changed")), \
                    self.assertRaisesRegex(ValueError, "changed"):
                NAMESPACE["handoff_initrd"](out)

    def test_modprobe_config_evidence_is_exact_bounded_copied_source(self):
        with tempfile.TemporaryDirectory() as directory:
            out = Path(directory)
            (out / "evidence").mkdir()
            source = out / "source/usr/lib/modprobe.d/systemd.conf"
            source.parent.mkdir(parents=True)
            data = b"# package config\noptions example value=1\n"
            source.write_bytes(data)
            item = {"archive_path": "usr/lib/modprobe.d/systemd.conf",
                    "source_path": str(source.relative_to(out)), **NAMESPACE["digest"](source)}
            record = NAMESPACE["record_modprobe_config"]
            record(out, [item])
            evidence = json.loads((out / "evidence/source-modprobe-config.json").read_text())
            self.assertEqual(evidence, [{
                "archive_path": item["archive_path"], "size": item["size"],
                "sha256": item["sha256"], "text": data.decode()}])
            for items, maximum in (([item], len(data) - 1), ([item], 100),
                                   ([item, item], 400),
                                   ([{**item, "sha256": "0" * 64}], 1024),
                                   ([{**item, "source_path": "../outside"}], 1024),
                                   ([{**item, "source_path": "config/systemd.conf"}], 1024)):
                with self.subTest(items=items, maximum=maximum), self.assertRaises(ValueError):
                    record(out, items, maximum)
            source.unlink()
            source.symlink_to(out / "evidence/source-modprobe-config.json")
            with self.assertRaises(ValueError):
                record(out, [item])

    def test_xz_decoding_limit_truncation_and_trailing_data(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "data.xz"
            data = lzma.compress(b"abc")
            path.write_bytes(data)
            self.assertEqual(NAMESPACE["decoded_info"](path, 3), {
                "compression": "xz", "decoded_size": 3,
                "decoded_sha256": hashlib.sha256(b"abc").hexdigest()})
            with self.assertRaises(ValueError):
                NAMESPACE["decoded_info"](path, 2)
            for invalid in (data[:-1], data + b"\0", data + data):
                path.write_bytes(invalid)
                with self.subTest(size=len(invalid)), self.assertRaises(ValueError):
                    NAMESPACE["decoded_info"](path, 3)

    def test_exact_download_size_and_hash_without_network(self):
        class Response(io.BytesIO):
            url = "https://dl.fedoraproject.org/test"
            headers = {}
        pin = {"name": "test", "url":
               "https://dl.fedoraproject.org/pub/fedora/linux/test.rpm",
               "size": 3, "sha256": hashlib.sha256(b"abc").hexdigest()}
        for data, valid in [(b"abc", True), (b"ab", False),
                            (b"abcd", False), (b"xyz", False)]:
            with tempfile.TemporaryDirectory() as directory, \
                    mock.patch("urllib.request.build_opener") as opener:
                opener.return_value.open.return_value = Response(data)
                path = Path(directory) / "download"
                if valid:
                    NAMESPACE["download"](pin, path)
                    self.assertEqual(path.read_bytes(), data)
                else:
                    with self.assertRaises(ValueError):
                        NAMESPACE["download"](pin, path)

    def test_download_rejects_unreviewed_source_before_network(self):
        with tempfile.TemporaryDirectory() as directory, \
                mock.patch("urllib.request.build_opener",
                           side_effect=AssertionError("network attempted")):
            for url in ("http://dl.fedoraproject.org/pub/fedora/linux/test.rpm",
                        "https://example.com/test.rpm"):
                with self.assertRaises(ValueError):
                    NAMESPACE["download"]({"url": url}, Path(directory) / "download")

    def test_modprobe_closure_rejects_directives_options_and_ambiguity(self):
        path = f"/lib/modules/{NAMESPACE['KVER']}/kernel/qcom_q6v5_pas.ko.xz"
        with mock.patch.dict(NAMESPACE, {"run": lambda *a, **k:
                                         (0, "insmod " + path + "\nbuiltin foo\n")}):
            self.assertEqual(NAMESPACE["closure"]()[2:], ([path], ["foo"]))
        for text in ("", "install /bin/evil\n", "insmod " + path + " option=x\n",
                     "insmod " + path + "\ninsmod " + path + "\n",
                     "builtin qcom_q6v5_pas\n"):
            with mock.patch.dict(NAMESPACE, {"run": lambda *a, text=text, **k: (0, text)}), \
                    self.subTest(text=text), self.assertRaises(ValueError):
                NAMESPACE["closure"]()

    def test_closure_failure_preserves_ordered_evidence_before_rejection(self):
        expected, receipt, modules, source_raw = closure_observations()
        observed = list(reversed(modules))
        paths = ["/audit/" + name.removeprefix("usr/") for name in observed]
        emitted_raw = "".join("insmod " + name + "\n" for name in paths)
        args = ["modprobe", "--show-depends", "--dirname=/audit", "qcom_q6v5_pas"]
        record = NAMESPACE["record_closure_comparison"]
        with tempfile.TemporaryDirectory() as directory:
            result, source = Path(directory) / "verification", Path(directory) / "source.txt"
            result.mkdir()
            source.write_text(source_raw)
            def capture(_result, *values):
                return record(result, *values[:-2], source, values[-1])
            with mock.patch.dict(NAMESPACE, {
                    "read_json": lambda name: expected if name.endswith("expected.json") else receipt,
                    "read_bound_expected": lambda path, receipt: expected,
                    "closure": lambda *unused: (args, emitted_raw, paths, ["builtin_b", "builtin_a"]),
                    "read_dependency_metadata": lambda *args, **kwargs: dependency_metadata_fixture()[:2],
                    "record_closure_comparison": capture,
                    "write_json": mock.Mock(side_effect=AssertionError("success marker written"))}), \
                    mock.patch("os.walk", return_value=[]), \
                    self.assertRaisesRegex(ValueError, "builtin list differs"):
                NAMESPACE["internal_verify"]()
            comparison = json.loads((result / "closure-comparison.json").read_text())
            self.assertEqual(comparison["expected_modules"], modules)
            self.assertEqual(comparison["observed_modules"], observed)
            self.assertEqual(comparison["expected_builtins"], ["builtin_a", "builtin_b"])
            self.assertEqual(comparison["observed_builtins"], ["builtin_b", "builtin_a"])
            self.assertEqual(comparison["initrd_sha256"], receipt["initrd_sha256"])
            self.assertEqual(comparison["initrd_size"], receipt["initrd_size"])
            self.assertEqual(comparison["expected_sha256"], receipt["expected_sha256"])
            self.assertEqual(comparison["expected_size"], receipt["expected_size"])
            self.assertEqual(comparison["emitted_modprobe"]["args"], args)
            self.assertEqual(comparison["source_modprobe"]["args"], expected["modprobe"]["args"])
            for name, raw in (("source", source_raw), ("emitted", emitted_raw)):
                value = (result / (name + "-modprobe.txt")).read_bytes()
                self.assertEqual(value, raw.encode())
                self.assertEqual(comparison[name + "_modprobe"]["sha256"], hashlib.sha256(value).hexdigest())
            self.assertFalse((result / "verified.json").exists())

    def test_run7_independent_orders_require_inspected_hard_only_rules(self):
        expected, observed = run7_closure_observations()
        compare = NAMESPACE["compare_module_closure"]
        metadata = dependency_metadata_fixture()
        self.assertEqual(compare(expected, observed, [], metadata), "hard-dependency-equivalent")
        source = [m["archive_path"] for m in expected["modules"]]
        self.assertEqual(compare(expected, source, [], metadata), "exact-order")
        with self.assertRaisesRegex(ValueError, "incomplete"):
            compare(expected, observed, [], [])
        unrelated = dependency_metadata_fixture({
            ("emitted", "modules.softdep"): b"softdep unrelated pre: qcom_q6v5_pas post: other\n",
            ("source", "modules.weakdep"): b"weakdep unrelated qcom_q6v5_pas\n"})
        self.assertEqual(compare(expected, observed, [], unrelated), "hard-dependency-equivalent")

    def test_run8_actual_rule_records_allow_the_observed_hard_only_orders(self):
        metadata = run8_dependency_metadata()
        self.assertEqual([(m["scope"], m["name"]) for m in metadata],
                         [("source", "modules.softdep"), ("source", "modules.weakdep"),
                          ("emitted", "modules.softdep"), ("emitted", "modules.weakdep")])
        for item in metadata:
            data = base64.b64decode(item["base64"], validate=True)
            self.assertEqual((len(data), hashlib.sha256(data).hexdigest()),
                             (item["size"], item["sha256"]))
            prefix = "source/usr/" if item["scope"] == "source" else ""
            self.assertEqual(item["path"], f"{prefix}lib/modules/{NAMESPACE['KVER']}/{item['name']}")
        expected, observed = run7_closure_observations()
        self.assertEqual(NAMESPACE["compare_module_closure"](expected, observed, [], metadata),
                         "hard-dependency-equivalent")
        with self.assertRaisesRegex(ValueError, "required module.*cifs"):
            NAMESPACE["reject_required_dependency_rules"](metadata, {"cifs"})
        corrupt = copy.deepcopy(metadata)
        corrupt[0]["sha256"] = "0" * 64
        with self.assertRaisesRegex(ValueError, "identity mismatch"):
            NAMESPACE["reject_required_dependency_rules"](corrupt, set())

    def test_observed_cifs_empty_records_require_an_unrelated_literal_subject(self):
        # kmod v34.2 keeps phase-less targets in S_NONE and appends an empty
        # record. The first matching subject wins, so it may shadow later rules.
        # https://github.com/kmod-project/kmod/blob/v34.2/libkmod/libkmod-config.c#L221-L367
        # https://github.com/kmod-project/kmod/blob/v34.2/libkmod/libkmod-module.c#L1175-L1205
        check = NAMESPACE["reject_required_dependency_rules"]
        for scope in ("source", "emitted"):
            for target in ("gcm", "ccm", "aead2", "aes", "nls"):
                row = f"softdep cifs {target}\n".encode()
                for later in (b"", b"softdep cifs pre: qcom_q6v5_pas\n"):
                    metadata = dependency_metadata_fixture({(scope, "modules.softdep"): row + later})
                    with self.subTest(scope=scope, target=target, later=later):
                        check(metadata, {"qcom_q6v5_pas"})
                        with self.assertRaisesRegex(ValueError, "required module.*cifs"):
                            check(metadata, {"qcom_q6v5_pas", "cifs"})
        # A permitted cifs row never hides a later relevant rule for the seed.
        metadata = dependency_metadata_fixture({
            ("emitted", "modules.softdep"):
                b"softdep cifs gcm\nsoftdep qcom-q6v5-pas pre: other\n"})
        with self.assertRaisesRegex(ValueError, "required module.*qcom_q6v5_pas"):
            check(metadata, {"qcom_q6v5_pas"})

    def test_cifs_empty_record_exception_rejects_unobserved_or_mixed_syntax(self):
        expected, observed = run7_closure_observations()
        bad = (
            b"softdep other gcm\n", b"softdep CIFS gcm\n", b"softdep cifs unknown\n",
            b"softdep cifs GCM\n", b"softdep cifs\n", b"softdep cifs gcm ccm\n",
            b"softdep cifs gcm pre: other\n", b"softdep cifs gcm post: other\n",
            b"softdep cifs pre: gcm post:\n", b"softdep cifs pre: gcm pre: other\n",
            b"softdep cifs gcm # extra\n", b"softdep cif* gcm\n",
            b"softdep cif[s] gcm\n", b"softdep cifs gc*\n", b"softdep cifs gc[m]\n",
            b"softdep cifs gcm\\\n", b"softdep cifs gcm\x00\n", b"softdep cifs gcm\r\n",
            b"softdep cifs gcm\x0b\n", b"softdep cifs gcm\xff\n",
            b"softdep cifs gcm\nsoftdep cifs unknown\n",
        )
        for scope in ("source", "emitted"):
            for row in bad:
                metadata = dependency_metadata_fixture({(scope, "modules.softdep"): row})
                with self.subTest(scope=scope, row=row), self.assertRaises(ValueError):
                    NAMESPACE["compare_module_closure"](expected, observed, [], metadata)

    def test_source_order_requires_the_bound_raw_source_lines(self):
        original, observed = run7_closure_observations()
        compare = NAMESPACE["compare_module_closure"]
        metadata = dependency_metadata_fixture()
        swapped = copy.deepcopy(original)
        swapped["modules"][0], swapped["modules"][1] = swapped["modules"][1], swapped["modules"][0]
        with self.assertRaisesRegex(ValueError, "source inventory order"):
            compare(swapped, observed, [], metadata)
        for change in ("swap", "extra", "action", "interior-alias", "hash"):
            expected = copy.deepcopy(original)
            lines = expected["modprobe"]["lines"]
            if change == "swap":
                lines[0], lines[1] = lines[1], lines[0]
            elif change == "extra":
                lines.append("builtin other")
            elif change == "action":
                lines[0] = "install /bin/true"
            elif change == "interior-alias":
                lines[0] = lines[0].replace("/lib/modules/", "/lib//modules/")
            expected["modprobe"]["stdout_sha256"] = hashlib.sha256(("\n".join(lines) + "\n").encode()).hexdigest()
            if change == "hash":
                expected["modprobe"]["stdout_sha256"] = "0" * 64
            with self.subTest(change=change), self.assertRaises(ValueError):
                compare(expected, observed, [], metadata)

    def test_copied_source_stdout_mismatch_blocks_comparison(self):
        expected, receipt, modules, raw = closure_observations()
        with mock.patch.dict(NAMESPACE, {
                "read_json": lambda path: receipt,
                "read_bound_expected": lambda path, receipt: expected,
                "read_dependency_metadata": lambda *args, **kwargs: [],
                "closure": lambda *args: ([], raw, ["/audit/" + p.removeprefix("usr/") for p in modules],
                                         expected["builtins"]),
                "record_closure_comparison": lambda *args: {"source_modprobe": {"sha256": "0" * 64}},
                "compare_module_closure": mock.Mock(side_effect=AssertionError("comparison reached")),
                "write_json": mock.Mock(side_effect=AssertionError("success marker written"))}), \
                mock.patch("os.walk", return_value=[]), \
                self.assertRaisesRegex(ValueError, "copied raw source"):
            NAMESPACE["internal_verify"]()

    def test_closure_dag_rejects_inventory_identity_and_edge_failures(self):
        original, order = run7_closure_observations()
        metadata = dependency_metadata_fixture()
        compare = NAMESPACE["compare_module_closure"]
        for observed in (order[:-1], order + [order[0]], order[:-1] + [order[0]],
                         order + [order[-1].replace("qcom_q6v5_pas", "unexpected")],
                         list(reversed(order))):
            with self.subTest(observed=observed), self.assertRaises(ValueError):
                compare(original, observed, [], metadata)
        for change in ("seed", "name", "duplicate-name", "cycle", "missing-edge", "duplicate-edge",
                       "source-order", "unreachable", "softdep", "weakdep"):
            expected = copy.deepcopy(original)
            if change == "seed":
                expected["modprobe"]["seed"] = "other"
            elif change == "name":
                expected["modules"][0]["name"] = "other"
            elif change == "duplicate-name":
                expected["modules"][1]["name"] = expected["modules"][0]["name"]
            elif change == "cycle":
                expected["modules"][0]["depends"] = ["qcom_q6v5_pas"]
            elif change == "missing-edge":
                expected["modules"][-1]["depends"].append("absent")
            elif change == "duplicate-edge":
                expected["modules"][-1]["depends"].append("smem")
            elif change == "source-order":
                expected["modules"].reverse()
            elif change == "unreachable":
                expected["modules"][-1]["depends"].remove("mdt_loader")
            else:
                expected["modules"][-1][change] = ["pre: other" if change == "softdep" else "other"]
            with self.subTest(change=change), self.assertRaises(ValueError):
                compare(expected, order, [], metadata)
        with self.assertRaises(ValueError):
            compare(original, order, ["unexpected_builtin"], metadata)
        duplicate = copy.deepcopy(original)
        duplicate["modules"].append(copy.deepcopy(duplicate["modules"][0]))
        with self.assertRaises(ValueError):
            compare(duplicate, [m["archive_path"] for m in duplicate["modules"]], [], metadata)
        ambiguous = copy.deepcopy(original)
        alias = copy.deepcopy(ambiguous["modules"][0])
        alias["name"] = alias["name"].replace("_", "-")
        alias["archive_path"] = alias["archive_path"].replace("rpmsg_core", "rpmsg-core")
        ambiguous["modules"].append(alias)
        with self.assertRaises(ValueError):
            compare(ambiguous, [m["archive_path"] for m in ambiguous["modules"]], [], metadata)
        for field in ("softdep", "weakdep"):
            for noncanonical in (False, True):
                expected = copy.deepcopy(original)
                del expected["modules"][-1][field]
                if noncanonical:
                    expected["modules"][-1][field.capitalize()] = ["other"]
                with self.subTest(field=field, noncanonical=noncanonical), self.assertRaises(ValueError):
                    compare(expected, order, [], metadata)

    def test_permutations_reject_relevant_unknown_and_pattern_rules(self):
        expected, observed = run7_closure_observations()
        compare = NAMESPACE["compare_module_closure"]
        bad_soft = (
            b"softdep qcom_q6v5_pas pre: other\n", b"softdep qcom-q6v5-pas pre: other\n",
            b"softdep qcom* pre: other\n", b"softdep unrelated pre: qcom[ab]\n",
            b"softdep unrelated pre:\n", b"softdep unrelated other\n",
            b"softdep unrelated post: a pre: b\n", b"softdep unrelated pre: a pre: b\n",
            b"softdep unrelated pre: post: b\n", b"install unrelated /bin/true\n",
            b"softdep unrelated pre: a\\\n b\n", b"# invalid\\\n",
            b"# invalid\x00\n", b"# nonascii\xff\n",
        )
        for scope in ("source", "emitted"):
            for rule in bad_soft:
                metadata = dependency_metadata_fixture({(scope, "modules.softdep"): rule})
                with self.subTest(scope=scope, rule=rule), self.assertRaises(ValueError):
                    compare(expected, observed, [], metadata)
            for rule in (b"weakdep qcom_q6v5_pas other\n", b"weakdep qcom-q6v5-pas other\n",
                         b"weakdep unrelated\n", b"weakdep unrelated *\n", b"unknown foo bar\n"):
                metadata = dependency_metadata_fixture({(scope, "modules.weakdep"): rule})
                with self.subTest(scope=scope, rule=rule), self.assertRaises(ValueError):
                    compare(expected, observed, [], metadata)
        exact = [m["archive_path"] for m in expected["modules"]]
        expected["modules"][-1]["softdep"] = ["pre: smem"]
        self.assertEqual(compare(expected, exact, [], dependency_metadata_fixture()), "exact-order")

    def test_dependency_metadata_read_is_bounded_receipted_and_nofollow(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            records = []
            for name in ("modules.softdep", "modules.weakdep"):
                relative = f"lib/modules/{NAMESPACE['KVER']}/{name}"
                path = root / relative
                path.parent.mkdir(parents=True, exist_ok=True)
                path.write_bytes(b"# fixture\n")
                records.append({"path": relative, **NAMESPACE["digest"](path)})
            read = NAMESPACE["read_dependency_metadata"]
            items = read(root, records)
            self.assertEqual([base64.b64decode(item["base64"]) for item in items],
                             [b"# fixture\n", b"# fixture\n"])
            for bad in (records[:1], records + records[:1],
                        [{**records[0], "sha256": "0" * 64}, records[1]]):
                with self.assertRaises(ValueError):
                    read(root, bad)
            path = root / records[0]["path"]
            with path.open("wb") as stream:
                stream.truncate((256 << 10) + 1)
            with self.assertRaises(ValueError):
                read(root, records)
            path.unlink()
            path.symlink_to(root / records[1]["path"])
            with self.assertRaises(OSError):
                read(root, records)

    def test_closure_evidence_bounds_and_symlink_rejection(self):
        expected, receipt, modules, raw = closure_observations()
        record = NAMESPACE["record_closure_comparison"]
        with tempfile.TemporaryDirectory() as directory:
            base = Path(directory)
            result, source = base / "verification", base / "source.txt"
            result.mkdir()
            source.write_text(raw)
            for text, observed in (("x" * ((4 << 20) + 1), modules), (raw, ["x" * (1 << 20)])):
                with self.subTest(size=len(text)), self.assertRaises(ValueError):
                    record(result, expected, receipt, [], text, observed, [], source)
            with source.open("wb") as stream:
                stream.truncate((4 << 20) + 1)
            with self.assertRaisesRegex(ValueError, "excessive source"):
                record(result, expected, receipt, [], raw, modules, [], source)
            source.write_text(raw)
            self.assertEqual(list(result.iterdir()), [])
            alias = base / "alias"
            alias.symlink_to(source)
            with self.assertRaises(OSError):
                record(result, expected, receipt, [], raw, modules, [], alias)
            alias.unlink()
            alias.symlink_to(result, target_is_directory=True)
            with self.assertRaises(OSError):
                record(alias, expected, receipt, [], raw, modules, [], source)
            (result / "source-modprobe.txt").symlink_to(source)
            with self.assertRaises(OSError):
                record(result, expected, receipt, [], raw, modules, [], source)
            self.assertEqual(source.read_text(), raw)

    def test_workflow_collects_only_allowlisted_verification_files_on_failure(self):
        workflow = (DIRECTORY.parents[1] / ".github/workflows/x13s-initrd-fixture.yml").read_text()
        script = textwrap.dedent(workflow.split("python3 - <<'PY'\n", 1)[1].split("\n          PY", 1)[0])
        self.assertIn("- name: Collect bounded evidence\n        if: always()", workflow)
        with tempfile.TemporaryDirectory() as directory:
            base = Path(directory)
            source = base / "x13s-initrd"
            result = source / "verification"
            result.mkdir(parents=True)
            (source / "evidence").mkdir()
            names = ("closure-comparison.json", "source-modprobe.txt", "emitted-modprobe.txt", "verified.json")
            for name in names + ("arbitrary.txt",):
                (result / name).write_text(name)
            previous = Path.cwd()
            try:
                os.chdir(base)
                with mock.patch.dict(os.environ, {"RUNNER_TEMP": directory}):
                    exec(compile(script, "workflow-evidence-collector", "exec"), {})
                    copied = base / "fixture-receipts/verification"
                    self.assertEqual({p.name for p in copied.iterdir()}, set(names))
                    outside = base / "outside"
                    outside.mkdir()
                    for name in ("", "evidence", "verification"):
                        destination = base / "fixture-receipts" / name
                        saved = base / ("saved-target-" + (name or "root"))
                        existed = destination.exists()
                        if existed:
                            destination.rename(saved)
                        destination.symlink_to(outside, target_is_directory=True)
                        with self.subTest(target=name), self.assertRaisesRegex(SystemExit, "symlink"):
                            exec(compile(script, "workflow-evidence-collector", "exec"), {})
                        destination.unlink()
                        if existed:
                            saved.rename(destination)
                    escaped = outside / "sentinel"
                    escaped.write_text("unchanged")
                    destination = copied / "closure-comparison.json"
                    destination.unlink()
                    destination.symlink_to(escaped)
                    with self.assertRaisesRegex(SystemExit, "symlink"):
                        exec(compile(script, "workflow-evidence-collector", "exec"), {})
                    self.assertEqual(escaped.read_text(), "unchanged")
                    destination.unlink()
                    result.rename(source / "saved")
                    result.symlink_to(source / "saved", target_is_directory=True)
                    with self.assertRaisesRegex(SystemExit, "symlink"):
                        exec(compile(script, "workflow-evidence-collector", "exec"), {})
            finally:
                os.chdir(previous)

    def test_vermagic_preserves_significant_trailing_space(self):
        value = NAMESPACE["KVER"] + " SMP preempt mod_unload aarch64 \n"
        self.assertEqual(NAMESPACE["module_field"]("vermagic", value), value[:-1])
        self.assertTrue(NAMESPACE["module_field"]("vermagic", value).endswith(" "))
        self.assertEqual(NAMESPACE["module_field"]("name", "qcom_q6v5_pas\n"), "qcom_q6v5_pas")
        self.assertEqual(NAMESPACE["module_field"]("weakdep", ""), [])

    def test_receipt_paths_are_canonical_and_relative(self):
        for path in ("", "/etc/test", "../test", "x/../../z", "x/./y", "x//y", "x/"):
            with self.subTest(path=path), self.assertRaises(ValueError):
                NAMESPACE["safe_relative"](path)
        self.assertEqual(str(NAMESPACE["safe_relative"]("lib/modules/example")),
                         "lib/modules/example")

    def test_native_arch_rejected_before_runtime_lookup(self):
        with mock.patch("os.uname") as uname, \
                mock.patch("shutil.which", side_effect=AssertionError("runtime lookup")):
            uname.return_value.machine = "x86_64"
            with self.assertRaisesRegex(ValueError, "native ARM64"):
                NAMESPACE["host"]("prepare", "/tmp/not-created")

    def test_remote_engine_rejected_without_container_work(self):
        with mock.patch("os.uname") as uname, \
                mock.patch("shutil.which", return_value="/usr/bin/docker"), \
                mock.patch.dict(os.environ, {"DOCKER_HOST": "tcp://example.com:2376",
                                            "BUILDX_BUILDER": "", "BUILDKIT_HOST": ""}), \
                mock.patch.dict(NAMESPACE, {"run": mock.Mock(
                    side_effect=AssertionError("container work attempted"))}):
            uname.return_value.machine = "aarch64"
            with self.assertRaisesRegex(ValueError, "local Unix-socket"):
                NAMESPACE["host"]("prepare", "/tmp/not-created")

    def test_free_space_preflight_before_build_or_output_creation(self):
        with tempfile.TemporaryDirectory() as directory, \
                mock.patch("os.uname") as uname, \
                mock.patch("shutil.which", return_value="/usr/bin/docker"), \
                mock.patch("shutil.disk_usage") as usage, \
                mock.patch.dict(os.environ, {"DOCKER_HOST": "unix:///var/run/docker.sock",
                                            "BUILDX_BUILDER": "", "BUILDKIT_HOST": ""}), \
                mock.patch.dict(NAMESPACE, {"run": mock.Mock(
                    side_effect=AssertionError("container work attempted"))}):
            uname.return_value.machine = "aarch64"
            usage.return_value.free = (6 << 30) - 1
            output = Path(directory) / "new-output"
            with self.assertRaisesRegex(ValueError, "6 GiB"):
                NAMESPACE["host"]("prepare", str(output))
            self.assertFalse(output.exists())

    def test_json_receipts_reject_symlink_and_oversize(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "receipt.json"
            path.write_text('{"schema_version":1}')
            self.assertEqual(NAMESPACE["read_json"](path), {"schema_version": 1})
            link = Path(directory) / "link.json"
            link.symlink_to(path)
            with self.assertRaises((ValueError, OSError)):
                NAMESPACE["read_json"](link)
            with path.open("wb") as output:
                output.truncate((4 << 20) + 1)
            with self.assertRaises(ValueError):
                NAMESPACE["read_json"](path)

    def test_strict_json_and_expected_receipt_binding(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "expected.json"
            data = b'{"modules": [], "metadata": {"source": "verified"}}\n'
            path.write_bytes(data)
            receipt = {"expected_size": len(data), "expected_sha256": hashlib.sha256(data).hexdigest()}
            read = NAMESPACE["read_bound_expected"]
            self.assertEqual(read(path, receipt), json.loads(data))
            replacement = data.replace(b"verified", b"modified")
            path.write_bytes(replacement)  # Same size, different DAG metadata.
            with self.assertRaisesRegex(ValueError, "differs"):
                read(path, receipt)
            path.write_bytes(data)
            for changed in ({}, {**receipt, "expected_size": len(data) + 1},
                            {**receipt, "expected_sha256": "0" * 64}):
                with self.assertRaisesRegex(ValueError, "differs"):
                    read(path, changed)
            for invalid in (b'{"x":1,"x":2}', b'{"x":{"a":1,"a":2}}',
                            b'{"depends":[],"Depends":["other"]}', b'{"K":1,"\\u212a":2}',
                            b'{"x":1,"\\u0078":2}', b'{} {}', b'{} trailing',
                            b'{"x":NaN}', b'{"x":Infinity}', b'[]'):
                path.write_bytes(invalid)
                bound = {"expected_size": len(invalid),
                         "expected_sha256": hashlib.sha256(invalid).hexdigest()}
                with self.subTest(invalid=invalid), self.assertRaises(ValueError):
                    read(path, bound)
                with self.assertRaises(ValueError):
                    NAMESPACE["read_json"](path)
            path.unlink()
            path.symlink_to(Path(directory) / "missing")
            with self.assertRaises(OSError):
                read(path, receipt)

    def test_expected_json_canonical_keys_and_go_matching_budgets(self):
        decode = NAMESPACE["decode_expected"]
        env = {"DRACUT_ARCH": "aarch64", "DRACUT_NO_MKNOD": "1", "HOME": "/tmp",
               "LC_ALL": "C", "PATH": "/usr/bin", "SOURCE_DATE_EPOCH": "0"}
        self.assertEqual(decode(json.dumps({"build_environment": env}).encode()),
                         {"build_environment": env})
        for invalid in (
            b'{"modules":[{"Softdep":["pre: other"]}]}',
            b'{"modules":[{"weakdep":[],"Weakdep":["other"]}]}',
            b'{"build_environment":{"OTHER":"x"}}',
            b'{"nested":{"PATH":"x"}}', b'{"build_environment":[{"PATH":"x"}]}',
            b'{"\\u212aernel_release":"x"}', b'{"bad-key":1}',
            b'{"' + b"a" * 129 + b'":1}', b'{"x":"\xff"}',
            '{"x":1}'.encode("utf-16"),
        ):
            with self.subTest(invalid=invalid), self.assertRaises(ValueError):
                decode(invalid)
        for depth, valid in ((63, True), (64, False)):
            raw = b'{"a":' + b"[" * depth + b"0" + b"]" * depth + b"}"
            if valid:
                decode(raw)
            else:
                with self.assertRaisesRegex(ValueError, "nesting"):
                    decode(raw)
        for count, valid in ((99994, True), (99995, False)):
            raw = b'{"a":[' + b",".join([b"0"] * count) + b"]}"
            if valid:
                decode(raw)
            else:
                with self.assertRaisesRegex(ValueError, "token"):
                    decode(raw)

    def test_runtime_security_and_strict_emission_options(self):
        self.assertNotIn("--privileged", PYTHON)
        self.assertNotIn("--cap-add", PYTHON)
        for required in ("--network=none", "--read-only", "--cap-drop=ALL",
                         "--tmpfs=/sys:ro,nosuid,noexec,size=1m", "DRACUT_NO_MKNOD",
                         "--setopt=tsflags=noscripts", "--nostrip", "--no-compress",
                         "--no-early-microcode", "--nohardlink", "--no-uefi"):
            self.assertIn(required, PYTHON)

    def test_fedora108_archive_owner_expression_uses_controlled_config(self):
        self.assertEqual(NAMESPACE["STRICT_CONFIG"].splitlines().count("cpio_owner=0:0"), 1)
        # EUID is readonly in Bash. Substitute only its variable name to cover
        # both observed source branches without privilege or an actual archive.
        owner_default = FEDORA108_OWNER_DEFAULT.replace("$EUID", "$fixture_euid")
        for config, euid, owner in (
            (NAMESPACE["STRICT_CONFIG"], 0, "0:0"),
            (NAMESPACE["STRICT_CONFIG"], 1000, "0:0"),
            ("", 0, None),  # Root otherwise retains source uid/gid.
            ("", 1000, "0:0"),
            ("cpio_owner=123:456\n", 0, "123:456"),
            ("cpio_owner=123:456\n", 1000, "0:0"),
        ):
            script = ('set -u\nfixture_euid=$1\nCPIO_REPRODUCIBLE=1\n'
                      + config + "\n" + owner_default
                      + '\ncpio() { printf "%s\\n" "$@"; }\n' + FEDORA108_CPIO_COMMAND)
            with self.subTest(euid=euid, owner=owner):
                _, output = NAMESPACE["run"](["bash", "-c", script, "fixture", str(euid)],
                                             maximum=1024)
                expected = ["-o", "--reproducible", "--null"]
                if owner:
                    expected += ["-R", owner]
                self.assertEqual(output.splitlines(), expected + ["-H", "newc", "--quiet"])

    def test_command_stdout_bound_and_success(self):
        run = NAMESPACE["run"]
        self.assertEqual(run(["/bin/sh", "-c", "printf hello"], maximum=5), (0, "hello"))
        with self.assertRaisesRegex(ValueError, "output limit"):
            run(["/bin/sh", "-c", "printf abcde"], maximum=4)
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "failed-build.log"
            status, text = NAMESPACE["log_run"](
                ["/bin/sh", "-c", "printf 'failure diagnostic'; exit 17"],
                path, check=False)
            self.assertEqual(status, 17)
            self.assertEqual(text, "failure diagnostic")
            self.assertEqual(path.read_text(), text)

    def test_command_deadline(self):
        start = time.monotonic()
        with self.assertRaisesRegex(ValueError, "timeout"):
            NAMESPACE["run"](["/bin/sh", "-c", "sleep 3"], timeout=0.05)
        self.assertLess(time.monotonic() - start, 2)


if __name__ == "__main__":
    unittest.main(verbosity=2)
