// Package livemedia holds the media layout and security notice shared by live
// ISO builders.
package livemedia

// ReleaseNotesName is the notice at the root of each generated live ISO.
const ReleaseNotesName = "RELEASE-NOTES.txt"

// ReleaseNotes documents the generated boot options, including when the source
// image normally enforces SELinux. Keep this notice distro-neutral.
const ReleaseNotes = `Tacklebox live media: security limitations

The native and browser builders label the live root of an image that ships an
SELinux policy, and its live session follows the mode in /etc/selinux/config.
Every other live entry includes enforcing=0: the squashfs paths and DDI
builds. There, policy denials are not enforced. Images without SELinux do not
gain SELinux protection.

The live installer can run with elevated privileges, passwordless autologin,
and screen locking disabled. Use trusted images, recipes and customization
scripts, and run live sessions only in trusted environments. Unix permissions
and other security mechanisms still apply.

This notice describes the live session, not the installed system. Verify the
installed system's SELinux policy and mode independently.

Enforcement restoration is tracked at:
https://github.com/tuna-os/tacklebox/issues/281
`
