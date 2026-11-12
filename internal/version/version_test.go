package version

import (
	"runtime/debug"
	"testing"

	"github.com/stretchr/testify/require"
)

const fullRevision = "2eec1aa95743d7ab5ab66083f5cbb40973c0f751"

func TestResolve_FallbackOrder(t *testing.T) {
	tests := []struct {
		name    string
		stamped string
		info    *debug.BuildInfo
		want    string
	}{
		{
			name:    "stamped version wins over build information",
			stamped: "v1.2.3-4-gabcdef0",
			info:    buildInfo("v0.0.0-20261112011634-2eec1aa95743", fullRevision, "false"),
			want:    "v1.2.3-4-gabcdef0",
		},
		{
			name:    "stamped version wins without build information",
			stamped: "v1.2.3",
			want:    "v1.2.3",
		},
		{
			name:    "empty stamped version is unset",
			stamped: "",
			info:    buildInfo("v0.0.0-20261112011634-2eec1aa95743", fullRevision, "false"),
			want:    "2eec1aa95743",
		},
		{
			name:    "empty stamped version without build information",
			stamped: "",
			want:    "dev",
		},
		{
			name:    "revision is shortened",
			stamped: "dev",
			info:    buildInfo("v0.0.0-20261112011634-2eec1aa95743", fullRevision, "false"),
			want:    "2eec1aa95743",
		},
		{
			name:    "modified tree is marked dirty",
			stamped: "dev",
			info:    buildInfo("v0.0.0-20261112011634-2eec1aa95743+dirty", fullRevision, "true"),
			want:    "2eec1aa95743-dirty",
		},
		{
			name:    "revision wins over module version",
			stamped: "dev",
			info:    buildInfo("v1.0.0", fullRevision, "false"),
			want:    "2eec1aa95743",
		},
		{
			name:    "short revision is kept whole",
			stamped: "dev",
			info:    buildInfo("(devel)", "abc123", "false"),
			want:    "abc123",
		},
		{
			name:    "module version when no revision is recorded",
			stamped: "dev",
			info:    buildInfo("v1.4.0", "", ""),
			want:    "v1.4.0",
		},
		{
			name:    "devel module version is ignored",
			stamped: "dev",
			info:    buildInfo("(devel)", "", ""),
			want:    "dev",
		},
		{
			name:    "empty module version is ignored",
			stamped: "dev",
			info:    buildInfo("", "", ""),
			want:    "dev",
		},
		{
			name:    "no build information",
			stamped: "dev",
			want:    "dev",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			must := require.New(t)

			must.Equal(tt.want, resolve(tt.stamped, tt.info))
		})
	}
}

func TestString_PrefersStampedVersion(t *testing.T) {
	must := require.New(t)

	old := Version
	Version = "v9.9.9-test"
	defer func() { Version = old }()

	must.Equal("v9.9.9-test", String())
}

// buildInfo returns build information with the given main module version
// and, when revision is non-empty, the VCS settings Go embeds.
func buildInfo(moduleVersion, revision, modified string) *debug.BuildInfo {
	info := &debug.BuildInfo{Main: debug.Module{Version: moduleVersion}}
	if revision != "" {
		info.Settings = []debug.BuildSetting{
			{Key: "vcs", Value: "git"},
			{Key: "vcs.revision", Value: revision},
			{Key: "vcs.modified", Value: modified},
		}
	}
	return info
}
