# Contributing

Thank you for your interest in this project! It is part of the [TunaOS](https://tunaos.org) ecosystem.

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

## Questions?

- [TunaOS Documentation](https://tunaos.org)
- [GitHub Issues](https://github.com/tuna-os/tunaOS/issues)
