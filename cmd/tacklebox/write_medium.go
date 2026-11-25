package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"regexp"

	"github.com/sigstore/sigstore-go/pkg/root"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"github.com/tuna-os/tacklebox/internal/medium"
	"github.com/tuna-os/tacklebox/internal/oci"
)

var writeMediumCmd = &cobra.Command{
	Use:   "write-medium REF-OR-OCI-LAYOUT TARGET",
	Short: "Fetch a prepared installer OCI image, verify it and write it to a file or device",
	Long: `Fetch a prepared installer OCI image and write the raw medium to TARGET.

REF is a registry reference (ghcr.io/org/name:tag or name@sha256:...) or the
path of an OCI layout directory. Chunks are fetched with the resumable
reader, checked against their layer and raw digests, decompressed and written
in disk order. The whole stream is hashed against the image config. Then the
target is read back and compared.

TARGET must be a missing or empty file. Anything else needs an explicit flag:
--force-overwrite for a used file, --confirm-device=TARGET for a block
device. A device must also be removable and unmounted, or pass
--allow-fixed-disk.

Signature verification is on by default: a Sigstore signature bundle whose
certificate identity is the CI workflow, plus a receipt attestation that
must match the image annotation. Both are fetched as referrers of the
image digest, or as sidecar files for an OCI layout directory.`,
	Args:         cobra.ExactArgs(2),
	SilenceUsage: true,
	RunE:         runWriteMedium,
}

func init() {
	f := writeMediumCmd.Flags()
	f.Bool("insecure-skip-verify", false, "skip signature and receipt verification (prints a loud warning, never the default)")
	f.Bool("force-overwrite", false, "allow writing over a non-empty regular file")
	f.String("confirm-device", "", "repeat the block device path to confirm writing it")
	f.Bool("allow-fixed-disk", false, "allow a block device that is not removable")
	f.Bool("resume", false, "keep the existing target and skip chunks that already match")
	f.String("layout-name", "", "ref name to pick in an OCI layout with several manifests")
	f.String("trust-root", "", "Sigstore trusted root JSON for signature verification (required unless --insecure-skip-verify)")
	f.String("identity-regexp", medium.DefaultIdentityRegexp, "regexp the signature's CI workflow identity must match")
	rootCmd.AddCommand(writeMediumCmd)
}

func runWriteMedium(cmd *cobra.Command, args []string) error {
	ref, target := args[0], args[1]
	f := cmd.Flags()
	skip, _ := f.GetBool("insecure-skip-verify")
	force, _ := f.GetBool("force-overwrite")
	confirm, _ := f.GetString("confirm-device")
	fixed, _ := f.GetBool("allow-fixed-disk")
	resume, _ := f.GetBool("resume")
	name, _ := f.GetString("layout-name")

	var v medium.Verifier = medium.StubVerifier{}
	if skip {
		v = medium.InsecureSkipVerifier{}
	}

	var src medium.Source
	if st, err := os.Stat(ref); err == nil && st.IsDir() {
		if !medium.IsLayout(ref) {
			return fmt.Errorf("%s is a directory but not an OCI layout (no oci-layout file)", ref)
		}
		src = &medium.LayoutSource{Dir: ref, Name: name}
	} else {
		base, r, pin, err := medium.ParseRegistryRef(ref)
		if err != nil {
			return err
		}
		src = &medium.RegistrySource{Client: oci.NewClient(base), Ref: r, Pin: pin}
	}

	if !skip {
		cv, err := verifierForSource(f, src)
		if err != nil {
			return err
		}
		v = cv
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	// Fail on signature policy before touching the target.
	plan, err := medium.Load(ctx, src)
	if err != nil {
		return err
	}
	if err := plan.Verify(ctx, v, ref); err != nil {
		return err
	}

	t, err := medium.OpenTarget(target, medium.TargetPolicy{OverwriteFile: force, ConfirmDevice: confirm, AllowFixedDisk: fixed}, resume)
	if err != nil {
		return err
	}
	defer func() { _ = t.F.Close() }()

	last := -1
	res, err := medium.Write(ctx, src, t, medium.Options{Verifier: v, Ref: ref, Plan: plan, Resume: resume, Progress: func(done, total int64) {
		if pct := int(done * 100 / total); pct != last {
			last = pct
			fmt.Fprintf(os.Stderr, "\r%3d%% (%d / %d MiB)", pct, done>>20, total>>20)
		}
	}})
	fmt.Fprintln(os.Stderr)
	if err != nil {
		return err
	}
	fmt.Printf("wrote %d bytes to %s\nmanifest %s\nsha256 %s (read back and verified)\nchunks written %d, skipped %d\n",
		res.TotalSize, target, res.ManifestDigest, res.SHA256, res.ChunksWritten, res.ChunksSkipped)
	return nil
}
func verifierForSource(f *pflag.FlagSet, src medium.Source) (*medium.CosignVerifier, error) {
	trustRoot, _ := f.GetString("trust-root")
	identityRe, _ := f.GetString("identity-regexp")
	re, err := regexp.Compile(identityRe)
	if err != nil {
		return nil, fmt.Errorf("bad --identity-regexp: %w", err)
	}
	if trustRoot == "" {
		return nil, fmt.Errorf("signature verification needs --trust-root with the Sigstore trusted root JSON (or --insecure-skip-verify to skip it)")
	}
	raw, err := os.ReadFile(trustRoot)
	if err != nil {
		return nil, fmt.Errorf("read --trust-root: %w", err)
	}
	roots, err := root.NewTrustedRootFromJSON(raw)
	if err != nil {
		return nil, fmt.Errorf("parse --trust-root: %w", err)
	}
	cv := &medium.CosignVerifier{Roots: roots, IdentityRegexp: re, Issuer: medium.DefaultIssuer}
	switch s := src.(type) {
	case *medium.LayoutSource:
		cv.FetchSignature, cv.FetchAttestation = medium.LayoutBundles(s.Dir)
	case *medium.RegistrySource:
		cv.FetchSignature, cv.FetchAttestation = medium.RegistryBundles(s.Client, s.Ref.Repo)
	default:
		return nil, fmt.Errorf("signature verification has no bundle transport for source %T", src)
	}
	return cv, nil
}
