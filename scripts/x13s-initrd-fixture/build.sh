#!/usr/bin/env bash
# Native ARM64 fixture producer; prepare -> Go TestFedoraEmittedInitrdFixture -> verify.
# No privileged containers, host device/sysfs/module mounts, or production wiring.
# Internal modes run only in the disposable image. Runtime failures need review;
# never repair them by adding capabilities, disabling signature checks, or raising caps.
set -euo pipefail
TBOX_FIXTURE_SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
export TBOX_FIXTURE_SCRIPT_DIR
exec python3 - "$@" <<'PY'
import base64
import hashlib
import json
import lzma
import os
from pathlib import Path
import re
import resource
import selectors
import signal
import shutil
import stat
import subprocess
import sys
import tempfile
import time
import urllib.request
import urllib.parse
import xml.etree.ElementTree as ET

HERE = Path("/fixture") if Path("/fixture/pins.json").is_file() else Path(
    os.environ.get("TBOX_FIXTURE_SCRIPT_DIR", "")
)
# The shell supplies no working-directory-dependent source paths on the host.
if not (HERE / "pins.json").is_file():
    raise SystemExit("Run with TBOX_FIXTURE_SCRIPT_DIR pointing to this script's directory")
PINS = json.loads((HERE / "pins.json").read_text())
KVER = PINS["kernel_release"]
LIMIT = PINS["max_initrd_bytes"]
ENV = {"PATH": "/usr/sbin:/usr/bin:/sbin:/bin", "LC_ALL": "C", "HOME": "/tmp"}
MODULE_DIRS = ("70tbox-x13s-adsp", "90tbox-live", "95tbox-root")
METADATA = ("modules.dep", "modules.dep.bin", "modules.alias", "modules.alias.bin",
            "modules.softdep", "modules.builtin", "modules.builtin.modinfo",
            "modules.builtin.bin", "modules.builtin.alias.bin", "modules.order",
            "modules.symbols", "modules.symbols.bin", "modules.weakdep")
STRICT_CONFIG = """DRACUT_ARCH=aarch64
DRACUT_COMPRESS_CAT=cat
compress=cat
early_microcode=no
acpi_override=no
enhanced_cpio=no
cpio_owner=0:0
do_hardlink=no
do_strip=no
uefi=no
hostonly=no
hostonly_cmdline=no
"""


def fail(message):
    raise ValueError(message)


def digest(path, maximum=LIMIT):
    path = Path(path)
    if not path.is_file() or path.stat().st_size > maximum:
        fail(f"not a bounded regular file: {path}")
    h = hashlib.sha256()
    count = 0
    with path.open("rb") as f:
        for block in iter(lambda: f.read(1024 * 1024), b""):
            count += len(block)
            if count > maximum:
                fail(f"file grew beyond limit: {path}")
            h.update(block)
    return {"size": count, "sha256": h.hexdigest()}


def handoff_initrd(out):
    # Dracut creates root-owned 0600 output. Hand only that open artifact to the
    # caller owning the private bind-mounted /out, without changing its mode.
    out = Path(out)
    if not out.is_absolute() or ".." in out.parts:
        fail("initrd handoff requires an absolute contained output directory")
    directory = os.open(out, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW)
    try:
        owner = os.fstat(directory)
        if not stat.S_ISDIR(owner.st_mode) or stat.S_IMODE(owner.st_mode) != 0o700:
            fail("initrd handoff output directory must be private mode 0700")
        fd = os.open("initrd.img", os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK,
                     dir_fd=directory)
        try:
            before = os.fstat(fd)
            if (not stat.S_ISREG(before.st_mode) or before.st_nlink != 1
                    or stat.S_IMODE(before.st_mode) != 0o600
                    or before.st_uid != os.geteuid() or not 6 <= before.st_size <= LIMIT):
                fail("initrd handoff requires a bounded owned single-link 0600 regular file")

            def identity(value):
                return (value.st_dev, value.st_ino, value.st_mode, value.st_nlink,
                        value.st_size, value.st_mtime_ns)

            def contained():
                current = os.stat(out, follow_symlinks=False)
                if ((current.st_dev, current.st_ino, current.st_mode, current.st_uid, current.st_gid)
                        != (owner.st_dev, owner.st_ino, owner.st_mode, owner.st_uid, owner.st_gid)):
                    fail("initrd handoff output directory changed")
                if identity(os.stat("initrd.img", dir_fd=directory, follow_symlinks=False)) != identity(before):
                    fail("initrd handoff artifact path changed")

            header = os.read(fd, 6)
            if header != b"070701":
                fail("dracut did not produce raw newc; full audit remains the Go test")
            h, count = hashlib.sha256(header), len(header)
            while block := os.read(fd, min(1024 * 1024, LIMIT + 1 - count)):
                count += len(block)
                if count > LIMIT:
                    fail("initrd handoff artifact grew beyond limit")
                h.update(block)
            after = os.fstat(fd)
            if (identity(after) != identity(before) or count != before.st_size
                    or (after.st_uid, after.st_gid, after.st_ctime_ns)
                    != (before.st_uid, before.st_gid, before.st_ctime_ns)):
                fail("initrd handoff artifact changed during inspection")
            contained()
            os.fchown(fd, owner.st_uid, owner.st_gid)
            after = os.fstat(fd)
            if (identity(after) != identity(before)
                    or (after.st_uid, after.st_gid) != (owner.st_uid, owner.st_gid)):
                fail("initrd handoff ownership or file identity mismatch")
            contained()
            return {"size": count, "sha256": h.hexdigest()}
        finally:
            os.close(fd)
    finally:
        os.close(directory)


def write_json(path, value):
    path = Path(path)
    path.write_text(json.dumps(value, indent=2, sort_keys=True) + "\n")


def read_json_bytes(path):
    fd = os.open(path, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK)
    with os.fdopen(fd, "rb") as source:
        before = os.fstat(source.fileno())
        if not stat.S_ISREG(before.st_mode) or before.st_size > 4 << 20:
            fail(f"invalid or excessive JSON input: {path}")
        data = source.read((4 << 20) + 1)
        after = os.fstat(source.fileno())
    if len(data) > 4 << 20:
        fail("JSON input grew beyond limit")
    if ((before.st_size, before.st_mtime_ns, before.st_ctime_ns)
            != (after.st_size, after.st_mtime_ns, after.st_ctime_ns) or len(data) != before.st_size):
        fail("JSON input changed during read")
    return data


def decode_json(data):
    if isinstance(data, bytes):
        data = data.decode("utf-8")
    def unique(pairs):
        value, folded = {}, set()
        for key, item in pairs:
            if key.casefold() in folded:
                fail(f"duplicate JSON key: {key}")
            folded.add(key.casefold())
            value[key] = item
        return value
    value = json.loads(data, object_pairs_hook=unique,
                       parse_constant=lambda token: fail(f"non-JSON numeric constant: {token}"))
    if not isinstance(value, dict):
        fail("JSON receipt must be an object")
    return value


def read_json(path):
    return decode_json(read_json_bytes(path))


