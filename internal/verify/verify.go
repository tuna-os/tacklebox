package verify

import (
	"strings"
)

// IsISO returns true if the artifact path has a .iso extension (case-insensitive).
func IsISO(path string) bool {
	return strings.HasSuffix(strings.ToLower(path), ".iso")
}

// Verify runs verification on an ISO or block image artifact.
func Verify(path string) []CheckResult {
	if IsISO(path) {
		return VerifyISO(path)
	}
	return VerifyBlock(path)
}
