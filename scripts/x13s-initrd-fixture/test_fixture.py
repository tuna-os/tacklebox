#!/usr/bin/env python3
"""Offline regressions for the fixture producer. Never downloads or runs containers."""
import ast
import hashlib
import io
import json
import lzma
import os
from pathlib import Path
import tempfile
import time
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


def keyring_observations(db, keys=""):
    return {
        "version": (0, "RPM version 6.0.2\n"),
        "keyring-macros": (0, f"_dbpath={db}\n_keyring=rpmdb\n_keyringpath={db}/pubkeys/\n"
                          "_pkgverify_level=digest\n_pkgverify_flags=0x20100\n"),
        "key-list": (0, keys),
    }


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
            with self.assertRaises(ValueError):
                NAMESPACE["read_json"](link)
            with path.open("wb") as output:
                output.truncate((4 << 20) + 1)
            with self.assertRaises(ValueError):
                NAMESPACE["read_json"](path)

    def test_runtime_security_and_strict_emission_options(self):
        self.assertNotIn("--privileged", PYTHON)
        self.assertNotIn("--cap-add", PYTHON)
        for required in ("--network=none", "--read-only", "--cap-drop=ALL",
                         "--tmpfs=/sys:ro,nosuid,noexec,size=1m", "DRACUT_NO_MKNOD",
                         "--setopt=tsflags=noscripts", "--nostrip", "--no-compress",
                         "--no-early-microcode", "--nohardlink", "--no-uefi"):
            self.assertIn(required, PYTHON)

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