def decode_expected(data):
    value = decode_json(data)
    budget = 99999  # Go's 100000-token budget includes the final EOF read.
    environment_keys = {"DRACUT_ARCH", "DRACUT_NO_MKNOD", "HOME", "LC_ALL", "PATH", "SOURCE_DATE_EPOCH"}

    def visit(item, depth=0, parent=""):
        nonlocal budget
        if depth > 64:
            fail("expected JSON nesting budget exceeded")
        budget -= 1
        if isinstance(item, dict):
            for key, child in item.items():
                budget -= 1
                if not (parent == "/build_environment" and key in environment_keys):
                    if not re.fullmatch(r"[a-z][a-z0-9_]{0,127}", key):
                        fail(f"noncanonical expected JSON key: {key}")
                visit(child, depth + 1, parent + "/" + key)
            budget -= 1
        elif isinstance(item, list):
            for child in item:
                visit(child, depth + 1, parent + "/[]")
            budget -= 1
        if budget < 0:
            fail("expected JSON token budget exceeded")
    visit(value)
    return value


def read_bound_expected(path, receipt):
    # Binding is meaningful only after the Go byte/ELF audit that emits this
    # receipt. It does not independently verify producer metadata.
    data = read_json_bytes(path)
    if (type(receipt.get("expected_size")) is not int
            or receipt["expected_size"] != len(data)
            or receipt.get("expected_sha256") != hashlib.sha256(data).hexdigest()):
        fail("expected.json differs from the Go-audited receipt")
    return decode_expected(data)


def record_modprobe_config(out, configs, maximum=1 << 20):
    # Public image-package config only, copied by internal_emit; no host config
    # is mounted. Retain exact text so a strict consumer rejection is actionable.
    evidence = []
    for item in configs:
        relative = safe_relative(item["source_path"])
        if relative.parts[0] != "source":
            fail("modprobe evidence must come from copied source")
        path = Path(out) / relative
        if path.is_symlink() or not path.is_file():
            fail("modprobe evidence source is not a regular copied file")
        with path.open("rb") as source:
            data = source.read(maximum + 1)
        if len(data) > maximum:
            fail("modprobe config evidence exceeds bound")
        if (len(data), hashlib.sha256(data).hexdigest()) != (item["size"], item["sha256"]):
            fail("modprobe config evidence differs from observed source")
        evidence.append({"archive_path": item["archive_path"], "size": item["size"],
                         "sha256": item["sha256"], "text": data.decode("utf-8")})
        encoded = json.dumps(evidence, indent=2, sort_keys=True) + "\n"
        if len(encoded.encode("utf-8")) > maximum:
            fail("aggregate modprobe config evidence exceeds bound")
    write_json(Path(out) / "evidence/source-modprobe-config.json", evidence)


def run(args, timeout=120, maximum=4 << 20, check=True, env=None, file_maximum=None):
    # Bound diagnostics while streaming, independently of files written by the
    # command. In particular a host Podman must not inherit a 4 MiB image-file cap.
    def limits():
        if file_maximum:
            resource.setrlimit(resource.RLIMIT_FSIZE, (file_maximum, file_maximum))
        resource.setrlimit(resource.RLIMIT_CORE, (0, 0))
    chunks, size = [], 0
    deadline = time.monotonic() + timeout
    with subprocess.Popen([str(x) for x in args], stdout=subprocess.PIPE,
                          stderr=subprocess.STDOUT, env=env or ENV,
                          preexec_fn=limits, start_new_session=True) as process:
        try:
            with selectors.DefaultSelector() as poll:
                poll.register(process.stdout, selectors.EVENT_READ)
                while poll.get_map():
                    remaining = deadline - time.monotonic()
                    if remaining <= 0:
                        fail(f"command timeout: {args[0]}")
                    for key, _ in poll.select(min(remaining, 1)):
                        block = os.read(key.fd, min(65536, maximum + 1 - size))
                        if not block:
                            poll.unregister(key.fd)
                            continue
                        size += len(block)
                        if size > maximum:
                            fail(f"command exceeded output limit: {args[0]}")
                        chunks.append(block)
            process.wait(timeout=max(0.01, deadline - time.monotonic()))
        except BaseException:
            os.killpg(process.pid, signal.SIGTERM)
            try:
                process.wait(timeout=5)
            except subprocess.TimeoutExpired:
                os.killpg(process.pid, signal.SIGKILL)
                process.wait()
            raise
    text = b"".join(chunks).decode("utf-8", errors="strict")
    if check and process.returncode:
        fail(f"command failed ({process.returncode}): {args}\n{text[-16384:]}")
    return process.returncode, text


def log_run(args, path, **kwargs):
    code, text = run(args, **kwargs)
    Path(path).write_text(text)
    return code, text


def download(pin, destination):
    # Only the reviewed official HTTPS source; no fallback to a different RPM.
    url = pin["url"]
    if not url.startswith("https://dl.fedoraproject.org/pub/fedora/linux/"):
        fail("unreviewed RPM source")
    deadline = time.monotonic() + 600
    class HTTPSRedirect(urllib.request.HTTPRedirectHandler):
        def redirect_request(self, req, fp, code, msg, headers, newurl):
            if urllib.parse.urlsplit(newurl).scheme != "https":
                fail("non-HTTPS redirect")
            return super().redirect_request(req, fp, code, msg, headers, newurl)
    opener = urllib.request.build_opener(HTTPSRedirect())
    with opener.open(url, timeout=30) as response:
        if not response.url.startswith("https://"):
            fail("non-HTTPS redirect")
        if response.headers.get("Content-Length") and int(response.headers["Content-Length"]) != pin["size"]:
            fail("RPM content length differs from pin")
        with destination.open("xb") as out:
            count = 0
            while True:
                if time.monotonic() > deadline:
                    fail("RPM download deadline exceeded")
                chunk = response.read(min(1024 * 1024, pin["size"] + 1 - count))
                if not chunk:
                    break
                count += len(chunk)
                if count > pin["size"]:
                    fail("RPM exceeds pinned size")
                out.write(chunk)
    if digest(destination, pin["size"]) != {"size": pin["size"], "sha256": pin["sha256"]}:
        fail(f"RPM checksum/size mismatch: {pin['name']}")


