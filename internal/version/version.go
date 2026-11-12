// Package version names the chiron build. Both the CLI's --version flag
// and the research packet's provenance comment call String, so a build
// reports its version identically everywhere.
package version

import "runtime/debug"

// Version is set at build time via
// -X github.com/rxbynerd/chiron/internal/version.Version=<value>
// (see Justfile); a plain `go build` leaves it at "dev". It must stay a
// string variable with a constant initialiser for -X to apply; read it
// through String.
var Version = "dev"

// unstamped is the value of Version in a build that was not stamped.
const unstamped = "dev"

// revisionLength is the number of commit hash characters String reports,
// the same length Go uses in a module pseudo-version.
const revisionLength = 12

// String returns the build's version: the stamped Version when set,
// otherwise the commit recorded in the embedded build information (with
// "-dirty" for a modified tree), otherwise the main module version, and
// "dev" when none of them is available.
func String() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		info = nil
	}
	return resolve(Version, info)
}

// resolve applies String's precedence to a stamped value and the build
// information, which may be nil.
func resolve(stamped string, info *debug.BuildInfo) string {
	if stamped != unstamped {
		return stamped
	}
	if info == nil {
		return unstamped
	}

	var revision string
	var modified bool
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			revision = s.Value
		case "vcs.modified":
			modified = s.Value == "true"
		}
	}
	if revision != "" {
		if len(revision) > revisionLength {
			revision = revision[:revisionLength]
		}
		if modified {
			revision += "-dirty"
		}
		return revision
	}

	if v := info.Main.Version; v != "" && v != "(devel)" {
		return v
	}
	return unstamped
}
