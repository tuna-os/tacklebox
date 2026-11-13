package livemedia

// PxebootDir is the directory, from the ESP and ISO root, that holds one
// environment's kernel, initrd and optional devicetree. Generated BLS entries
// and GRUB configurations refer to files in it, so every live ISO builder must
// use the same layout. The result has a leading and a trailing slash.
func PxebootDir(envID string) string { return "/images/pxeboot/" + envID + "/" }

// KernelPath is the path of an environment's kernel on live media.
func KernelPath(envID string) string { return PxebootDir(envID) + "vmlinuz" }

// InitrdPath is the path of an environment's initrd on live media.
func InitrdPath(envID string) string { return PxebootDir(envID) + "initrd.img" }

// DevicetreePath is the path of an environment's devicetree on live media.
func DevicetreePath(envID string) string { return PxebootDir(envID) + "devicetree" }