def signed_rpm(text, code):
    # rpmkeys exits zero for digest-only unsigned RPMs. Require an actual approved
    # RSA header-signature success. RPM4 grammar comes from the retained 3494
    # receipt; RPM6 grammar from native run 39377874335, job 118480012249.
    if code != 0 or "\0" in text or re.search(
            r"NOT OK|NOTFOUND|NOKEY|NOTTRUSTED|BAD|UNTRUSTED", text, re.IGNORECASE):
        return False
    signatures = [line for line in text.splitlines()
                  if re.search(r"\bsignature\b", line, re.IGNORECASE)]
    # Never ignore an extra unsupported, duplicate, or mixed-format signature.
    if len(signatures) != 1:
        return False
    for line in text.splitlines():
        if line == signatures[0] or not line or line.endswith(".rpm:"):
            continue
        if not re.fullmatch(
                r"[ \t]*(?:(?:Header|Payload|Legacy) )?"
                r"(?:SHA1|SHA256|SHA512|SHA3-256|SHA3-512|MD5)(?: ALT)? digest: OK[ \t]*",
                line):
            return False
    modern = re.fullmatch(
        r"[ \t]*Header OpenPGP V4 RSA/SHA256 signature, key fingerprint: "
        r"([0-9a-fA-F]{40}): OK[ \t]*", signatures[0])
    if modern:
        return modern[1].upper() == PINS["fedora44_fingerprint"]
    legacy = re.fullmatch(
        r"[ \t]*Header V[34] RSA/SHA256 Signature, key ID "
        r"([0-9a-fA-F]{40}|[0-9a-fA-F]{16}|[0-9a-fA-F]{8}): OK[ \t]*", signatures[0])
    return bool(legacy) and PINS["fedora44_fingerprint"].endswith(legacy[1].upper())


def validate_rpm_keyring(db, phase, observations):
    # These settings and the full-fingerprint listing were observed on the
    # actual Fedora6.0.2 builder. Unknown native profiles require review.
    if phase not in ("before-import", "after-import"):
        fail("unknown RPM keyring validation phase")
    if any(observations[name][0] != 0 for name in ("version", "keyring-macros", "key-list")):
        fail("RPM keyring inspection failed")
    if observations["version"][1].splitlines() != ["RPM version 6.0.2"]:
        fail("unreviewed native RPM version")
    required_macros = {
        "_dbpath": str(db), "_keyring": "rpmdb", "_keyringpath": str(db) + "/pubkeys/",
        "_pkgverify_level": "digest", "_pkgverify_flags": "0x20100",
    }
    lines = observations["keyring-macros"][1].splitlines()
    if len(lines) != len(required_macros) or set(lines) != {
            key + "=" + value for key, value in required_macros.items()}:
        fail("RPM keyring or verification settings differ from isolated reviewed profile")
    keys = observations["key-list"][1].splitlines()
    if phase == "before-import":
        if keys:
            fail("RPM verification keyring is not initially empty")
    else:
        approved = (PINS["fedora44_fingerprint"].lower()
                    + " Fedora (44) <fedora-44-primary@fedoraproject.org> public key")
        if keys != [approved]:
            fail("RPM verification keyring is not exactly the approved Fedora44 key")


def rpm_keyring_diagnostics(db, evidence, phase):
    # Read only these public-key/verification settings, not --showrc or the
    # environment. RPM6 can use a filesystem keyring independent of --dbpath.
    queries = [
        ("version", ["rpm", "--version"]),
        ("keyring-macros", ["rpm", "--dbpath", db, "--eval",
            "_dbpath=%{_dbpath}\n_keyring=%{_keyring}\n_keyringpath=%{_keyringpath}\n"
            "_pkgverify_level=%{_pkgverify_level}\n_pkgverify_flags=%{_pkgverify_flags}"]),
        ("key-list", ["rpmkeys", "--dbpath", db, "--list"]),
    ]
    context, observations = [], {}
    for name, command in queries:
        code, text = log_run(command, evidence / f"rpm-{phase}-{name}.txt",
                             maximum=16384, check=False)
        observations[name] = (code, text)
        # Keep failure context small enough to survive Docker's bounded build log.
        tail = text.encode("utf-8")[-1024:].decode("utf-8", errors="replace")
        context.append(f"{phase} {name} exit={code}:\n{tail}")
    context = "\n".join(context)
    try:
        validate_rpm_keyring(db, phase, observations)
    except ValueError as error:
        fail(f"{error}\nBEGIN RPM DIAGNOSTICS\n{context}\nEND RPM DIAGNOSTICS")
    return context


def signature_failure(name, code, text, context):
    # The complete bounded receipt is also saved in the build image. Print its
    # tail because a failed image cannot be used to retrieve that evidence.
    tail = text.encode("utf-8")[-8192:].decode("utf-8", errors="replace")
    details = context.encode("utf-8")[-4096:].decode("utf-8", errors="replace")
    fail(f"approved signature missing: {name}; rpmkeys exit={code}\n"
         f"approved fingerprint={PINS['fedora44_fingerprint']}\n"
         f"BEGIN RPM DIAGNOSTICS\n{details}\n"
         f"BEGIN RPMKEYS RECEIPT (last 8192 bytes)\n{tail}\nEND RPM DIAGNOSTICS")


def rpm_identity(path):
    _, text = run(["rpm", "-qp", "--qf",
                  "%{NAME}\t%{EPOCHNUM}\t%{VERSION}\t%{RELEASE}\t%{ARCH}\t%{SOURCERPM}\t%{BUILDTIME}\n", path])
    fields = text.strip().split("\t")
    if len(fields) != 7:
        fail("unexpected RPM identity")
    return dict(zip(("name", "epoch", "version", "release", "arch", "source_rpm", "build_time"), fields))


def capture_repodata(out, phase):
    records = []
    # These are the actual libdnf caches used by the install, not a fresh network
    # read that may race the transaction. Preserve each repomd and its data identities.
    for base in (Path("/var/cache/libdnf5"), Path("/var/cache/dnf")):
        if not base.exists():
            continue
        for src in sorted(base.rglob("repomd.xml")):
            if src.is_symlink():
                fail("unexpected repomd symlink")
            d = digest(src, 4 << 20)
            dst = out / f"{phase}-repomd-{len(records)}.xml"
            shutil.copyfile(src, dst)
            tree = ET.parse(dst).getroot()
            ns = {"r": "http://linux.duke.edu/metadata/repo"}
            data = []
            for node in tree.findall("r:data", ns):
                checksum = node.find("r:checksum", ns)
                location = node.find("r:location", ns)
                if checksum is None or location is None:
                    fail("incomplete repomd identity")
                data.append({"type": node.get("type"), "checksum_type": checksum.get("type"),
                             "checksum": checksum.text, "location": location.get("href")})
            records.append({"phase": phase, "cache_path": str(src),
                            "evidence_path": "evidence/" + dst.name, **d, "data": data})
    if not records:
        fail("cannot capture the DNF transaction's actual repomd identities")
    return records


