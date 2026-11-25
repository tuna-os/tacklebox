package medium

import (
	"context"
	"errors"
	"fmt"
	"os"
)

// ErrSignatureUnverified is returned by the default verifier: signature
// verification is not implemented yet, so the image is not authenticated.
var ErrSignatureUnverified = errors.New("image signature and qualification receipt are not verified: cosign verification is not implemented yet (TODO tuna-os/tacklebox#511); pass --insecure-skip-verify to continue without it")

// Verifier authenticates an installer image before any byte is written.
// subject is the digest of the manifest; manifest is its raw bytes.
//
// A real implementation checks a cosign signature whose identity is the CI
// workflow (never a branch name) and the qualification receipt attestation.
type Verifier interface {
	Verify(ctx context.Context, ref, subject string, manifest []byte) error
}

// StubVerifier always fails. It is the default so that nobody mistakes an
// unauthenticated write for a verified one.
type StubVerifier struct{}

func (StubVerifier) Verify(context.Context, string, string, []byte) error {
	return ErrSignatureUnverified
}

// InsecureSkipVerifier accepts every image. Use it only behind an explicit
// user flag; it prints a warning to stderr.
type InsecureSkipVerifier struct{}

func (InsecureSkipVerifier) Verify(_ context.Context, ref, subject string, _ []byte) error {
	fmt.Fprintf(os.Stderr, "WARNING: signature verification skipped for %s (%s)\n", ref, subject)
	return nil
}
