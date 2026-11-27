// labreach checks that this workstation reaches what the virtual lab needs
// outside it: what the workstation downloads, and what the Talos machines
// reach through its NAT (their DNS and NTP servers, the registries, the
// discovery service). lab/lab.sh doctor runs it; the logic lives in
// internal/lab.
//
// It writes a table to stderr and the number of endpoints the lab needs and
// does not reach to stdout.
//
// usage: go run ./hack/labreach --spec <lab.yaml> [--timeout 5s]
package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"github.com/dennisklein/platform-kit/metal/internal/lab"
)

func main() {
	spec := flag.String("spec", "", "the lab spec (lab/lab.yaml)")
	timeout := flag.Duration("timeout", lab.ProbeTimeout, "how long each check waits for an answer")
	flag.Parse()
	if *spec == "" || flag.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "usage: labreach --spec <lab.yaml> [--timeout 5s]")
		os.Exit(2)
	}
	s, err := lab.Load(*spec)
	if err != nil {
		fmt.Fprintf(os.Stderr, "labreach: %v\n", err)
		os.Exit(1)
	}
	probes, err := s.Probes()
	if err != nil {
		fmt.Fprintf(os.Stderr, "labreach: %s: %v\n", *spec, err)
		os.Exit(1)
	}
	misses := lab.FormatReach(os.Stderr, lab.Check(context.Background(), probes, *timeout))
	fmt.Println(misses)
}
