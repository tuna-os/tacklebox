# Contributing

Thank you for your interest in this project! It is part of the [TunaOS](https://tunaos.org) ecosystem.

<!-- hive-contribute-plea: donated-compute appeal, keep in sync across repos -->
## Contribute compute — no code needed

**[Donate time with your AI CLI](https://hub.tunaos.org/#contribute).**

Your machine runs one task at a time using your signed-in AI coding CLI.
The relay alternates between eligible **School** (OS) and **Reef** (apps)
hives after each task. If one hive has no work, the other can continue.

The guide covers prerequisites, verified `just` setup, credentials, provider
costs, and how to stop with Ctrl+C. Your subscription quota or API charges apply.
Never post account credentials or API keys in issues or pull requests.

## Getting Started

1. Fork the repo and clone it locally.
2. Install the Go version declared in [`go.mod`](go.mod) and the
   [`just`](https://just.systems) command runner.
3. Read the project [README](README.md), [user guide](docs/USER-GUIDE.md), and
   [architecture overview](ARCHITECTURE.md).
4. Open an issue to discuss your change before you submit a PR.

## Validate Your Changes

Run the fast checks used for every pull request:

```bash
just build
just test
go vet ./...
go test -tags=integration -run='Help' ./...
```

If you change an example or fixture recipe, make sure that every JSON file
still parses:

```bash
for recipe in examples/*.json fixtures/*.json; do
  python3 -m json.tool "$recipe" >/dev/null
done
```

Media builds and boot tests need more host tools, elevated privileges, and
substantial disk space. Run the relevant script under `scripts/` when your
change affects the image, bootloader entries, or live boot path. The
pull-request CI runs the broader smoke suite.

## Pull Requests

- Keep PRs focused — one change per PR.
- Follow the existing code style and conventions.
- Update docs if your change affects usage.
- Include the validation commands you ran in the PR description.

## Versioning and Releases

Use Conventional Commits for commits and squash-merge PR titles:

- `feat:` adds a feature and bumps the minor version.
- `fix:` fixes a bug and bumps the patch version.
- `feat!:` or a `BREAKING CHANGE:` footer marks a breaking change. Before
  1.0, this bumps the minor version; from 1.0 onward, it bumps the major version.

Release-please runs on pushes to `main` and opens a release PR with the
version manifest and changelog. The first release is v0.1.0. A maintainer
reviews and merges the release PR to create the tag and GitHub Release.
The repository must allow GitHub Actions to create pull requests.

The release-please workflow then calls the binary and container publishers
with that tag. This explicit call is needed because tags created with
`GITHUB_TOKEN` do not trigger other workflows. No personal access token is needed.
GoReleaser uploads static Linux amd64/arm64 tarballs and SHA-256 checksums to
GitHub Releases. Each binary reports its version with `tacklebox --version`.
The container workflow publishes both `v0.1.0` and `0.1.0` style tags to GHCR,
as well as the existing `latest` and commit tags.

Tag pushes from maintainers also run the publishers. To test binary packaging
without publishing, run `goreleaser check` and
`goreleaser release --snapshot --clean`, or dispatch the binary workflow on a
branch. A manual run on a release tag publishes assets to that release.

## Questions?

- [TunaOS Documentation](https://tunaos.org)
- [GitHub Issues](https://github.com/tuna-os/tunaOS/issues)
