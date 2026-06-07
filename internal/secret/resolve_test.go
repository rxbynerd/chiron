package secret

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestFileResolveWarnsOnOpenPermissions pins C1-SEC-5: a key file
// readable beyond its owner resolves — a mis-permissioned but valid
// file must not block a run — but warns the operator.
func TestFileResolveWarnsOnOpenPermissions(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	ctx := context.Background()

	open := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(open, []byte("k3y"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := File{}.Resolve(ctx, "secret://file"+open)
	if err != nil || got != "k3y" {
		t.Fatalf("Resolve = %q, %v; the warning must not block resolution", got, err)
	}
	if !strings.Contains(buf.String(), "tighten to 0600") {
		t.Errorf("no permission warning logged for a 0644 key file: %q", buf.String())
	}

	buf.Reset()
	tight := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(tight, []byte("k3y"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := (File{}).Resolve(ctx, "secret://file"+tight); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if buf.Len() != 0 {
		t.Errorf("a 0600 key file must not warn: %q", buf.String())
	}
}

func TestParseRef(t *testing.T) {
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
			got, err := ParseRef(tt.ref)
			if (err != nil) != tt.wantErr {
				t.Fatalf("ParseRef(%q) error = %v, wantErr %v", tt.ref, err, tt.wantErr)
			}
			if got != tt.want {
				t.Fatalf("ParseRef(%q) = %+v, want %+v", tt.ref, got, tt.want)
			}
		})
	}
}

// A rejected literal must not be echoed into the error: errors end up in
// logs, and the literal may be a credential.
func TestParseRefLiteralNotEchoed(t *testing.T) {
	const literal = "totally-secret-literal-value-12345"
	_, err := ParseRef(literal)
	if err == nil {
		t.Fatal("ParseRef accepted a literal value")
	}
	if strings.Contains(err.Error(), literal) {
		t.Fatalf("error echoes the literal value: %v", err)
	}
}

func TestEnvResolve(t *testing.T) {
	ctx := context.Background()

	t.Setenv("CHIRON_TEST_SECRET", "hunter2")
	got, err := Env{}.Resolve(ctx, "secret://CHIRON_TEST_SECRET")
	if err != nil || got != "hunter2" {
		t.Fatalf("Resolve bare = %q, %v; want hunter2, nil", got, err)
	}
	got, err = Env{}.Resolve(ctx, "secret://env/CHIRON_TEST_SECRET")
	if err != nil || got != "hunter2" {
		t.Fatalf("Resolve explicit = %q, %v; want hunter2, nil", got, err)
	}

	if _, err := (Env{}).Resolve(ctx, "secret://CHIRON_TEST_UNSET"); err == nil {
		t.Fatal("Resolve of unset variable succeeded; want error")
	}
	t.Setenv("CHIRON_TEST_EMPTY", "")
	if _, err := (Env{}).Resolve(ctx, "secret://CHIRON_TEST_EMPTY"); err == nil {
		t.Fatal("Resolve of empty variable succeeded; want error")
	}
	if _, err := (Env{}).Resolve(ctx, "secret://file/etc/key"); err == nil {
		t.Fatal("env resolver accepted a file reference; want error")
	}
	if _, err := (Env{}).Resolve(ctx, "literal-value"); err == nil {
		t.Fatal("env resolver accepted a literal; want error")
	}
}

func TestFileResolve(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	path := filepath.Join(dir, "key")
	if err := os.WriteFile(path, []byte("hunter2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := File{}.Resolve(ctx, "secret://file"+path)
	if err != nil || got != "hunter2" {
		t.Fatalf("Resolve = %q, %v; want hunter2 with trailing newline trimmed", got, err)
	}

	empty := filepath.Join(dir, "empty")
	if err := os.WriteFile(empty, []byte("\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := (File{}).Resolve(ctx, "secret://file"+empty); err == nil {
		t.Fatal("Resolve of empty file succeeded; want error")
	}

	if _, err := (File{}).Resolve(ctx, "secret://file"+filepath.Join(dir, "absent")); err == nil {
		t.Fatal("Resolve of missing file succeeded; want error")
	}
	if _, err := (File{}).Resolve(ctx, "secret://ENV_NAME"); err == nil {
		t.Fatal("file resolver accepted an env reference; want error")
	}
}

func TestDefaultResolverDispatch(t *testing.T) {
	ctx := context.Background()
	r := Default()

	t.Setenv("CHIRON_TEST_SECRET", "from-env")
	got, err := r.Resolve(ctx, "secret://CHIRON_TEST_SECRET")
	if err != nil || got != "from-env" {
		t.Fatalf("env dispatch = %q, %v; want from-env, nil", got, err)
	}

	path := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(path, []byte("from-file"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err = r.Resolve(ctx, "secret://file"+path)
	if err != nil || got != "from-file" {
		t.Fatalf("file dispatch = %q, %v; want from-file, nil", got, err)
	}

	if _, err := r.Resolve(ctx, "not-a-reference"); err == nil {
		t.Fatal("default resolver accepted a literal; want error")
	}
}
