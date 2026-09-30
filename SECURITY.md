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
