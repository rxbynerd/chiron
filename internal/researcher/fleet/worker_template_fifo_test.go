//go:build unix

package fleet

import (
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestLoadReportTemplateRejectsFIFO: a --template path that resolves to a
// FIFO with no writer attached is rejected promptly rather than blocking on
// open.
func TestLoadReportTemplateRejectsFIFO(t *testing.T) {
	path := filepath.Join(t.TempDir(), "format.fifo")
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Fatalf("Mkfifo: %v", err)
	}

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
		if r.err == nil || !strings.Contains(r.err.Error(), "not a regular file") {
			t.Fatalf("LoadReportTemplate = %v, %v, want an error containing %q", r.rt, r.err, "not a regular file")
		}
		if r.rt != nil {
			t.Errorf("LoadReportTemplate returned a template alongside its error")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("LoadReportTemplate blocked on a FIFO with no writer")
	}
}
