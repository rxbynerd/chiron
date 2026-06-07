// Command chiron is the Equestrianism suite's researcher: it drives a
// long-running research agent end-to-end (start, await, retrieve, format)
// and emits a cited Markdown report.
package main

import (
	"os"

	"github.com/rxbynerd/chiron/internal/cli"
)

func main() {
	os.Exit(cli.Execute())
}
