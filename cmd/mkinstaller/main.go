// Command mkinstaller builds a small synthetic installer OCI image as an OCI
// layout directory, for offline tests of `tacklebox write-medium`.
//
//	mkinstaller -o /tmp/layout -size 6MiB -chunk 1MiB
package main

import (
	"bytes"
	"flag"
	"fmt"
	"os"

	"github.com/tuna-os/tacklebox/internal/medium"
)

func main() {
	out := flag.String("o", "", "output OCI layout directory (required)")
	size := flag.Int("size", 6<<20, "raw medium size in bytes")
	chunk := flag.Int64("chunk", 1<<20, "raw bytes per chunk")
	seed := flag.Int64("seed", 1, "seed for the synthetic data")
	flag.Parse()
	if *out == "" {
		flag.Usage()
		os.Exit(2)
	}
	d, err := medium.BuildLayout(*out, bytes.NewReader(medium.SyntheticMedium(*size, *seed)), medium.BuildOptions{
		ChunkSize: *chunk, RefName: "synthetic",
		Annotations: map[string]string{"org.tunaos.installer.synthetic": "true"},
		BootModes:   []string{"uefi"},
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "mkinstaller:", err)
		os.Exit(1)
	}
	fmt.Println(d)
}
