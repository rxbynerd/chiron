package secret

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A key file readable beyond its owner still resolves, so a
// mis-permissioned but valid file does not block a run. The operator is
// warned instead.
func TestFile_Resolve_WarnsOnOpenPermissions(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	ctx := context.Background()

	open := filepath.Join(t.TempDir(), "key")
	must.NoError(os.WriteFile(open, []byte("k3y"), 0o644))
	got, err := File{}.Resolve(ctx, "secret://file"+open)
	must.NoError(err, "the warning must not block resolution")
	must.Equal("k3y", got)
	check.Contains(buf.String(), "tighten to 0600", "no permission warning logged for a 0644 key file")

	buf.Reset()
	tight := filepath.Join(t.TempDir(), "key")
	must.NoError(os.WriteFile(tight, []byte("k3y"), 0o600))
	_, err = File{}.Resolve(ctx, "secret://file"+tight)
	must.NoError(err)
	check.Empty(buf.String(), "a 0600 key file must not warn")
}

func TestParseRef_AcceptsValidAndRejectsInvalid(t *testing.T) {
	tests := []struct {
		name    string
		ref     string
		want    Ref
		wantErr bool
	}{
		{"bare name is env", "secret://GEMINI_API_KEY", Ref{BackendEnv, "GEMINI_API_KEY"}, false},
		{"explicit env", "secret://env/GEMINI_API_KEY", Ref{BackendEnv, "GEMINI_API_KEY"}, false},
		{"file with implied leading slash", "secret://file/etc/chiron/key", Ref{BackendFile, "/etc/chiron/key"}, false},
		{"file with explicit leading slash", "secret://file//etc/chiron/key", Ref{BackendFile, "/etc/chiron/key"}, false},
		{"bare segment named file is env", "secret://file", Ref{BackendEnv, "file"}, false},
		{"literal value rejected", "AIzaNotARealKeyJustALiteral", Ref{}, true},
		{"empty reference", "secret://", Ref{}, true},
		{"empty env name", "secret://env/", Ref{}, true},
		{"env name with slash", "secret://env/FOO/BAR", Ref{}, true},
		{"empty file path", "secret://file/", Ref{}, true},
		{"slashes-only file path", "secret://file///", Ref{}, true},
		{"unknown backend", "secret://gcp/projects/x/secrets/y", Ref{}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			must := require.New(t)
			got, err := ParseRef(tt.ref)
			if tt.wantErr {
				must.Error(err)
			} else {
				must.NoError(err)
			}
			must.Equal(tt.want, got)
		})
	}
}

// A rejected literal must not be echoed into the error: errors end up in
// logs, and the literal may be a credential.
func TestParseRefLiteralNotEchoed(t *testing.T) {
	must := require.New(t)
	const literal = "totally-secret-literal-value-12345"
	_, err := ParseRef(literal)
	must.Error(err, "ParseRef accepted a literal value")
	must.NotContains(err.Error(), literal, "error echoes the literal value")
}

func TestEnv_Resolve_ReadsVariableAndRejectsOthers(t *testing.T) {
	ctx := context.Background()
	t.Setenv("CHIRON_TEST_SECRET", "hunter2")
	t.Setenv("CHIRON_TEST_EMPTY", "")

	for _, tt := range []struct {
		name string
		ref  string
		want string
	}{
		{"bare name", "secret://CHIRON_TEST_SECRET", "hunter2"},
		{"explicit env", "secret://env/CHIRON_TEST_SECRET", "hunter2"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			must := require.New(t)
			got, err := Env{}.Resolve(ctx, tt.ref)
			must.NoError(err)
			must.Equal(tt.want, got)
		})
	}

	for _, tt := range []struct {
		name string
		ref  string
	}{
		{"rejects unset variable", "secret://CHIRON_TEST_UNSET"},
		{"rejects empty variable", "secret://CHIRON_TEST_EMPTY"},
		{"rejects file reference", "secret://file/etc/key"},
		{"rejects literal value", "literal-value"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			must := require.New(t)
			_, err := Env{}.Resolve(ctx, tt.ref)
			must.Error(err)
		})
	}
}

func TestFile_Resolve_ReadsKeyFileAndRejectsOthers(t *testing.T) {
	must := require.New(t)
	ctx := context.Background()
	dir := t.TempDir()

	path := filepath.Join(dir, "key")
	must.NoError(os.WriteFile(path, []byte("hunter2\n"), 0o600))
	got, err := File{}.Resolve(ctx, "secret://file"+path)
	must.NoError(err)
	must.Equal("hunter2", got, "the trailing newline should be trimmed")

	empty := filepath.Join(dir, "empty")
	must.NoError(os.WriteFile(empty, []byte("\n"), 0o600))

	for _, tt := range []struct {
		name string
		ref  string
	}{
		{"rejects empty file", "secret://file" + empty},
		{"rejects missing file", "secret://file" + filepath.Join(dir, "absent")},
		{"rejects env reference", "secret://ENV_NAME"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			must := require.New(t)
			_, err := File{}.Resolve(ctx, tt.ref)
			must.Error(err)
		})
	}
}

func TestDefaultResolver_Dispatch(t *testing.T) {
	must := require.New(t)
	ctx := context.Background()
	r := Default()

	t.Setenv("CHIRON_TEST_SECRET", "from-env")
	got, err := r.Resolve(ctx, "secret://CHIRON_TEST_SECRET")
	must.NoError(err)
	must.Equal("from-env", got)

	path := filepath.Join(t.TempDir(), "key")
	must.NoError(os.WriteFile(path, []byte("from-file"), 0o600))
	got, err = r.Resolve(ctx, "secret://file"+path)
	must.NoError(err)
	must.Equal("from-file", got)

	_, err = r.Resolve(ctx, "not-a-reference")
	must.Error(err)
}
