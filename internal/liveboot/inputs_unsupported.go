//go:build !linux

package liveboot

import "fmt"

// ReadInput refuses native payload preparation on unsupported host platforms.
// Portable PE/board/scope validation remains available; there is no weaker
// path-opening fallback when Linux descriptor-relative no-follow IO is absent.
func ReadInput(_ string, _ Input) ([]byte, error) {
	return nil, fmt.Errorf("experimental X13s payload preparation requires a Linux host")
}
