# recipe-gen input reference

`tacklebox recipe-gen` reads one YAML or JSON file and writes a JSON recipe.
It only reads and converts configuration; it does not pull images, build media,
or require root. The input uses the same field names as a full recipe, with
most fields optional. See the [user guide](USER-GUIDE.md#2-recipes) for how
recipes affect media builds.

```sh
tacklebox recipe-gen environments.yaml > recipe.json
# Equivalent, with a confirmation message on stderr:
tacklebox recipe-gen environments.yaml --output recipe.json
```

`-o` is the short form of `--output`. Output is always JSON, indented with two
spaces and ending with a newline, regardless of the input or output filename.
The output file is created or overwritten. Input must be a file path; `-`
does not read stdin. YAML and JSON both go through the YAML parser, so the
filename extension does not select a parser.

## Input fields

Use a top-level mapping/object and the exact snake_case names below. Quote
size values and image references as strings. YAML booleans should be `true`
or `false`.

| Field | Type | Requirement / behavior |
|---|---|---|
| `media_name` | string | Required, nonempty media label. |
| `bootable_environments` | array of objects | Required, at least one environment. Order determines the default boot environment. |
| `size` | string | Optional; estimated when empty or omitted. Use sizes such as `"20G"` or `"8192M"`. |
| `default_boot` | string | Optional environment ID; defaults to the first environment's `id`. |
| `shared_store` | object | Optional; fields listed below. |
| `partitions` | object | Optional string fields `esp`, `store`, and `persist`, such as `esp: "1G"`. Passed through without estimating partition sizes. |
| `offline_payloads` | array | Optional image-reference strings or objects with nonempty string `source` and `ref` fields. See the offline example. |
| `kargs` | array of strings | Optional extra kernel arguments; passed through. |

Each `bootable_environments` entry supports:

| Field | Type | Requirement / behavior |
|---|---|---|
| `id` | string | Supply a unique, nonempty ID for a usable recipe; used in paths and boot entries. |
| `image` | string | Supply a bootable container image reference for a usable recipe. |
| `title` | string | Optional; empty or omitted becomes `id`. |
| `modes` | array of strings | Optional; omitted, `null`, or `[]` becomes `["live"]`. Use `live`, `persistent`, or both. |
| `desktop`, `backend` | string | Optional full-recipe settings; passed through. |
| `skip_initramfs_rebuild` | boolean | Optional; passed through. Enable only for images already shipping the required modules. |
| `live_customize` | array of strings | Optional script paths; passed through. Relative paths resolve from the generated recipe's directory when building, so keep it beside the input or adjust paths. |
| `remora` | string or object | Optional manifest path/URL or inline manifest; passed through as JSON. |

`shared_store` supports these optional fields:

| Field | Type | Generation behavior |
|---|---|---|
| `format` | string | Empty or omitted becomes `"ext4"`. |
| `dedup` | boolean | Defaults to `false` for one environment; forced to `true` for two or more. |
| `compression`, `compressor` | string | Passed through; generation does not fill build-time compression defaults. |
| `dedup_layout`, `delta_base` | string | Passed through; see [multi-env ISOs](USER-GUIDE.md#4-multi-env-isos-and-dedup). |
| `prune_source_images` | boolean | Passed through; removes source payload images during a build when enabled. Intended for ephemeral builders. |

## Default rules and validation limits

When `size` is empty or omitted, the estimate is **5G per environment + 3G**:
`8G` for one, `13G` for two, `18G` for three. The extra 3G represents a 1G ESP
and 2G persistence partition. This is a fixed estimate, not an inspection of
image sizes. Partition overrides and offline payloads do not increase it;
choose an explicit size when needed. Generation leaves omitted partition sizes
unset; the build computes them later.

For multiple environments, even an explicit `shared_store.dedup: false` becomes
`true`. To disable it, edit the generated JSON afterward. Dedup affects ISO
targets only. Explicit nonempty size, format, title, default_boot, and modes
are preserved; an empty string triggers the corresponding default.

The generator rejects malformed input, incompatible field types, an empty
`media_name`, and a missing or empty environment list. It does **not** validate
image availability, unique/nonempty IDs, image presence, mode names, size
syntax, or whether `default_boot` names an existing environment. Successful
generation is not proof that a recipe can build or boot.

Pass-through means fields recognized by the full recipe schema survive as
values, subject to the defaults above. Unknown keys (including misspellings)
are silently dropped, not preserved. Output is serialized from the recipe
structure: key order and YAML comments are lost, some unset fields appear as
empty strings, `{}`, or `null`, and optional zero-valued fields may disappear.
For example, an omitted `offline_payloads` is emitted as `null`. Payload
objects whose `source` and `ref` are equal are emitted as a single string.

## Examples

Save any example to a file and run the command above. Image references here
illustrate the input format; generation does not contact their registries.

### Single environment (YAML)

```yaml
media_name: TUNAOS
bootable_environments:
  - id: gnome
    image: ghcr.io/tuna-os/yellowfin:gnome
```

This produces `size: "8G"`, `default_boot: "gnome"`, store format `ext4`,
title `gnome`, and modes `["live"]`. Dedup remains false (and is omitted
from JSON).

### Multiple environments with dedup (YAML)

```yaml
media_name: TUNAOS-MULTI
shared_store:
  dedup: true
  compression: release
bootable_environments:
  - id: gnome
    image: ghcr.io/tuna-os/yellowfin:gnome
    title: GNOME
  - id: gnome-hwe
    image: ghcr.io/tuna-os/yellowfin:gnome-hwe
    title: GNOME (HWE)
```

The size becomes `13G`, the default boot is `gnome`, and both environments
get `["live"]`. Omitting `dedup` produces the same enabled setting.

### Offline payloads and explicit overrides (JSON)

```json
{
  "media_name": "TUNAOS-OFFLINE",
  "size": "30G",
  "default_boot": "kde",
  "partitions": {"esp": "1G", "persist": "4G"},
  "kargs": ["console=ttyS0"],
  "bootable_environments": [
    {
      "id": "kde",
      "image": "ghcr.io/tuna-os/yellowfin:kde",
      "title": "KDE offline installer",
      "modes": ["live"]
    }
  ],
  "offline_payloads": [
    "ghcr.io/tuna-os/yellowfin:kde",
    {"source": "localhost/custom-os:stable", "ref": "ghcr.io/example/custom-os:stable"}
  ]
}
```

These size, partition, kernel argument, and payload values survive generation.
Payloads are not inferred from the environment list: list every image you want
embedded. A string uses the same image name as source and destination; an
object maps a builder-side image to its canonical name in the embedded store.
See [offline installs](USER-GUIDE.md#5-offline-installs-offline_payloads).