def internal_install():
    if HERE != Path("/fixture") or os.geteuid() != 0:
        fail("internal install requires the disposable fixture image")
    evidence = HERE / "evidence"
    repodata = capture_repodata(evidence, "bootstrap")
    home = HERE / "gnupg"
    home.mkdir(mode=0o700)
    key = Path(PINS["fedora44_key"])
    _, shown = run(["gpg", "--homedir", home, "--batch", "--with-colons", "--show-keys", key])
    fingerprints = [x.split(":")[9] for x in shown.splitlines() if x.startswith("fpr:")]
    if fingerprints != [PINS["fedora44_fingerprint"]]:
        fail(f"unexpected Fedora44 key fingerprints: {fingerprints}")
    (evidence / "fedora44-key.txt").write_text(shown)
    db = HERE / "rpmkeys"
    db.mkdir()
    before_keys = rpm_keyring_diagnostics(db, evidence, "before-import")
    run(["rpmkeys", "--dbpath", db, "--import", key])
    after_keys = rpm_keyring_diagnostics(db, evidence, "after-import")
    downloads = HERE / "rpms"
    downloads.mkdir()
    identities = []
    rpms = []
    for pin in PINS["packages"]:
        rpm = downloads / Path(pin["url"]).name
        download(pin, rpm)
        code, text = log_run(["rpmkeys", "--dbpath", db, "--checksig", "--verbose", rpm],
                             evidence / (pin["name"] + ".rpmkeys.txt"), check=False)
        if not signed_rpm(text, code):
            signature_failure(pin["name"], code, text, before_keys + "\n" + after_keys)
        actual = rpm_identity(rpm)
        for field in ("name", "epoch", "version", "release", "arch", "source_rpm"):
            if actual[field] != str(pin[field]):
                fail(f"RPM build identity mismatch: {pin['name']} {field}")
        identities.append({**actual, "rpm_size": pin["size"], "rpm_sha256": pin["sha256"]})
        rpms.append(rpm)
    kernel_build = next(p for p in identities if p["name"] == "kernel-core")
    if int(kernel_build["build_time"]) != PINS["source_date_epoch"]:
        fail("SOURCE_DATE_EPOCH differs from verified kernel RPM BUILDTIME")
    unsigned = HERE / "unsigned-control.rpm"
    shutil.copyfile(rpms[0], unsigned)
    run(["rpmsign", "--delsign", unsigned])
    code, text = log_run(["rpmkeys", "--dbpath", db, "--checksig", "--verbose", unsigned],
                         evidence / "unsigned-control.rpmkeys.txt", check=False)
    if signed_rpm(text, code):
        fail("unsigned RPM control incorrectly accepted")
    unsigned.unlink()
    # No kernel-install/dracut scriptlets run against the build host's kernel.
    # This is a package-content fixture, not a fully configured distribution image.
    log_run(["dnf", "-y", "--releasever=44", "--setopt=reposdir=/fixture/repos.d",
             "--setopt=keepcache=True", "--setopt=install_weak_deps=False",
             "--setopt=tsflags=noscripts", "--setopt=localpkg_gpgcheck=True",
             "--setopt=timeout=30", "--setopt=retries=2", "--setopt=max_parallel_downloads=2",
             "install", *rpms], evidence / "payload-dnf.log", timeout=600, file_maximum=1 << 30)
    repodata += capture_repodata(evidence, "payload")
    _, installed = log_run(["rpm", "-qa", "--qf",
                           "%{NAME}\t%{EPOCHNUM}\t%{VERSION}\t%{RELEASE}\t%{ARCH}\t%{SOURCERPM}\n"],
                          evidence / "installed.tsv")
    if "kernel-uki-dtbloader\t" in installed:
        fail("loader RPM must not be installed in this fixture")
    for pin in PINS["packages"]:
        line = "\t".join(str(pin[k]) for k in ("name", "epoch", "version", "release", "arch", "source_rpm"))
        if installed.splitlines().count(line) != 1:
            fail(f"installed pin is missing or ambiguous: {pin['name']}")
    config = Path("/usr/lib/modules") / KVER / "config"
    kernel = config.with_name("vmlinuz")
    if digest(config)["sha256"] != PINS["kernel_config_sha256"] or digest(kernel)["sha256"] != PINS["kernel_sha256"]:
        fail("installed config/kernel bytes differ from reviewed package observation")
    run(["depmod", "-a", KVER], timeout=120)
    controlled = HERE / "config"
    controlled.mkdir()
    distro = Path("/usr/lib/dracut/dracut.conf.d/01-dist.conf")
    if digest(distro)["sha256"] != PINS["dist_config_sha256"]:
        fail("unreviewed Fedora dracut vendor config")
    shutil.copyfile(distro, controlled / distro.name)
    (controlled / "99-tbox-x13s-strict.conf").write_text(STRICT_CONFIG)
    (HERE / "empty.d").mkdir()
    base_installed = (evidence / "base-installed.tsv").read_text().splitlines()
    write_json(HERE / "provenance.json", {
        "source_packages": identities, "repodata": repodata,
        "installed_packages": sorted(installed.splitlines()),
        "base_installed_packages": sorted(base_installed),
        "transaction_added_packages": sorted(set(installed.splitlines()) - set(base_installed)),
        "transaction_removed_packages": sorted(set(base_installed) - set(installed.splitlines())),
        "bootstrap_trust": "digest-pinned Fedora base and its preinstalled Fedora signing key",
        "transitive_dependencies_pinned": False,
        "scriptlets": "payload/dependency transaction uses tsflags=noscripts; explicit depmod follows",
        "unsigned_control_rejected": True})
    # Retain small provenance and source payloads, not duplicate RPM/cache archives.
    shutil.rmtree(downloads)
    for cache in (Path("/var/cache/libdnf5"), Path("/var/cache/dnf")):
        if cache.exists():
            shutil.rmtree(cache)


def copy_source(path, out, archive_path=None):
    path = Path(path)
    resolved = path.resolve(strict=True)
    if not str(resolved).startswith("/usr/") or not resolved.is_file():
        fail(f"source escaped installed /usr: {path}")
    d = digest(resolved, 256 << 20)
    relative = Path("source") / str(resolved).lstrip("/")
    dst = out / relative
    dst.parent.mkdir(parents=True, exist_ok=True)
    if not dst.exists():
        shutil.copyfile(resolved, dst)
    result = {"source_path": str(relative), **d}
    if archive_path is not None:
        result["archive_path"] = archive_path
    return result


def decoded_info(path, maximum):
    suffix = Path(path).suffix
    compression = {".xz": "xz", ".zst": "zstd"}.get(suffix, "none")
    h = hashlib.sha256()
    count = 0
    if compression == "none":
        return {"compression": compression, "decoded_size": digest(path, maximum)["size"],
                "decoded_sha256": digest(path, maximum)["sha256"]}
    if compression == "xz":
        decoder = lzma.LZMADecompressor(memlimit=256 << 20)
        with open(path, "rb") as f:
            while not decoder.eof:
                block = f.read(65536) if decoder.needs_input else b""
                if not block and decoder.needs_input:
                    fail("truncated xz source")
                data = decoder.decompress(block, max_length=min(65536, maximum + 1 - count))
                count += len(data)
                if count > maximum:
                    fail("decoded source exceeds limit")
                h.update(data)
            if decoder.unused_data or f.read(1):
                fail("trailing xz source bytes")
    else:
        # Trusted source package, bounded decoder output and window allocation.
        with tempfile.TemporaryFile() as f:
            def limits():
                resource.setrlimit(resource.RLIMIT_FSIZE, (maximum, maximum))
                resource.setrlimit(resource.RLIMIT_AS, (512 << 20, 512 << 20))
            subprocess.run(["zstd", "--decompress", "--stdout", "--memory=256MB", str(path)],
                           stdout=f, check=True, timeout=30, env=ENV, preexec_fn=limits)
            f.seek(0)
            for data in iter(lambda: f.read(65536), b""):
                count += len(data)
                h.update(data)
    return {"compression": compression, "decoded_size": count, "decoded_sha256": h.hexdigest()}


