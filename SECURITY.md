# Security Policy

## Supported Versions

The project publishes Tacklebox as a Go binary and as a container image on GHCR.
Only the latest release is actively supported.

| Version | Status |
|---|---|
| Latest release | ✅ Supported |
| Older releases | ❌ Unsupported — upgrade to latest |
| `main` branch | ⚠️ Best effort (development) |

## Reporting a Vulnerability

**Please do not report security vulnerabilities through public GitHub issues.**

Instead, report them privately via GitHub Security Advisories:

1. Go to the [Security tab](https://github.com/tuna-os/tacklebox/security)
2. Click **Report a vulnerability**
3. Provide a detailed description of the issue, including steps to reproduce

You can expect:
- **Acknowledgment** within 48 hours
- **Status update** within 5 business days
- **Resolution timeline** based on severity

## Security Model

Tacklebox:
- Uses the memory-safe Go language
- Runs with elevated privileges (root required for disk operations)
- Executes `bootc`, `dracut`, `sgdisk`, and other system tools with validated arguments
- Uses BuildKit secret mounts, never environment variables, for sensitive data
- Operates on user-provided images from trusted registries

### Live media

Generated live ISOs request **SELinux permissive mode** (`enforcing=0`) for
the entire live session, even if the source image uses SELinux to enforce
access rules. On SELinux-enabled images, SELinux does not block access. This
applies to every live ISO layout and the CLI, native pure-Go, browser and
DDI builders. It does not describe the SELinux mode of an installed system;
verify that system's policy and mode independently.

Live assembly does not reliably retain the SELinux labels that a boot with
SELinux enforcement needs. If you remove `enforcing=0`, unlabeled live
roots can fail to boot. See [the architecture notes](ARCHITECTURE.md#live-media-security-limitations)
for the build constraints and the separate downgrade of dm-verity in DDI media.

The live installer can run with elevated privileges, passwordless autologin
and the screen lock disabled. SELinux MAC enforcement does not limit code
that runs in that session. Use trusted images, recipes and customization
scripts; use the live session in a trusted environment and protect access to
target disks and the network. Unix permissions and other security mechanisms
still apply.

Each generated ISO includes `/RELEASE-NOTES.txt` with this limitation.
Include it in release announcements for downstream ISOs as well. Enforcement
restoration remains tracked in [#281](https://github.com/tuna-os/tacklebox/issues/281).

## Supply Chain Security

- Commit SHAs fix the versions of GitHub Actions.
- `go build` and a fixed toolchain give a reproducible Go build.
- Multi-stage Dockerfiles create container images with a small surface.
- `go.mod` manages external dependencies and verifies their checksums.

## Disclosure Policy

We follow coordinated disclosure:
1. The reporter submits a vulnerability through a private channel.
2. We investigate and develop a fix.
3. We release the fix in a new version.
4. We publish an advisory after the release.

See `ARCHITECTURE.md` and `README.md` for full architecture and usage details.
