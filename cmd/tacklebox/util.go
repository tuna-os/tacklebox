// Package-file split of the former 1,420-line cmd/tacklebox/build.go
// (architect refactor, tuna-os/tacklebox#217). Declarations moved
// verbatim between files of the same package — no behavior change.
package main

import (
	"bufio"
	"fmt"
	"github.com/tuna-os/tacklebox/internal/runner"
	"os"
	"strings"
	"time"
)

// compressArtifact xz-compresses artifact next to itself, keeping the
// original. -f overwrites a stale .xz from a previous build.
func compressArtifact(artifact string) error {
	fmt.Printf(">>> Compressing image to %s.xz...\n", artifact)
	if err := runner.Run("xz", "-T0", "-k", "-f", artifact); err != nil {
		return fmt.Errorf("compress image %s: %w", artifact, err)
	}
	fmt.Printf(">>> Compression complete: %s.xz\n", artifact)
	return nil
}

// envTitle returns the boot-menu title for an env: the recipe's `title`
// field when set (e.g. "Bluefin (GNOME)"), otherwise the env ID, with the
// boot mode appended.

func printTimings(t map[string]time.Duration, total time.Duration) {
	// Stable column layout. Sort by descending duration so the cost centres
	// stand out without the reader having to scan the whole list.
	type row struct {
		name string
		d    time.Duration
	}
	rows := make([]row, 0, len(t))
	for k, v := range t {
		rows = append(rows, row{k, v})
	}
	// Simple insertion sort — list is tiny.
	for i := 1; i < len(rows); i++ {
		for j := i; j > 0 && rows[j-1].d < rows[j].d; j-- {
			rows[j-1], rows[j] = rows[j], rows[j-1]
		}
	}
	fmt.Println(">>> Phase timings:")
	for _, r := range rows {
		fmt.Printf("    %-24s %8s  (%4.1f%%)\n", r.name, r.d.Round(time.Millisecond), 100*float64(r.d)/float64(total))
	}
	fmt.Printf("    %-24s %8s\n", "TOTAL", total.Round(time.Millisecond))
}

// confirmDestructive prints a summary of the target and requires the user to
// type 'yes' before continuing — unless --yes is set or stdin isn't a tty
// (CI / scripts). This prevents the classic `sudo tacklebox build x /dev/sda`
// typo from nuking the wrong disk.

// confirmDestructive prints a summary of the target and requires the user to
// type 'yes' before continuing — unless --yes is set or stdin isn't a tty
// (CI / scripts). This prevents the classic `sudo tacklebox build x /dev/sda`
// typo from nuking the wrong disk.
func confirmDestructive(target string, assumeYes bool) error {
	if assumeYes {
		fmt.Printf(">>> --yes set, skipping destructive confirmation for %s\n", target)
		return nil
	}
	// Best-effort summary: lsblk if available, otherwise nothing.
	if out, err := runner.Output("lsblk", "-o", "NAME,SIZE,TYPE,MODEL,LABEL,MOUNTPOINT", target); err == nil {
		fmt.Println(string(out))
	}

	// Detect non-interactive stdin (e.g. running in CI) and refuse unless --yes.
	fi, err := os.Stdin.Stat()
	if err != nil || (fi.Mode()&os.ModeCharDevice) == 0 {
		return fmt.Errorf("refusing to destroy %s without --yes (stdin is not a terminal)", target)
	}

	fmt.Printf(">>> About to ERASE %s and write a new partition table. Type 'yes' to continue: ", target)
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil {
		return fmt.Errorf("read confirmation: %w", err)
	}
	if strings.TrimSpace(strings.ToLower(line)) != "yes" {
		return fmt.Errorf("aborted by user")
	}
	return nil
}

// runEnvs runs the install + extract + BLS pipeline for each environment.
// When parallel > 1 it runs that many environments concurrently using a
// fixed-size worker pool. BLS writes happen inside the per-env worker
// because each env produces its own entry files (no contention).
//
// CAUTION: concurrent bootc installs share /var/lib/containers and a single
// target store mount. In practice they work because they install to distinct
// stateroots (different subdirs of /target), but this is OPT-IN behaviour
// because we haven't broadly battle-tested it across image families. Stick
// with parallel=1 for production builds; use --parallel-install=N to try
// the faster path when total wall time matters more than risk.

// remoraAt returns the remora manifest for environment i, or nil when the
// manifest slice is absent or shorter than the environment list (remora
// layering is optional per environment).