def closure(root="/", config=None):
    args = ["modprobe", "--show-depends", "--set-version", KVER, "--dirname", root]
    if config:
        args += ["--config", config]
    args += ["qcom_q6v5_pas"]
    _, text = run(args, timeout=30)
    modules, builtins = [], []
    for line in text.splitlines():
        parts = line.split()
        if len(parts) == 2 and parts[0] == "insmod":
            modules.append(parts[1])
        elif len(parts) == 2 and parts[0] == "builtin":
            builtins.append(parts[1])
        else:
            fail(f"unexpected modprobe action (including install directives/options): {line}")
    if not modules or len(modules) > 512 or len(set(modules)) != len(modules):
        fail("missing, excessive, or duplicate module closure")
    if not any(re.search(r"/qcom_q6v5_pas\.ko(?:\.xz|\.zst)?$", p) for p in modules):
        fail("PAS is not modular in observed closure")
    return args, text, modules, builtins


def module_field(field, value):
    if field == "vermagic":
        # VERMAGIC_STRING can end in a significant space. Remove only modinfo's
        # output record terminator, not bytes from the ELF metadata itself.
        return value.removesuffix("\n")
    if field == "name":
        return value.strip()
    if field == "depends":
        return [part for part in value.strip().split(",") if part]
    return value.splitlines()


def internal_stage():
    out = Path("/out")
    shutil.copytree(HERE / "config", out / "config")
    (out / "empty.d").mkdir()
    shutil.copytree(HERE / "evidence", out / "evidence", dirs_exist_ok=True)
    shutil.copyfile(HERE / "provenance.json", out / "provenance.json")


def internal_emit():
    out = Path("/out")
    config = Path("/usr/lib/modules") / KVER / "config"
    # Observe source inputs before dracut regenerates its own indexes.
    expected = json.loads((HERE / "provenance.json").read_text())
    expected.update({"schema_version": 1, "architecture": "aarch64", "kernel_release": KVER,
                     "pins": PINS, "kernel": copy_source(config.with_name("vmlinuz"), out),
                     "kernel_config": copy_source(config, out), "modules": [],
                     "module_metadata": [], "modprobe_config": [], "firmware": []})
    args, text, paths, builtins = closure()
    (out / "evidence/source-modprobe.txt").write_text(text)
    expected["builtins"] = builtins
    expected["modprobe"] = {"seed": "qcom_q6v5_pas", "args": args, "lines": text.splitlines(),
                            "stdout_sha256": hashlib.sha256(text.encode()).hexdigest()}
    for path in paths:
        resolved = Path(path).resolve(strict=True)
        if not str(resolved).startswith(f"/usr/lib/modules/{KVER}/"):
            fail("source module outside selected release")
        item = copy_source(resolved, out, str(resolved).lstrip("/"))
        item.update(decoded_info(resolved, 128 << 20))
        for field in ("name", "vermagic", "depends", "softdep", "weakdep", "firmware"):
            _, value = run(["modinfo", "-F", field, resolved])
            item[field] = module_field(field, value)
        if not item["vermagic"].startswith(KVER + " "):
            fail("source module vermagic mismatch")
        expected["modules"].append(item)
    for name in METADATA:
        source = config.with_name(name)
        if source.is_file():
            expected["module_metadata"].append({"name": name, **copy_source(source, out)})
    for directory in ("/etc/modprobe.d", "/run/modprobe.d", "/usr/local/lib/modprobe.d",
                      "/usr/lib/modprobe.d", "/lib/modprobe.d"):
        for source in sorted(Path(directory).glob("*.conf")):
            if source.is_file():
                # /etc files are source evidence too; copy_source intentionally
                # restricts binary package payloads to /usr, so handle config here.
                resolved = source.resolve(strict=True)
                rel = Path("source") / str(source).lstrip("/")
                dest = out / rel
                dest.parent.mkdir(parents=True, exist_ok=True)
                shutil.copyfile(resolved, dest)
                expected["modprobe_config"].append({"source_path": str(rel),
                    "archive_path": str(source).lstrip("/"), **digest(resolved, 1 << 20)})
    record_modprobe_config(out, expected["modprobe_config"])
    cfg = config.read_text()
    for name in ("qcadsp8280.mbn", "qccdsp8280.mbn", "qcslpi8280.mbn"):
        logical = "qcom/sc8280xp/LENOVO/21BX/" + name
        variants = [Path("/usr/lib/firmware") / (logical + suffix) for suffix in ("", ".xz", ".zst")]
        present = [p for p in variants if p.is_file()]
        if len(present) != 1:
            fail(f"missing or ambiguous firmware variant: {logical}")
        path = present[0]
        item = copy_source(path, out, str(path).lstrip("/"))
        item.update(decoded_info(path, 256 << 20))
        item["logical_name"] = logical
        if item["compression"] != "none":
            flag = {"xz": "CONFIG_FW_LOADER_COMPRESS_XZ=y", "zstd": "CONFIG_FW_LOADER_COMPRESS_ZSTD=y"}[item["compression"]]
            if "CONFIG_FW_LOADER_COMPRESS=y\n" not in cfg or flag + "\n" not in cfg:
                fail("firmware compression unsupported by selected kernel")
        expected["firmware"].append(item)
    command = ["dracut", "--force", "--no-hostonly", "--no-hostonly-cmdline",
               "--reproducible", "--no-compress", "--no-early-microcode", "--nohardlink",
               "--nostrip", "--no-uefi", "--conf", "/dev/null", "--confdir", "/fixture-empty",
               "--omit", "squash squash-lib squash-squashfs squash-erofs",
               "--add", "tbox-live tbox-root tbox-x13s-adsp", "--kver", KVER,
               "--tmpdir", "/tmp", "--logfile", "/out/evidence/dracut.log", "/out/initrd.img"]
    env = {**ENV, "DRACUT_ARCH": "aarch64", "DRACUT_NO_MKNOD": "1",
           "SOURCE_DATE_EPOCH": str(PINS["source_date_epoch"])}
    _, log = run(command, timeout=600, file_maximum=LIMIT, env=env)
    (out / "evidence/dracut-stdout.log").write_text(log)
    expected["initrd"] = {"path": "initrd.img", **handoff_initrd(out)}
    expected["dracut_command"] = command
    expected["build_environment"] = env
    expected["tools"] = {}
    for tool, argv in {"dracut": ["dracut", "--version"], "rpm": ["rpm", "--version"],
                       "kmod": ["modprobe", "--version"], "python": ["python3", "--version"],
                       "cpio": ["cpio", "--version"], "ldconfig": ["ldconfig", "--version"]}.items():
        expected["tools"][tool] = run(argv)[1].splitlines()[0]
    expected["process_security"] = [line for line in Path("/proc/self/status").read_text().splitlines()
                                    if line.startswith(("Cap", "NoNewPrivs:", "Seccomp:"))]
    expected["os_release"] = Path("/etc/os-release").read_text()
    expected["controlled_config"] = [{"name": p.name, **digest(p, 1 << 20)}
                                     for p in sorted(Path("/usr/lib/dracut/dracut.conf.d").glob("*"))]
    write_json(out / "expected.json", expected)


