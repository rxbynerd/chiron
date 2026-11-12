// Package golden compares test output with golden files under the testdata
// directory of the package under test, and rewrites them on request.
//
// Setting CHIRON_UPDATE_GOLDEN=1 makes Assert write the built value to the
// golden file instead of comparing. Any other non-empty value fails the test,
// so a misspelt switch is never read as "off". The written diff must be
// reviewed before it is committed. CI never sets the variable.
package golden

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
)

const (
	updateEnv   = "CHIRON_UPDATE_GOLDEN"
	updateValue = "1"
)

// tb is the subset of testing.TB that compare uses, so the package's own
// tests can observe failures.
type tb interface {
	assert.TestingT
	Helper()
	Logf(format string, args ...any)
	Fatalf(format string, args ...any)
}

// Assert compares got, byte for byte, with testdata/<name> relative to the
// package under test. A missing or unreadable file stops the test. A mismatch
// fails the test without stopping it, so a caller comparing several goldens
// reports every mismatch. When CHIRON_UPDATE_GOLDEN is 1, Assert instead
// writes exactly got to the file, creating its directory, and compares
// nothing; it logs each file it creates or changes.
func Assert(t *testing.T, name string, got []byte) {
	t.Helper()
	compare(t, filepath.Join("testdata", filepath.FromSlash(name)), got)
}

func compare(t tb, path string, got []byte) {
	t.Helper()
	update, err := updateRequested()
	if err != nil {
		t.Fatalf("%v", err)
	}
	if update {
		rewrite(t, path, got)
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden %s: %v (create it with %s=%s)", path, err, updateEnv, updateValue)
	}
	assert.Equal(t, string(want), string(got),
		"golden %s differs (regenerate with %s=%s and review the diff)", path, updateEnv, updateValue)
}

// rewrite writes got to path unless the file already holds exactly got.
func rewrite(t tb, path string, got []byte) {
	t.Helper()
	if current, err := os.ReadFile(path); err == nil && bytes.Equal(current, got) {
		return
	}
	if err := write(path, got); err != nil {
		t.Fatalf("update golden %s: %v", path, err)
	}
	t.Logf("updated golden %s", path)
}

func updateRequested() (bool, error) {
	v := os.Getenv(updateEnv) //nolint:forbidigo // every package's tests share this one switch
	switch v {
	case "":
		return false, nil
	case updateValue:
		return true, nil
	default:
		return false, fmt.Errorf("%s must be %q or unset, got %q", updateEnv, updateValue, v)
	}
}

func write(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}
