package golden

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeT records failures so a test can assert that a comparison failed.
type fakeT struct {
	errors []string
	fatals []string
	logs   []string
}

func (f *fakeT) Helper() {}

func (f *fakeT) Errorf(format string, args ...any) {
	f.errors = append(f.errors, fmt.Sprintf(format, args...))
}

func (f *fakeT) Logf(format string, args ...any) {
	f.logs = append(f.logs, fmt.Sprintf(format, args...))
}

// Fatalf ends the calling goroutine, like testing.T.Fatalf.
func (f *fakeT) Fatalf(format string, args ...any) {
	f.fatals = append(f.fatals, fmt.Sprintf(format, args...))
	runtime.Goexit()
}

// compareOnFake runs compare on its own goroutine, so a Fatalf stops only
// that goroutine.
func compareOnFake(path string, got []byte) *fakeT {
	f := &fakeT{}
	done := make(chan struct{})
	go func() {
		defer close(done)
		compare(f, path, got)
	}()
	<-done
	return f
}

// inTempDir makes a testdata directory holding one golden file and runs the
// test from the directory above it.
func inTempDir(t *testing.T, name, content string) {
	t.Helper()
	must := require.New(t)
	dir := t.TempDir()
	must.NoError(os.MkdirAll(filepath.Join(dir, "testdata"), 0o755))
	must.NoError(os.WriteFile(filepath.Join(dir, "testdata", name), []byte(content), 0o644))
	t.Chdir(dir)
}

func TestAssert_Match(t *testing.T) {
	inTempDir(t, "a.txt", "alpha\n")
	t.Setenv(updateEnv, "")

	Assert(t, "a.txt", []byte("alpha\n"))
}

func TestAssert_Match_NestedName(t *testing.T) {
	must := require.New(t)
	inTempDir(t, "a.txt", "alpha\n")
	must.NoError(os.MkdirAll(filepath.Join("testdata", "pages"), 0o755))
	must.NoError(os.WriteFile(filepath.Join("testdata", "pages", "p.golden.txt"), []byte("page\n"), 0o644))
	t.Setenv(updateEnv, "")

	Assert(t, "pages/p.golden.txt", []byte("page\n"))
}

func TestCompare_Mismatch_FailsWithoutStoppingAndNamesFile(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	inTempDir(t, "a.txt", "alpha\n")
	t.Setenv(updateEnv, "")
	path := filepath.Join("testdata", "a.txt")

	f := compareOnFake(path, []byte("beta\n"))

	check.Empty(f.fatals, "a mismatch must not stop the test")
	must.Len(f.errors, 1)
	check.Contains(f.errors[0], path)
	check.Contains(f.errors[0], updateEnv+"="+updateValue)
}

func TestCompare_Mismatch_IsExactAboutWhitespace(t *testing.T) {
	must := require.New(t)
	inTempDir(t, "a.txt", "alpha\n")
	t.Setenv(updateEnv, "")

	f := compareOnFake(filepath.Join("testdata", "a.txt"), []byte("alpha"))

	must.Len(f.errors, 1, "a missing trailing newline is a mismatch")
}

func TestCompare_MissingFile_StopsTheTest(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	inTempDir(t, "a.txt", "alpha\n")
	t.Setenv(updateEnv, "")
	path := filepath.Join("testdata", "absent.txt")

	f := compareOnFake(path, []byte("alpha\n"))

	must.Len(f.fatals, 1)
	check.Contains(f.fatals[0], path)
	check.Contains(f.fatals[0], updateEnv+"="+updateValue)
	check.Empty(f.errors)
}

func TestAssert_Update_WritesExactBytesAndCreatesDirectory(t *testing.T) {
	for _, tt := range []struct {
		name  string
		file  string
		bytes []byte
	}{
		{name: "creates the file and its directory", file: "nested/dir/new.txt", bytes: []byte("fresh\n")},
		{name: "keeps a missing trailing newline", file: "no-newline.txt", bytes: []byte("fresh")},
		{name: "writes binary bytes unchanged", file: "bin.dat", bytes: []byte{0x00, 0xff, '\r', '\n'}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			must := require.New(t)
			inTempDir(t, "other.txt", "unrelated\n")
			t.Setenv(updateEnv, updateValue)

			Assert(t, tt.file, tt.bytes)

			written, err := os.ReadFile(filepath.Join("testdata", filepath.FromSlash(tt.file)))
			must.NoError(err)
			must.Equal(tt.bytes, written)
		})
	}
}

func TestAssert_Update_ReplacesAnExistingFile(t *testing.T) {
	must := require.New(t)
	inTempDir(t, "a.txt", "old\n")
	t.Setenv(updateEnv, updateValue)

	Assert(t, "a.txt", []byte("new\n"))

	t.Setenv(updateEnv, "")
	Assert(t, "a.txt", []byte("new\n"))
	written, err := os.ReadFile(filepath.Join("testdata", "a.txt"))
	must.NoError(err)
	must.Equal("new\n", string(written))
}

func TestCompare_Update_ReportsAnUnwritableTarget(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	inTempDir(t, "a.txt", "alpha\n")
	t.Setenv(updateEnv, updateValue)
	// A regular file where the directory should be makes MkdirAll fail.
	must.NoError(os.WriteFile(filepath.Join("testdata", "blocked"), nil, 0o644))
	path := filepath.Join("testdata", "blocked", "x.txt")

	f := compareOnFake(path, []byte("x"))

	must.Len(f.fatals, 1)
	check.Contains(f.fatals[0], path)
}

func TestCompare_RejectsAnyOtherNonEmptyValue(t *testing.T) {
	for _, tt := range []struct {
		name  string
		value string
	}{
		{name: "zero", value: "0"},
		{name: "true", value: "true"},
		{name: "yes", value: "yes"},
		{name: "another number", value: "2"},
		{name: "padded one", value: " 1"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			must := require.New(t)
			check := assert.New(t)
			inTempDir(t, "a.txt", "alpha\n")
			t.Setenv(updateEnv, tt.value)

			f := compareOnFake(filepath.Join("testdata", "a.txt"), []byte("beta\n"))

			must.Len(f.fatals, 1)
			check.Contains(f.fatals[0], updateEnv)
			check.Empty(f.errors)
			content, err := os.ReadFile(filepath.Join("testdata", "a.txt"))
			must.NoError(err)
			check.Equal("alpha\n", string(content), "a rejected value must not rewrite the file")
		})
	}
}

func TestCompare_Update_LogsOnlyFilesItChanges(t *testing.T) {
	for _, tt := range []struct {
		name     string
		file     string
		existing string
		got      string
		logged   bool
	}{
		{name: "creates a file", file: "new.txt", got: "fresh\n", logged: true},
		{name: "changes a file", file: "a.txt", existing: "old\n", got: "new\n", logged: true},
		{name: "leaves an identical file alone", file: "a.txt", existing: "same\n", got: "same\n", logged: false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			must := require.New(t)
			check := assert.New(t)
			inTempDir(t, "a.txt", tt.existing)
			t.Setenv(updateEnv, updateValue)
			path := filepath.Join("testdata", tt.file)

			f := compareOnFake(path, []byte(tt.got))

			check.Empty(f.fatals)
			if !tt.logged {
				check.Empty(f.logs)
				return
			}
			must.Len(f.logs, 1)
			check.Contains(f.logs[0], path)
		})
	}
}