def safe_relative(value):
    p = Path(value)
    if not value or p.is_absolute() or any(x in ("", ".", "..") for x in value.split("/")):
        fail(f"unsafe relative path: {value}")
    return p


def read_dependency_metadata(root, records, source=False):
    evidence = []
    for name in ("modules.softdep", "modules.weakdep"):
        path = f"lib/modules/{KVER}/{name}"
        matches = [item for item in records
                   if (item.get("name") == name if source else item.get("path") == path)]
        if len(matches) != 1:
            fail(f"missing or ambiguous dependency metadata: {name}")
        item = matches[0]
        relative = safe_relative(item["source_path"] if source else path)
        if source and relative.parts[0] != "source":
            fail("dependency metadata is outside copied source")
        fd = os.open(Path(root) / relative, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK)
        with os.fdopen(fd, "rb") as stream:
            info = os.fstat(stream.fileno())
            if not stat.S_ISREG(info.st_mode) or info.st_size > 256 << 10:
                fail("dependency metadata is not a bounded regular file")
            data = stream.read((256 << 10) + 1)
        sha = hashlib.sha256(data).hexdigest()
        if len(data) > 256 << 10 or (len(data), sha) != (item["size"], item["sha256"]):
            fail("dependency metadata differs from validated receipt")
        evidence.append({"scope": "source" if source else "emitted", "name": name,
                         "path": str(relative), "size": len(data), "sha256": sha,
                         "base64": base64.b64encode(data).decode("ascii")})
    return evidence


def module_name(value):
    if not isinstance(value, str) or not re.fullmatch(r"[A-Za-z0-9_-]{1,128}", value):
        fail(f"unsupported literal module name: {value!r}")
    return value.replace("-", "_")


def reject_required_dependency_rules(metadata, required):
    if sorted((item["scope"], item["name"]) for item in metadata) != [
            ("emitted", "modules.softdep"), ("emitted", "modules.weakdep"),
            ("source", "modules.softdep"), ("source", "modules.weakdep")]:
        fail("source/emitted dependency metadata inventory is incomplete")
    for item in metadata:
        data = base64.b64decode(item["base64"], validate=True)
        if len(data) > 256 << 10 or (len(data), hashlib.sha256(data).hexdigest()) != (item["size"], item["sha256"]):
            fail("dependency rule evidence identity mismatch")
        if any(byte not in (9, 10) and not 32 <= byte <= 126 for byte in data) or b"\\" in data:
            fail("unsupported dependency rule encoding or continuation")
        kind = "softdep" if item["name"] == "modules.softdep" else "weakdep"
        for number, line in enumerate(data.decode("ascii").splitlines(), 1):
            line = line.strip()
            if not line or line.startswith("#"):
                continue
            fields = line.split()
            if len(fields) < 3 or fields[0] != kind:
                fail(f"malformed {item['scope']} {item['name']}:{number}")
            subject = module_name(fields[1])
            if kind == "weakdep":
                for value in fields[2:]:
                    module_name(value)
            else:
                labels, count = [], 0
                for value in fields[2:]:
                    if value in ("pre:", "post:"):
                        if labels and not count:
                            fail("empty softdep phase")
                        labels.append(value)
                        count = 0
                    else:
                        if not labels:
                            fail("softdep target lacks a phase")
                        module_name(value)
                        count += 1
                if labels not in (["pre:"], ["post:"], ["pre:", "post:"]) or not count:
                    fail("malformed or repeated softdep phases")
            # kmod uses fnmatch(subject, module_name); literal-only grammar above
            # avoids approximating wildcard/escape semantics. Targets of an
            # unrelated subject do not apply to this exact seed closure.
            if subject in required:
                fail(f"required module has unreviewed {kind} rule: {subject}")


def compare_module_closure(expected, observed, builtins, metadata):
    modules = expected["modules"]
    if not 1 <= len(modules) <= 512 or not isinstance(observed, list) or len(observed) > 512:
        fail("module closure exceeds bounded inventory")
    paths, names, graph = [], {}, {}
    for item in modules:
        for field in ("softdep", "weakdep"):
            if (field not in item or not isinstance(item[field], list) or len(item[field]) > 512
                    or any(not isinstance(value, str) or len(value) > 4096 for value in item[field])):
                fail(f"missing, noncanonical, or unbounded module {field} metadata")
        path = item["archive_path"]
        relative = safe_relative(path)
        match = re.fullmatch(r"(.+)\.ko(?:\.xz|\.zst)?", relative.name)
        name = module_name(item["name"])
        if (len(path) > 4096 or not path.startswith(f"usr/lib/modules/{KVER}/kernel/")
                or not match or module_name(match[1]) != name or name in names or path in paths):
            fail("ambiguous module name/path identity")
        paths.append(path)
        names[name] = path
        dependencies = item["depends"]
        if not isinstance(dependencies, list) or len(dependencies) > 512:
            fail("unbounded hard dependency list")
        graph[name] = [module_name(value) for value in dependencies]
        if len(set(graph[name])) != len(graph[name]):
            fail("duplicate hard dependency")
    if (len(observed) != len(paths) or len(set(observed)) != len(observed)
            or set(observed) != set(paths)):
        fail("emitted closure has missing, extra, or duplicate modules")
    if not isinstance(builtins, list) or len(builtins) > 512 or builtins != expected["builtins"]:
        fail("emitted builtin list differs from source")
    builtin_names = [module_name(value) for value in builtins]
    if len(set(builtin_names)) != len(builtin_names) or set(builtin_names) & names.keys():
        fail("ambiguous builtin/module identity")
    if expected["modprobe"]["seed"] != "qcom_q6v5_pas" or "qcom_q6v5_pas" not in names:
        fail("missing or incorrect modular PAS seed")
    lines = expected["modprobe"]["lines"]
    if (not isinstance(lines, list) or not 1 <= len(lines) <= 1024
            or any(not isinstance(line, str) or len(line) > 8192 for line in lines)):
        fail("unbounded raw source closure lines")
    raw = ("\n".join(lines) + "\n").encode("utf-8")
    if len(raw) > 4 << 20 or hashlib.sha256(raw).hexdigest() != expected["modprobe"]["stdout_sha256"]:
        fail("raw source closure lines differ from stdout identity")
    source_paths, source_builtins = [], []
    for line in lines:
        fields = line.split()
        if len(fields) != 2:
            fail("unsupported raw source closure action")
        if fields[0] == "insmod" and fields[1].startswith("/"):
            # kmod --dirname / emits //lib/...; normalize only leading slashes
            # and the known usr-merge alias, never traversal/interior aliases.
            path = fields[1].lstrip("/")
            safe_relative(path)
            if path.startswith("lib/modules/"):
                path = "usr/" + path
            source_paths.append(path)
        elif fields[0] == "builtin":
            module_name(fields[1])
            source_builtins.append(fields[1])
        else:
            fail("unsupported raw source closure action")
    if source_paths != paths or source_builtins != expected["builtins"]:
        fail("source inventory order differs from raw source closure")
    if sum(len(values) for values in graph.values()) > 16384:
        fail("hard dependency graph exceeds edge bound")
    for order in (paths, observed):
        positions = {path: index for index, path in enumerate(order)}
        for name, dependencies in graph.items():
            for dependency in dependencies:
                if dependency in builtin_names:
                    continue
                if dependency not in names or positions[names[dependency]] >= positions[names[name]]:
                    fail(f"missing, cyclic, or inverted hard dependency: {dependency} before {name}")
    if paths == observed:
        return "exact-order"
    for item in modules:
        if item["softdep"] or item["weakdep"]:
            fail("permuted closure has required module soft/weak constraints")
    reject_required_dependency_rules(metadata, set(names) | set(builtin_names))
    reachable, pending = set(), ["qcom_q6v5_pas"]
    while pending:
        name = pending.pop()
        if name not in reachable:
            reachable.add(name)
            pending.extend(value for value in graph[name] if value in graph)
    if reachable != set(names):
        fail("permuted inventory is not the seed's hard dependency closure")
    return "hard-dependency-equivalent"


