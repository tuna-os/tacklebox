//go:build !linux

package medium

import "errors"

func inspectDevice(string) (DeviceInfo, error) {
	return DeviceInfo{}, errors.New("block device inspection is only implemented on Linux")
}
