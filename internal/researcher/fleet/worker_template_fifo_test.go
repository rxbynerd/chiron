//go:build unix

package fleet

import (
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestLoadReportTemplate_RejectsFIFO_WithoutWriter: a --template path that
// resolves to a FIFO with no writer attached is rejected promptly rather than
// blocking on open.
func TestLoadReportTemplate_RejectsFIFO_WithoutWriter(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)

	path := filepath.Join(t.TempDir(), "format.fifo")
	must.NoError(syscall.Mkfifo(path, 0o600), "Mkfifo")

	type result struct {
		rt  *ReportTemplate
		err error
	}
	done := make(chan result, 1)
	go func() {
		rt, err := LoadReportTemplate(path)
		done <- result{rt, err}
	}()

	select {
	case r := <-done:
		must.ErrorContains(r.err, "not a regular file")
		check.Nil(r.rt, "LoadReportTemplate returned a template alongside its error")
	case <-time.After(5 * time.Second):
		must.Fail("LoadReportTemplate blocked on a FIFO with no writer")
	}
}