def record_closure_comparison(result, expected, receipt, args, text, modules, builtins, source,
                              dependency_metadata=None):
    # Persist evidence before the comparison can fail. These
    # files describe trusted dry-run output; they do not authorize extra modules.
    raw_limit = 4 << 20
    fd = os.open(source, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK)
    with os.fdopen(fd, "rb") as stream:
        observed = os.fstat(stream.fileno())
        if not stat.S_ISREG(observed.st_mode) or observed.st_size > raw_limit:
            fail("invalid or excessive source modprobe evidence")
        source_raw = stream.read(raw_limit + 1)
    emitted_raw = text.encode("utf-8")
    if len(source_raw) > raw_limit or len(emitted_raw) > raw_limit:
        fail("modprobe evidence exceeds raw output bound")
    comparison = {
        "schema_version": 1, "kernel_release": KVER,
        "initrd_sha256": receipt["initrd_sha256"], "initrd_size": receipt["initrd_size"],
        "expected_sha256": receipt["expected_sha256"], "expected_size": receipt["expected_size"],
        "expected_modules": [m["archive_path"] for m in expected["modules"]],
        "observed_modules": modules, "expected_builtins": expected["builtins"],
        "observed_builtins": builtins,
        "source_modprobe": {"path": "source-modprobe.txt", "args": expected["modprobe"]["args"],
                           "size": len(source_raw), "sha256": hashlib.sha256(source_raw).hexdigest(),
                           "expected_sha256": expected["modprobe"]["stdout_sha256"]},
        "emitted_modprobe": {"path": "emitted-modprobe.txt", "args": args,
                            "size": len(emitted_raw), "sha256": hashlib.sha256(emitted_raw).hexdigest()},
    }
    if dependency_metadata is not None:
        comparison["dependency_metadata"] = dependency_metadata
    encoded = (json.dumps(comparison, indent=2, sort_keys=True) + "\n").encode("utf-8")
    if len(encoded) > 1 << 20:
        fail("closure comparison evidence exceeds JSON bound")
    directory = os.open(result, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW)
    try:
        for name, data in (("source-modprobe.txt", source_raw), ("emitted-modprobe.txt", emitted_raw),
                           ("closure-comparison.json", encoded)):
            fd = os.open(name, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW,
                         0o600, dir_fd=directory)
            with os.fdopen(fd, "wb") as output:
                output.write(data)
    finally:
        os.close(directory)
    return comparison


def internal_verify():
    receipt = read_json("/input/audit-root.json")
    expected = read_bound_expected("/input/expected.json", receipt)
    if receipt.get("schema_version") != 1 or receipt.get("kernel_release") != KVER:
        fail("unsupported audit receipt")
    if receipt["initrd_sha256"] != expected["initrd"]["sha256"]:
        fail("audit receipt refers to a different initrd")
    listed = set()
    for item in receipt["files"]:
        rel = safe_relative(item["path"])
        if str(rel) in listed:
            fail("duplicate audit staging receipt path")
        file = Path("/audit") / rel
        if file.is_symlink() or not file.is_file():
            fail("audit staging contains a non-regular file")
        if digest(file, 256 << 20) != {k: item[k] for k in ("size", "sha256")}:
            fail("audit staging changed after Go inspection")
        listed.add(str(rel))
    actual = set()
    for root, dirs, files in os.walk("/audit", followlinks=False):
        for name in dirs + files:
            p = Path(root) / name
            if p.is_symlink():
                fail("audit staging contains symlink")
        actual.update(str((Path(root) / name).relative_to("/audit")) for name in files)
    if actual != listed:
        fail("audit staging contains unreceipted files")
    args, text, paths, builtins = closure("/audit", "/audit/etc/modprobe.d")
    normalized = []
    for value in paths:
        prefix = f"/audit/lib/modules/{KVER}/"
        if not value.startswith(prefix):
            fail(f"modprobe escaped staged selected release: {value}")
        normalized.append("usr/" + value.removeprefix("/audit/"))
    metadata = read_dependency_metadata("/input", expected["module_metadata"], source=True)
    metadata += read_dependency_metadata("/audit", receipt["files"])
    diagnostics = record_closure_comparison("/result", expected, receipt, args, text, normalized, builtins,
                                           "/input/evidence/source-modprobe.txt", metadata)
    if diagnostics["source_modprobe"]["sha256"] != expected["modprobe"]["stdout_sha256"]:
        fail("copied raw source modprobe output differs from audited stdout identity")
    comparison = compare_module_closure(expected, normalized, builtins, metadata)
    write_json(Path("/result/verified.json"), {
        "schema_version": 1, "initrd_sha256": receipt["initrd_sha256"],
        "expected_sha256": receipt["expected_sha256"], "expected_size": receipt["expected_size"],
        "kernel_release": KVER, "module_closure": normalized, "builtins": builtins,
        "closure_comparison": comparison,
        "modprobe_command": args, "modprobe_stdout_sha256": hashlib.sha256(text.encode()).hexdigest(),
        "scope": "native fixture emitted bytes and PAS dependency resolution; no hardware qualification"})


