# Dakota reference cell

This triage record covers the three failures in
[`tuna-os/tacklebox#180`](https://github.com/tuna-os/tacklebox/issues/180).
They are separate classes of failure. Do not combine them into one retry
result.

## Evidence

| run | observed failure | likely boundary | next evidence |
| --- | --- | --- | --- |
| 31131445981 | image build completed, but the live ISO produced serial output and never emitted `TUNAOS_LIVE_READY` during the 900-second gate | initramfs/live-root handoff | preserve the serial log and record the selected kernel, initramfs section list, and live cmdline |
| 31136095286 | build stopped at `Authoring EROFS live root… 0/1` after all 120 layers had unpacked | EROFS authoring or its input tree, not registry fetch | capture RSS, disk space, and the last authoring phase before changing the image |
| 31140381276 | build stopped at `Unpacking layer 2/120` with a small wasm payload | registry body stall (#156), not EROFS | require the fetch timeout/resume diagnostics and the final layer/byte offset |

Keep the runs as three fixtures for regression tests. A successful image build
does not prove that the ISO boots. Do not treat a retry of a layer fetch as a
fix for creation of the filesystem.

## Existing coverage

`internal/oci/resume.go` handles the layer failure. Body reads have a deadline
for stalls. The code closes stalled streams and resumes them with a range
request. Header timeouts put a limit on the reopen path. Existing OCI tests
cover the resume path. Keep this instrumentation during the Dakota tests.

The native ISO path in pure Go has separate phases for EROFS, the ESP, and the
ISO. The CI `pure-iso` job captures the serial boot log. The EROFS phase still
needs a run with a Dakota-sized image. A watchdog timeout alone would hide the
phase that failed.

## Dakota acceptance checklist

Before you mark the reference cell green, run one build with this evidence:

1. Retain progress for OCI layers and messages about stalls or resumes in the
   artifact.
2. Retain progress for EROFS plus samples of resident memory and free space at
   least once per minute.
3. Inspect the combined initramfs as separate raw and compressed sections.
   Confirm that it has the tbox hooks and the stock hooks from the image.
4. Retain a QEMU serial log until `TUNAOS_LIVE_READY` or the 900-second limit.
   Put the kernel command line and selected initramfs beside the log.

Use a Dakota rerun to validate a code change only after these artifacts exist.
This rule keeps the three defects separate when a random layer or serial log
is missing. Thus, the test does not appear intermittent.

## Decision

Do not add a workaround specific to this image. Rerun Dakota only with the
three evidence streams above. The tested binary must also contain the merged
path for layer deadlines. If the fetch trace has no errors, target the EROFS
phase in the next change. If EROFS completes, use the live boot trace to test
the stock initramfs against the prepended tbox overlay. This sequence keeps the
expensive diagnostic for the reference cell and does not hide one failure with
a workaround for another.
