package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
	"github.com/tuna-os/tacklebox/internal/verify"
)

var verifyCmd = &cobra.Command{
	Use:   "verify PATH",
	Short: "Sanity-check a tacklebox-built image or ISO",
	Long: `Verify a built artifact is well-formed.

Auto-detects the artifact type:
  - .iso suffix → ISO9660 verification (no sudo required; uses xorriso)
  - everything else → raw block image (uses sudo + losetup + mount)

Checks performed (both types):
  1. Bootloader entries directory exists and is non-empty.
  2. Each BLS entry's linux= and initrd= paths resolve to real files.
  3. Per-env content is distinct — catches the bootc cross-env collision
     observed on 2026-05-11 where two stateroots ended up with the same
     ostree commit hash. For ISO targets, hashes the per-env squashfs.
     For block targets, scans ostree/composefs deployment hashes.

Exits 0 on success, 1 on any check failure (with a list of failures).
`,
	Args:         cobra.ExactArgs(1),
	SilenceUsage: true,
	RunE:         runVerify,
}

func init() {
	rootCmd.AddCommand(verifyCmd)
}

func runVerify(cmd *cobra.Command, args []string) error {
	path := args[0]
	if _, err := os.Stat(path); err != nil {
		return fmt.Errorf("stat %s: %w", path, err)
	}

	var results []verify.CheckResult
	if verify.IsISO(path) {
		fmt.Printf(">>> Verifying ISO: %s\n", path)
		results = verify.VerifyISO(path)
	} else {
		fmt.Printf(">>> Verifying block image: %s\n", path)
		results = verify.VerifyBlock(path)
	}

	var failed int
	for _, r := range results {
		fmt.Println(r)
		if r.Err != nil {
			failed++
		}
	}
	if failed > 0 {
		return fmt.Errorf("%d/%d checks failed", failed, len(results))
	}
	fmt.Printf(">>> All %d checks passed\n", len(results))
	return nil
}
