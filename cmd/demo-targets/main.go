// Command demo-targets is a placeholder for a local demo HTTP server.
//
// It will eventually serve fixed endpoints (/ok, /broken, /slow) so the
// checker can be demonstrated offline. Phase 0 only proves the binary builds
// and exits cleanly.
package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "demo-targets: skeleton (phase 0) — routes not wired yet")
	os.Exit(0)
}
