// Command chiron is the Equestrianism suite's researcher: it drives a
// long-running research agent end-to-end (start, await, retrieve, format)
// and emits a cited Markdown report.
package main

import (
	"fmt"
	"os"

	"github.com/rxbynerd/chiron/internal/cli"
)

func main() {
	if err := cli.NewRootCommand().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "chiron:", err)
		os.Exit(1)
	}
}
