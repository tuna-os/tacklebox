// Package livemedia holds the media layout and security notice shared by live
// ISO builders.
package livemedia

// ReleaseNotesName is the notice at the root of each generated live ISO.
const ReleaseNotesName = "RELEASE-NOTES.txt"

// ReleaseNotes documents the generated boot options, including when the source
// image normally enforces SELinux. Keep this notice distro-neutral.
const ReleaseNotes = `Tacklebox live media: security limitations

Generated live boot entries include enforcing=0. On SELinux-enabled images,
this requests permissive mode for the entire live session: policy denials
are not enforced. An enforcing source image does not imply an enforcing
live ISO. Images without SELinux do not gain SELinux protection.

The rootless live assembly paths do not preserve the SELinux labels needed
for a reliable enforcing boot. This also applies to the pure-Go native and
browser builders. DDI live entries request permissive mode even when the
supplied root filesystem has labels.

The live installer can run with elevated privileges, passwordless autologin,
and screen locking disabled. Use trusted images, recipes and customization
scripts, and run live sessions only in trusted environments. Unix permissions
and other security mechanisms still apply; SELinux MAC enforcement does not.

This notice describes the live session, not the installed system. Verify the
installed system's SELinux policy and mode independently. Removing enforcing=0
alone does not restore missing labels and can prevent the live system booting.

Enforcement restoration is tracked at:
https://github.com/tuna-os/tacklebox/issues/281
`
