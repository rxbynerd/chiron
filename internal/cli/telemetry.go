package cli

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
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

// otlpHeaderEnv lists the variables the OTLP exporter reads request
// headers from.
var otlpHeaderEnv = []string{"OTEL_EXPORTER_OTLP_HEADERS", "OTEL_EXPORTER_OTLP_TRACES_HEADERS"}

// otlpEnv reads an OTLP variable as the exporter does: trimmed, with a
// blank value meaning unset.
func otlpEnv(name string) string {
	return strings.TrimSpace(os.Getenv(name))
}

// checkNoOTLPHeaderEnv refuses an explicit OTLP endpoint while a header
// variable is set: those headers are credentials for the collector the
// environment names, and the exporter would send them to any endpoint.
func checkNoOTLPHeaderEnv() error {
	for _, name := range otlpHeaderEnv {
		if otlpEnv(name) != "" {
			return fmt.Errorf("telemetry.otlp_endpoint: %s is set; environment headers go only to the collector the environment names (unset it, or name that collector with OTEL_EXPORTER_OTLP_ENDPOINT instead)", name)
		}
	}
	return nil
}

// checkOTLPHeaderEnv refuses a header variable the OTLP exporter cannot
// parse, applying the exporter's grammar: comma-separated name=value
// entries with a token name and a URL-escaped value. The exporter logs a
// malformed entry raw to the process stderr, and the value is usually a
// credential, so the error names the variable and withholds its value.
func checkOTLPHeaderEnv() error {
	for _, name := range otlpHeaderEnv {
		for _, entry := range strings.Split(otlpEnv(name), ",") {
			if strings.TrimSpace(entry) == "" {
				continue
			}
			key, value, found := strings.Cut(entry, "=")
			if !found || !validHeaderName(strings.TrimSpace(key)) {
				return malformedHeaderEnv(name)
			}
			if _, err := url.PathUnescape(value); err != nil {
				return malformedHeaderEnv(name)
			}
		}
	}
	return nil
}

func malformedHeaderEnv(name string) error {
	return fmt.Errorf("%s: malformed header list (value withheld); each entry must be name=value with a valid header name and a URL-escaped value", name)
}

// validHeaderName reports whether name is an HTTP token, the exporter's
// rule for a header name.
func validHeaderName(name string) bool {
	if name == "" {
		return false
	}
	for _, c := range name {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case strings.ContainsRune("!#$%&'*+-.^_`|~", c):
		default:
			return false
		}
	}
	return true
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