def host(mode, output):
    if os.uname().machine not in ("aarch64", "arm64"):
        fail("native ARM64 runner required; emulation/host kernel fallback is unsupported")
    runtime = shutil.which("docker") or shutil.which("podman")
    if not runtime:
        fail("existing Docker or Podman is required; this script does not install a runtime")
    if Path(runtime).name == "docker":
        if os.environ.get("BUILDX_BUILDER") or os.environ.get("BUILDKIT_HOST"):
            fail("custom/remote BuildKit builders are outside this native fixture")
        endpoint = os.environ.get("DOCKER_HOST")
        if not endpoint:
            _, raw = run([runtime, "context", "inspect", "--format",
                          "{{json .Endpoints.docker.Host}}"], env=os.environ.copy())
            endpoint = json.loads(raw)
        if not endpoint.startswith("unix://"):
            fail("fixture requires an existing local Unix-socket Docker engine")
    elif os.environ.get("CONTAINER_HOST") or os.environ.get("CONTAINER_CONNECTION"):
        fail("remote Podman is outside this native fixture")
    out = Path(output)
    if not out.is_absolute() or "," in str(out) or "\n" in str(out):
        fail("OUTPUT must be an absolute mount-safe path")
    repo = HERE.resolve().parents[1]
    if mode == "prepare":
        if out.exists():
            fail("OUTPUT must not exist; do not overwrite another run")
        existing = out.parent
        while not existing.exists():
            existing = existing.parent
        minimum_space = 6 << 30
        space = {"minimum_free_bytes": minimum_space,
                 "output_filesystem_free_bytes": shutil.disk_usage(existing).free}
        if space["output_filesystem_free_bytes"] < minimum_space:
            fail("OUTPUT filesystem needs at least 6 GiB free")
        _, info_text = run([runtime, "info", "--format", "{{json .}}"], env=os.environ.copy())
        info = json.loads(info_text)
        storage = info.get("DockerRootDir") or info.get("store", {}).get("graphRoot")
        space["runtime_storage_path"] = storage
        space["runtime_storage_free_bytes"] = None
        if storage:
            try:
                space["runtime_storage_free_bytes"] = shutil.disk_usage(storage).free
            except OSError:
                pass  # Remote/inaccessible daemon storage is explicitly unobserved.
        if space["runtime_storage_free_bytes"] is not None and space["runtime_storage_free_bytes"] < minimum_space:
            fail("runtime storage filesystem needs at least 6 GiB free")
        out.mkdir(mode=0o700, parents=True)
        (out / "evidence").mkdir()
        write_json(out / "preflight-space.json", space)
        # Minimal build context: never send .git, home, credentials or arbitrary repo files.
        context = out / "context"
        context.mkdir()
        for name in ("build.sh", "pins.json", "Containerfile"):
            shutil.copyfile(HERE / name, context / name)
        tag = "localhost/tbox-x13s-fixture:" + hashlib.sha256(str(out).encode()).hexdigest()[:20]
        # The enclosing CI job caps total elapsed time; per-command deadlines also apply.
        command = [runtime, "build", "--platform=linux/arm64", "--pull=true", "--no-cache",
                   "--tag", tag, "--file", context / "Containerfile", context]
        status, buildlog = log_run(command, out / "evidence/container-build.log",
                                  timeout=1200, maximum=4 << 20, check=False,
                                  env=os.environ.copy())
        if status:
            fail(f"container build failed ({status}); see evidence/container-build.log\n{buildlog[-16384:]}")
        _, raw = run([runtime, "image", "inspect", tag], env=os.environ.copy())
        image = json.loads(raw)[0]
        if image.get("Architecture") not in ("arm64", "aarch64"):
            fail("built image has wrong architecture")
        image_id = image["Id"]
        write_json(out / "runtime.json", {"runtime": Path(runtime).name, "image_id": image_id,
                   "base_image": PINS["base_image"], "image_tag": tag})
        _, version = run([runtime, "version"], env=os.environ.copy())
        (out / "evidence/runtime-version.txt").write_text(version)
    else:
        state = read_json(out / "runtime.json")
        if state["runtime"] != Path(runtime).name:
            fail("runtime differs from prepare")
        image_id = state["image_id"]
    common = [runtime, "run", "--rm", "--network=none", "--read-only",
              "--security-opt=no-new-privileges", "--pids-limit=256",
              "--memory=4g", "--cpus=2", "--tmpfs=/tmp:rw,exec,nosuid,size=2g",
              "--tmpfs=/run:rw,nosuid,noexec,size=64m", "--tmpfs=/sys:ro,nosuid,noexec,size=1m"]
    mount = lambda src, dst, ro=True: ["--mount", f"type=bind,src={src},dst={dst}" + (",readonly" if ro else "")]
    def container(command, phase, timeout):
        name = "tbox-x13s-" + hashlib.sha256(str(out).encode()).hexdigest()[:20] + "-" + phase
        command = [*command[:2], "--name", name, *command[2:]]
        try:
            return run(command, timeout=timeout, env=os.environ.copy())
        finally:
            # Exact owned ephemeral container only. No pruning, host cleanup, or
            # deletion of source/output/evidence, even when the command failed.
            run([runtime, "rm", "--force", name], timeout=30, check=False, env=os.environ.copy())
    if mode == "prepare":
        container([*common, *mount(out, "/out", False), image_id, "_stage"], "stage", 60)
        command = [*common, *mount(out, "/out", False),
                   *mount(out / "config", "/usr/lib/dracut/dracut.conf.d"),
                   *mount(out / "empty.d", "/fixture-empty")]
        for name in MODULE_DIRS:
            directory = repo / "src/dracut" / name
            if not directory.is_dir():
                fail("required reviewed module directory missing")
            command += mount(directory, "/usr/lib/dracut/modules.d/" + name)
        command += [image_id, "_emit"]
        write_json(out / "runtime-command.json", {"emit": command, "capability_policy":
                   "runtime defaults, no added capabilities; ldconfig -r needs chroot"})
        container(command, "emit", 720)
    elif mode == "verify":
        receipt = read_json(out / "audit-root.json")
        root = Path(receipt["audit_root"])
        if root.is_symlink() or not root.is_dir() or not root.resolve().is_relative_to(out.resolve()):
            fail("audit_root must be an owned directory below OUTPUT")
        if root.stat().st_uid != os.geteuid() or stat.S_IMODE(root.stat().st_mode) != 0o700:
            fail("audit_root must be owned by this caller with mode 0700")
        expected = read_json(out / "expected.json")
        if digest(out / "initrd.img") != {k: expected["initrd"][k] for k in ("size", "sha256")}:
            fail("initrd changed after prepare")
        result = out / "verification"
        result.mkdir()
        user = ["--user", f"{os.geteuid()}:{os.getegid()}"]
        if Path(runtime).name == "podman":
            user += ["--userns=keep-id"]
        command = [*common, "--cap-drop=ALL", *user, *mount(root, "/audit"),
                   *mount(out, "/input"), *mount(result, "/result", False), image_id, "_verify"]
        container(command, "verify", 90)
        shutil.copyfile(result / "verified.json", out / "verified.json")
        shutil.copyfile(result / "emitted-modprobe.txt", out / "evidence/emitted-modprobe.txt")
    print(f"{mode}: {out}")


try:
    if len(sys.argv) == 2 and sys.argv[1].startswith("_"):
        {"_install": internal_install, "_stage": internal_stage,
         "_emit": internal_emit, "_verify": internal_verify}[sys.argv[1]]()
    elif len(sys.argv) == 3 and sys.argv[1] in ("prepare", "verify"):
        host(sys.argv[1], sys.argv[2])
    else:
        fail("usage: build.sh {prepare|verify} /absolute/new/output")
except (ValueError, OSError, subprocess.SubprocessError, KeyError) as error:
    print(f"fixture: {error}", file=sys.stderr)
    sys.exit(1)
PY
