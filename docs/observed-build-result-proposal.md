# Observed build results

This API supports tunaOS#3470 and iso-builder#270.
It reports observations from the engine.
It does not qualify media.
It does not authenticate a producer.
The consumer and its engine pin stay unchanged.

## API contract

`tboxBuildIso(opts, onChunk)` returns a Promise with a numeric byte count.
`tboxBuildIsoWithResult(opts, onChunk)` returns a Promise with an object.
The object contains `bytesWritten` and `observedInputs`.
The schema version is 1.
Both APIs use the same build path.

The engine returns a result after output and cleanup succeed.
An error rejects the Promise without a result.
Discard partial output after an error.
No API exposes a result from a previous operation.

The byte count has a maximum of 9007199254740991.
The engine rejects output that would exceed this limit before it calls the sink.
This limit preserves an exact integer in JavaScript.

## Source evidence

The resolver hashes the exact bytes of each manifest.
It retains the hash of the index at selection time.
A direct manifest has no index observation.
The resolver checks the child hash against its descriptor.
It does not fetch the tag again for evidence.
A request by digest must match the bytes of the manifest or index.

The optional fields `registryEndpoint`, `repository` and `requestedReference`
retain the exact coordinates of that resolve operation.
They are additive fields in version 1. Older results can omit all three.
The endpoint is a transport and can be a proxy. It does not prove the origin.
Endpoints with credentials, queries or fragments fail before a request.

A collector must check these coordinates against the actual operation and an
independently reviewed map from origin to transport. It can then combine the
declared origin, consumed repository and observed child digest into a reference.
An unknown mapping must stay unbound. These fields do not observe CPU policy
or authenticate the producer.

`platformDescriptor` contains architecture and variant from the index.
These fields do not prove hardware support.
A direct manifest has no platform descriptor.
The API does not copy a requested platform into this field.

The source manifest contains a descriptor for the config.
The API retains that descriptor without an extra download.
`bytesVerified` starts as false.

An offline build consumes the config blob.
The reader checks its digest on close.
Only successful verification sets `bytesVerified` to true.
A descriptor alone does not prove that the engine consumed those bytes.

An observed result can also include `source.config.content` with `os`,
`architecture` and `variant`. These values are read from the retained JSON only
after the original config reader verifies its digest on close and its OPFS
arena is sealed. They are not copied from the request or index descriptor.
Older results and builds which do not consume config bytes omit this object.
No CPU baseline or hardware-support claim follows from these fields.

## Overlay evidence

The engine retains the digest when it resolves the overlay.
It reports that digest only after it applies the layers.
An error returns no observation.
An overlay without new layers retains its existing error behavior.

`baseAnnotationMatched` is true only when an annotation exists and matches the source digest.
An absent annotation does not prove linkage.
A requested digest does not count as an observed annotation.

`overlay.baseDigest` retains the exact annotation selected by the existing base
verification, after all overlay layers apply. The actual-base annotation keeps
its existing precedence over the declared-base annotation. Missing, mismatched
and failed overlays produce no applied identity. Request values cannot supply
this observation.

The status `custom-image` means the caller chose to omit the curated overlay.
The status `absent` means this build did not apply an overlay.
This follows the existing policy for optional overlays.
It does not prove that the registry has no overlay.

## State and reset

The engine reserves an operation before its Promise starts.
If an operation is active, the engine rejects another call without a change to state.
This rule covers introspection, builds, exports and reset.
Handle the rejected Promise.

Each build copies mutable metadata and xattrs.
It retains immutable references to blob content.
It does not copy file bodies.
The base tree stays unchanged.

Each build owns its post arena and offline metadata.
Cleanup removes those arenas after success or failure.
The next build starts from the same base.

`tboxReset()` keeps its synchronous null return on success.
It clears state immediately.
It queues OPFS cleanup.
The next operation waits for that cleanup before it uses files.
`tboxResetAsync()` returns a Promise for explicit completion and errors.
A failed introspection also clears old facts and evidence.

Cleanup aborts an open writer before deletion.
A cleanup error rejects the build and blocks later operations.
Reset can retry incomplete cleanup.
After removal, the engine records success to avoid a second deletion.
The original error remains available if cleanup also fails.

## Limits and acceptance

The API does not copy requested identity into evidence.
This includes CPU baseline, hardware target and tool revisions.
Frontend identity needs a receipt from its producer.
A backend hash does not identify the frontend commit.

`installer` reports the final applied tree immediately before EROFS authoring,
after the post arena is sealed. The engine hashes the raw
`etc/tunaos/live-source.json` stamp and keeps its installer app, revision and
bundle claims under `stampClaims`. It then reads the actual Flatpak
`app/<app>/<arch>/<branch>/active` symlink, deployed metadata and application
name. Only one consistent deployment yields a `deployment`; its commit is the
actual deployment directory identifier. `sourceStampMatched` states whether
that identity agrees with the stamp, not whether the stamp is authenticated.
Without a readable regular source stamp this object is absent. With a stamp,
missing, ambiguous or inconsistent layouts retain a diagnostic missing reason.

The optional `helper` hashes the deployed `files/bin/fisherman` bytes. It does
not infer a source revision from a binary without VCS metadata and cannot
identify the frontend commit instead of the deployment. `shippedRecipe` hashes
an actual `etc/bootc-installer/recipe.json` file if present. Shipping that file
does not prove that a frontend loaded it, or define the canonical
`recipe_sha256` domain. A bundle claim in the stamp is never represented as
remeasured bundle bytes.

`engine` comes from the executable's own `debug.ReadBuildInfo`, cached once.
It is reported only when both the main module and executable package identify
tacklebox's `cmd/tbwasm` or `cmd/purebuild`, never from an iso-builder host
module, dependency revision or environment pin. `vcsRevision` and
`vcsModified` retain valid actual VCS settings when present; modified remains
the literal `"true"` or `"false"` setting. An absent or dirty VCS stamp does
not identify exact source bytes. Module/Go-version metadata without a VCS stamp
stays only that, not an invented revision.

The browser path consumes a BuildRequest and normalized build options. It does
not parse a `MediaRecipe` file or a separate canonical build-config document.
RFC014 currently names request, recipe and config digests without defining the
recipe/config byte domains. These additions do not equate those digests to an
arbitrary request serialization, source OCI config or frontend JavaScript.
Defining those domains requires an explicit contract clarification.

Boot inputs need separate hashes of the bytes that the engine consumes.
Paths and requested IDs do not provide those hashes.
The current API does not claim those hashes.

Resolver policies stay unchanged.
The pure resolver lets amd64/v2 use a descriptor without a variant.
The OCI resolver uses a stricter policy.
A policy change needs separate review.

Tests use small synthetic inputs without production downloads or physical disks.
Tests check hashes and errors in native code.
Tests for JavaScript execute WASM with a model of OPFS locks.
The fixture files cannot boot.
These tests do not qualify firmware, boot or installation.

Before a consumer update, complete native CI and WASM tests, then review the result.
Use a trusted collector for producer receipts and independent media hashes.
Keep diagnostic status until real boot and install checks pass.
