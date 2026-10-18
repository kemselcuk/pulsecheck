// Command pulsecheck is the main entry point of the PulseCheck service.
//
// Phase 0 keeps this scaffold minimal: it only reports that the binary runs.
// Configuration loading, the checker loop, storage, and the HTTP server are
// wired in later phases.
package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "pulsecheck: skeleton (phase 0) — config not wired yet")
	os.Exit(0)
}
