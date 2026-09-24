// Package version holds the chiron build version. Both the CLI's
// --version flag and the research packet's provenance comment read
// the same variable, so a build reports its version identically
// everywhere.
package version

// Version is set at build time via
// -X github.com/rxbynerd/chiron/internal/version.Version=<value>
// (see Justfile); a plain `go build` leaves it at "dev".
var Version = "dev"
