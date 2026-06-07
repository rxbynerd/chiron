package sink

import (
	"context"

	"github.com/rxbynerd/chiron/internal/types"
)

// Multi fans the report out to several sinks in order — the CLI binds
// it to compose --out with -o (file plus stdout-json, say). With no
// sinks it discards the report, which is exactly -o none without --out:
// the run's events and exit code still carry the outcome.
func Multi(sinks ...ReportSink) ReportSink {
	return multi(sinks)
}

type multi []ReportSink

var _ ReportSink = multi(nil)

// Write implements ReportSink, stopping at the first failing sink: a
// report that did not land where it was asked to is a failed emit, not
// a warning.
func (m multi) Write(ctx context.Context, result *types.RunResult) error {
	for _, s := range m {
		if err := s.Write(ctx, result); err != nil {
			return err
		}
	}
	return nil
}
