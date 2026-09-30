// Command priors is the memory-layer CLI: scoped, gated stores of facts
// that agents read at session start and write through a reviewed path.
package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "usage: priors [add|list|show|search|lint|index]")
	os.Exit(1)
}
