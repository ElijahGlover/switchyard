// Command mkmmdb builds an MMDB file from CSV. See package mmdbcsv for the
// format.
//
//	mkmmdb examples/netmap.csv examples/netmap.mmdb
package main

import (
	"bytes"
	"fmt"
	"os"

	"github.com/ElijahGlover/switchyard/internal/mmdbcsv"
)

func main() {
	if len(os.Args) != 3 {
		fmt.Fprintln(os.Stderr, "usage: mkmmdb in.csv out.mmdb")
		os.Exit(2)
	}
	if err := run(os.Args[1], os.Args[2]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(in, out string) error {
	f, err := os.Open(in)
	if err != nil {
		return err
	}
	defer f.Close()
	// Build in memory so a bad CSV never leaves a truncated database behind.
	var buf bytes.Buffer
	if err := mmdbcsv.Build(f, &buf); err != nil {
		return fmt.Errorf("%s: %w", in, err)
	}
	return os.WriteFile(out, buf.Bytes(), 0o644)
}
