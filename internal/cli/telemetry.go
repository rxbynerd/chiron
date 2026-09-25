package cli

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"

	"go.opentelemetry.io/otel"

	"github.com/rxbynerd/chiron/internal/config"
	"github.com/rxbynerd/chiron/internal/secret"
)

// langfuseHeaders resolves both Langfuse key references into the headers
// Langfuse's OTLP endpoint expects: HTTP Basic authentication with the
// public key as the user, and ingestion version 4 so spans display in real
// time. A resolution failure names the field, never a value.
func langfuseHeaders(ctx context.Context, tc config.TelemetryConfig) (map[string]string, error) {
	public, err := secret.Default().Resolve(ctx, tc.LangfusePublicKeyRef)
	if err != nil {
		return nil, fmt.Errorf("telemetry.langfuse_public_key_ref: %w", err)
	}
	private, err := secret.Default().Resolve(ctx, tc.LangfuseSecretKeyRef)
	if err != nil {
		return nil, fmt.Errorf("telemetry.langfuse_secret_key_ref: %w", err)
	}
	return map[string]string{
		"Authorization":                "Basic " + base64.StdEncoding.EncodeToString([]byte(public+":"+private)),
		"x-langfuse-ingestion-version": "4",
	}, nil
}

const (
	// maxOTelErrorBytes bounds one reported SDK error: a failed export
	// quotes the collector's response body, which may run to megabytes.
	maxOTelErrorBytes = 1 << 10
	// maxOTelErrorScanBytes bounds the text scrubbed per report. It is well
	// past maxOTelErrorBytes, so a credential straddling that cut is still
	// whole when Scrub sees it.
	maxOTelErrorScanBytes = 16 << 10
)

// One run owns the error route at a time; a later routeOTelErrors takes it
// over.
var (
	otelErrors        = &otelErrorHandler{}
	installOTelErrors sync.Once
)

// routeOTelErrors sends the OTel SDK's process-wide error reports to a
// scrubbing logger on stderr until the returned func runs. The SDK's
// default handler prints them unscrubbed, and a collector's error response
// can echo request headers.
func routeOTelErrors(stderr io.Writer) (restore func()) {
	installOTelErrors.Do(func() { otel.SetErrorHandler(otelErrors) })
	logger := slog.New(secret.NewScrubHandler(slog.NewTextHandler(stderr, nil)))
	otelErrors.logger.Store(logger)
	return func() { otelErrors.logger.CompareAndSwap(logger, nil) }
}

// otelErrorHandler reports SDK errors, chiefly failed exports, scrubbed and
// bounded, to the bound run's stderr. With no run bound they are dropped.
type otelErrorHandler struct {
	logger atomic.Pointer[slog.Logger]
}

// Handle implements otel.ErrorHandler.
func (h *otelErrorHandler) Handle(err error) {
	logger := h.logger.Load()
	if err == nil || logger == nil {
		return
	}
	raw := err.Error()
	truncated := len(raw) > maxOTelErrorScanBytes
	if truncated {
		raw = raw[:maxOTelErrorScanBytes]
	}
	// Scrub before the final cut: cutting first could shorten a credential
	// below its pattern's minimum length.
	msg := secret.Scrub(raw)
	if len(msg) > maxOTelErrorBytes {
		msg = msg[:maxOTelErrorBytes]
		truncated = true
	}
	if truncated {
		msg = strings.ToValidUTF8(msg, "") + " [truncated]"
	}
	logger.Warn("telemetry error", "err", msg)
}
