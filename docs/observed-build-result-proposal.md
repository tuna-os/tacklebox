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

## Overlay evidence

The engine retains the digest when it resolves the overlay.
It reports that digest only after it applies the layers.
An error returns no observation.
An overlay without new layers retains its existing error behavior.

`baseAnnotationMatched` is true only when an annotation exists and matches the source digest.
An absent annotation does not prove linkage.
A requested digest does not count as an observed annotation.

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
