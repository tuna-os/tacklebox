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
just check
just build
go vet ./...
go test -tags=integration -run='Help' ./...
```

You can format Go files with `just fmt`.

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

## Questions?

- [TunaOS Documentation](https://tunaos.org)
- [GitHub Issues](https://github.com/tuna-os/tunaOS/issues)
