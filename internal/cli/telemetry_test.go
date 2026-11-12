package cli

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/chiron/internal/config"
	"github.com/rxbynerd/chiron/internal/researcher/fleet/model"
	"github.com/rxbynerd/chiron/internal/researcher/fleet/model/modeltest"
	"github.com/rxbynerd/chiron/internal/researcher/fleet/search"
	"github.com/rxbynerd/chiron/internal/researcher/fleet/search/searchtest"
	"github.com/rxbynerd/chiron/internal/trace"
)

// Credentials shaped like real Langfuse and model keys, so the leak
// assertions exercise the scrubber's real patterns.
const (
	testLangfusePublic = "pk-lf-1a2b3c4d-5e6f-7a8b-9c0d-1e2f3a4b5c6d"
	testLangfuseSecret = "sk-lf-6d5c4b3a-2f1e-0d9c-8b7a-6f5e4d3c2b1a"
	testModelKey       = "sk-proj-Q3vT8mZ2xL5nR9wK4bY7hJ1cF6pD0sA2gE8uN3oV5iX7tM4"
)

// otlpCollector records the export requests a fake OTLP/HTTP collector
// receives. Gzip bodies are stored decompressed.
type otlpCollector struct {
	mu       sync.Mutex
	requests []otlpRequest
}

type otlpRequest struct {
	method string
	path   string
	header http.Header
	body   []byte
}

// handler records each request and answers with status and reply.
func (c *otlpCollector) handler(t *testing.T, status int, reply string) http.HandlerFunc {
	check := assert.New(t)
	return func(w http.ResponseWriter, r *http.Request) {
		var body io.Reader = r.Body
		if r.Header.Get("Content-Encoding") == "gzip" {
			zr, err := gzip.NewReader(r.Body)
			if err != nil {
				check.NoError(err, "collector: gzip body")
				http.Error(w, "bad gzip body", http.StatusBadRequest)
				return
			}
			defer zr.Close()
			body = zr
		}
		raw, err := io.ReadAll(body)
		check.NoError(err, "collector: reading body")
		c.mu.Lock()
		c.requests = append(c.requests, otlpRequest{method: r.Method, path: r.URL.Path, header: r.Header.Clone(), body: raw})
		c.mu.Unlock()
		w.WriteHeader(status)
		_, _ = io.WriteString(w, reply)
	}
}

func (c *otlpCollector) recorded() []otlpRequest {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.requests)
}

// protoKey is the protobuf encoding of an OTLP KeyValue's key field (field
// 1, length-delimited) for a key under 128 bytes, so a match pins an
// attribute key rather than a substring of a longer one.
func protoKey(key string) []byte {
	return append([]byte{0x0a, byte(len(key))}, key...)
}

// protoIntAttr is the protobuf encoding of an OTLP KeyValue holding a small
// integer (0-127): the key, then an AnyValue whose int_value is v.
func protoIntAttr(key string, v byte) []byte {
	return append(protoKey(key), 0x12, 0x02, 0x18, v)
}

// assertWorkerSpend checks exported span bodies for the spend attributes
// of a worker run that searched once and used 60 input and 18 output
// tokens, and for the run core's metrics.
func assertWorkerSpend(t *testing.T, bodies []byte) {
	t.Helper()
	for _, want := range []struct {
		name string
		wire []byte
	}{
		{"worker search_count", protoIntAttr("search_count", 1)},
		{"worker input_tokens", protoIntAttr("input_tokens", 60)},
		{"worker output_tokens", protoIntAttr("output_tokens", 18)},
		{"worker estimated_cost_gbp", protoKey("estimated_cost_gbp")},
		{"run metric task_duration_seconds", protoKey("metric." + trace.MetricTaskDurationSeconds)},
		{"run metric estimated_cost_gbp", protoKey("metric." + trace.MetricEstimatedCostGBP)},
	} {
		t.Run(want.name, func(t *testing.T) {
			must := require.New(t)
			must.True(bytes.Contains(bodies, want.wire))
		})
	}
}

// langfuseArgs points a worker run's spans at a Langfuse endpoint with key
// references resolved from the environment.
func langfuseArgs(endpoint string) []string {
	return []string{
		"--langfuse-endpoint", endpoint,
		"--langfuse-public-key-ref", "secret://LANGFUSE_PUBLIC_KEY",
		"--langfuse-secret-key-ref", "secret://LANGFUSE_SECRET_KEY",
	}
}

// langfuseTelemetry is langfuseArgs as a TelemetryConfig.
func langfuseTelemetry(endpoint string) config.TelemetryConfig {
	return config.TelemetryConfig{
		LangfuseEndpoint:     endpoint,
		LangfusePublicKeyRef: "secret://LANGFUSE_PUBLIC_KEY",
		LangfuseSecretKeyRef: "secret://LANGFUSE_SECRET_KEY",
	}
}

// isolateOTLPEnv blanks the OTLP endpoint and header variables so the
// developer's shell cannot change which collector a test binds or what it
// sends; a test sets the ones it uses afterwards.
func isolateOTLPEnv(t *testing.T) {
	t.Helper()
	for _, name := range []string{
		"OTEL_EXPORTER_OTLP_ENDPOINT", "OTEL_EXPORTER_OTLP_TRACES_ENDPOINT",
		"OTEL_EXPORTER_OTLP_HEADERS", "OTEL_EXPORTER_OTLP_TRACES_HEADERS",
	} {
		t.Setenv(name, "")
	}
}

// TestWorker_ForwardsSpansToLangfuse drives a worker run through the command
// path with the Langfuse flags: spans reach <endpoint>/v1/traces with Basic
// authentication and ingestion version 4, carry the worker's spend
// attributes and the run core's metrics, and no credential transits a span
// body, stderr or stdout. The OTLP environment variable names a decoy
// collector that must receive nothing, since the flags win over it.
func TestWorker_ForwardsSpansToLangfuse(t *testing.T) {
	for _, tt := range []struct {
		name        string
		compression string
	}{
		{"uncompressed", ""},
		{"gzip", "gzip"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			must := require.New(t)
			langfuse := &otlpCollector{}
			langfuseSrv := httptest.NewServer(langfuse.handler(t, http.StatusOK, ""))
			defer langfuseSrv.Close()
			decoy := &otlpCollector{}
			decoySrv := httptest.NewServer(decoy.handler(t, http.StatusOK, ""))
			defer decoySrv.Close()
			searchSrv := searchtest.NewFakeServer([]search.Result{{Title: "Sky", URL: "https://example.org/sky", Snippet: "scattering"}})
			defer searchSrv.Close()
			modelSrv := modeltest.NewFakeServer(
				modeltest.FakeReply{Content: `{"action":"search","query":"sky"}`, FinishReason: "stop", Usage: model.Usage{InputTokens: 20, OutputTokens: 6, TotalTokens: 26}},
				modeltest.FakeReply{
					Content:      `{"action":"final","answer":"# Answer\n\nRayleigh scattering.","citations":[{"url":"https://example.org/sky","title":"Sky"}]}`,
					FinishReason: "stop",
					Usage:        model.Usage{InputTokens: 40, OutputTokens: 12, TotalTokens: 52},
				},
			)
			defer modelSrv.Close()
			t.Setenv("MODEL_KEY", testModelKey)
			t.Setenv("LANGFUSE_PUBLIC_KEY", testLangfusePublic)
			t.Setenv("LANGFUSE_SECRET_KEY", testLangfuseSecret)
			isolateOTLPEnv(t)
			t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", decoySrv.URL)
			t.Setenv("OTEL_EXPORTER_OTLP_COMPRESSION", tt.compression)

			args := workerArgs(modelSrv, searchSrv, append([]string{
				"-o", "text",
				"--fleet-price-input", "1.2",
				"--fleet-price-output", "9.6",
			}, langfuseArgs(langfuseSrv.URL+"/api/public/otel")...)...)
			stdout, stderr, err := execute(t, args...)
			must.NoError(err, "research --agent worker, stderr: %s", stderr)
			must.Contains(stdout, "Rayleigh scattering", "stdout lacks the report")

			requests := langfuse.recorded()
			must.NotEmpty(requests, "no spans reached the Langfuse collector")
			must.Empty(decoy.recorded(), "the OTLP environment collector received requests; the Langfuse flags must win")
			basic := base64.StdEncoding.EncodeToString([]byte(testLangfusePublic + ":" + testLangfuseSecret))
			var bodies []byte
			for _, r := range requests {
				must.Equal("/api/public/otel/v1/traces", r.path)
				must.Equal("Basic "+basic, r.header.Get("Authorization"), "export must use Basic base64(public:secret)")
				must.Equal("4", r.header.Get("X-Langfuse-Ingestion-Version"))
				must.Equal(tt.compression, r.header.Get("Content-Encoding"))
				bodies = append(bodies, r.body...)
			}
			assertWorkerSpend(t, bodies)

			for _, leaked := range []string{testLangfusePublic, testLangfuseSecret, basic, testModelKey} {
				must.False(bytes.Contains(bodies, []byte(leaked)), "a credential transited a span body")
				must.False(strings.Contains(stderr, leaked) || strings.Contains(stdout, leaked), "a credential reached stderr or stdout")
			}
			must.NotContains(stderr, "telemetry error", "a clean export reported an SDK error")
		})
	}
}

// TestWorker_ForwardsSpansToOTLPEndpoint drives a worker run through the
// command path with --otlp-endpoint: spans reach <endpoint>/v1/traces with
// no authentication headers and carry the worker's spend attributes and
// the run core's metrics. The OTLP environment variable names a decoy
// collector that must receive nothing, since the flag wins over it.
func TestWorker_ForwardsSpansToOTLPEndpoint(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	collector := &otlpCollector{}
	collectorSrv := httptest.NewServer(collector.handler(t, http.StatusOK, ""))
	defer collectorSrv.Close()
	decoy := &otlpCollector{}
	decoySrv := httptest.NewServer(decoy.handler(t, http.StatusOK, ""))
	defer decoySrv.Close()
	searchSrv := searchtest.NewFakeServer([]search.Result{{Title: "Sky", URL: "https://example.org/sky", Snippet: "scattering"}})
	defer searchSrv.Close()
	modelSrv := modeltest.NewFakeServer(
		modeltest.FakeReply{Content: `{"action":"search","query":"sky"}`, FinishReason: "stop", Usage: model.Usage{InputTokens: 20, OutputTokens: 6, TotalTokens: 26}},
		modeltest.FakeReply{
			Content:      `{"action":"final","answer":"# Answer\n\nRayleigh scattering.","citations":[{"url":"https://example.org/sky","title":"Sky"}]}`,
			FinishReason: "stop",
			Usage:        model.Usage{InputTokens: 40, OutputTokens: 12, TotalTokens: 52},
		},
	)
	defer modelSrv.Close()
	t.Setenv("MODEL_KEY", testModelKey)
	isolateOTLPEnv(t)
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", decoySrv.URL)

	args := workerArgs(modelSrv, searchSrv,
		"-o", "text",
		"--fleet-price-input", "1.2",
		"--fleet-price-output", "9.6",
		"--otlp-endpoint", collectorSrv.URL,
	)
	stdout, stderr, err := execute(t, args...)
	must.NoError(err, "research --agent worker, stderr: %s", stderr)
	check.Contains(stdout, "Rayleigh scattering", "stdout lacks the report")

	requests := collector.recorded()
	must.NotEmpty(requests, "no spans reached the --otlp-endpoint collector")
	check.Empty(decoy.recorded(), "the OTLP environment collector received requests; --otlp-endpoint must win")
	var bodies []byte
	for _, r := range requests {
		check.Equal(http.MethodPost, r.method)
		check.Equal("/v1/traces", r.path)
		for _, h := range []string{"Authorization", "X-Langfuse-Ingestion-Version"} {
			check.Empty(r.header.Get(h), "%s must not be sent to a generic collector", h)
		}
		bodies = append(bodies, r.body...)
	}
	assertWorkerSpend(t, bodies)
	check.False(bytes.Contains(bodies, []byte(testModelKey)) || strings.Contains(stderr, testModelKey), "the model key transited a span body or stderr")
}

// TestTelemetry_ExportError_IsScrubbed: a collector that rejects the export
// and echoes the credentials in its error body does not fail the run, and
// the SDK's error report reaches stderr scrubbed.
func TestTelemetry_ExportError_IsScrubbed(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	basic := base64.StdEncoding.EncodeToString([]byte(testLangfusePublic + ":" + testLangfuseSecret))
	echo := `{"message":"invalid credentials","authorization":"Basic ` + basic +
		`","publicKey":"` + testLangfusePublic + `","secretKey":"` + testLangfuseSecret + `"}`
	langfuse := &otlpCollector{}
	langfuseSrv := httptest.NewServer(langfuse.handler(t, http.StatusUnauthorized, echo))
	defer langfuseSrv.Close()
	searchSrv := searchtest.NewFakeServer([]search.Result{{Title: "Sky", URL: "https://example.org/sky"}})
	defer searchSrv.Close()
	modelSrv := modeltest.NewFakeServer(
		modeltest.FakeReply{Content: `{"action":"search","query":"sky"}`, FinishReason: "stop"},
		modeltest.FakeReply{Content: `{"action":"final","answer":"Blue.","citations":[{"url":"https://example.org/sky","title":"Sky"}]}`, FinishReason: "stop"},
	)
	defer modelSrv.Close()
	t.Setenv("MODEL_KEY", testModelKey)
	t.Setenv("LANGFUSE_PUBLIC_KEY", testLangfusePublic)
	t.Setenv("LANGFUSE_SECRET_KEY", testLangfuseSecret)
	isolateOTLPEnv(t)

	args := workerArgs(modelSrv, searchSrv, append([]string{"-o", "none"}, langfuseArgs(langfuseSrv.URL+"/api/public/otel")...)...)
	_, stderr, err := execute(t, args...)
	must.NoError(err, "a rejected export must not fail the run; stderr: %s", stderr)
	must.NotEmpty(langfuse.recorded(), "no export reached the collector")
	check.Contains(stderr, "telemetry error", "stderr lacks the export failure")
	check.Contains(stderr, "401", "stderr lacks the export failure")
	check.Contains(stderr, "[REDACTED", "stderr lacks redaction markers")
	for _, leaked := range []string{testLangfusePublic, testLangfuseSecret, basic} {
		check.NotContains(stderr, leaked, "a credential from the collector's reply reached stderr")
	}
}

// TestLangfuse_KeyResolutionFailure_StopsBeforeSpend: an unresolvable Langfuse key
// reference stops the run before any model, search or collector request,
// and the error names the field without echoing the other key's value.
func TestLangfuse_KeyResolutionFailure_StopsBeforeSpend(t *testing.T) {
	for _, tt := range []struct {
		name  string
		unset string
		want  string
	}{
		{"public key", "LANGFUSE_PUBLIC_KEY", "telemetry.langfuse_public_key_ref"},
		{"secret key", "LANGFUSE_SECRET_KEY", "telemetry.langfuse_secret_key_ref"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			must := require.New(t)
			langfuse := &otlpCollector{}
			langfuseSrv := httptest.NewServer(langfuse.handler(t, http.StatusOK, ""))
			defer langfuseSrv.Close()
			searchSrv := searchtest.NewFakeServer(nil)
			defer searchSrv.Close()
			modelSrv := modeltest.NewFakeServer()
			defer modelSrv.Close()
			t.Setenv("MODEL_KEY", testModelKey)
			t.Setenv("LANGFUSE_PUBLIC_KEY", testLangfusePublic)
			t.Setenv("LANGFUSE_SECRET_KEY", testLangfuseSecret)
			t.Setenv(tt.unset, "")
			isolateOTLPEnv(t)

			args := workerArgs(modelSrv, searchSrv, append([]string{"-o", "none"}, langfuseArgs(langfuseSrv.URL+"/api/public/otel")...)...)
			_, stderr, err := execute(t, args...)
			must.ErrorContains(err, tt.want)
			for _, leaked := range []string{testLangfusePublic, testLangfuseSecret, testModelKey} {
				must.False(strings.Contains(err.Error(), leaked) || strings.Contains(stderr, leaked), "a resolved key leaked: %v", err)
			}
			must.Zero(modelSrv.CallCount(), "a run with an unresolvable telemetry key must not dial any endpoint")
			must.Zero(searchSrv.CallCount(), "a run with an unresolvable telemetry key must not dial any endpoint")
			must.Empty(langfuse.recorded(), "a run with an unresolvable telemetry key must not dial any endpoint")
		})
	}
}

// exportOneSpan binds newTracer for tc, records one span and flushes it.
func exportOneSpan(t *testing.T, tc config.TelemetryConfig) {
	t.Helper()
	must := require.New(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	tr, shutdown, err := newTracer(ctx, tc, io.Discard)
	must.NoError(err, "newTracer")
	must.NotNil(shutdown, "newTracer bound %T, want the OTel binding", tr)
	_, span := tr.StartSpan(ctx, trace.SpanResearch)
	span.End(nil)
	must.NoError(shutdown(ctx), "shutdown")
}

// TestNewTracer_EndpointPrecedence: the Langfuse keys win over an explicit OTLP
// endpoint, which wins over the OTLP environment variables; the variables
// alone still bind OTel, and with none the no-op tracer applies.
func TestNewTracer_EndpointPrecedence(t *testing.T) {
	t.Run("no destination binds noop", func(t *testing.T) {
		must := require.New(t)
		isolateOTLPEnv(t)
		tr, shutdown, err := newTracer(context.Background(), config.TelemetryConfig{}, io.Discard)
		must.NoError(err, "newTracer")
		_, isNoop := tr.(trace.Noop)
		must.True(isNoop, "newTracer bound %T, want trace.Noop", tr)
		must.Nil(shutdown, "the no-op tracer must not return a shutdown")
	})

	for _, tt := range []struct {
		name     string
		envVar   string
		envPath  string
		wantPath string
	}{
		{"flag wins over the generic variable", "OTEL_EXPORTER_OTLP_ENDPOINT", "", "/v1/traces"},
		{"flag wins over the traces variable", "OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "/v1/traces", "/v1/traces"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			must := require.New(t)
			flagCollector := &otlpCollector{}
			flagSrv := httptest.NewServer(flagCollector.handler(t, http.StatusOK, ""))
			defer flagSrv.Close()
			envCollector := &otlpCollector{}
			envSrv := httptest.NewServer(envCollector.handler(t, http.StatusOK, ""))
			defer envSrv.Close()
			isolateOTLPEnv(t)
			t.Setenv(tt.envVar, envSrv.URL+tt.envPath)

			exportOneSpan(t, config.TelemetryConfig{OTLPEndpoint: flagSrv.URL})

			got := flagCollector.recorded()
			must.NotEmpty(got, "no span reached the flag's collector")
			must.Equal(tt.wantPath, got[0].path)
			must.Empty(envCollector.recorded(), "the environment's collector received requests")
		})
	}

	t.Run("environment alone binds OTel", func(t *testing.T) {
		must := require.New(t)
		envCollector := &otlpCollector{}
		envSrv := httptest.NewServer(envCollector.handler(t, http.StatusOK, ""))
		defer envSrv.Close()
		isolateOTLPEnv(t)
		t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", envSrv.URL)

		exportOneSpan(t, config.TelemetryConfig{})

		must.NotEmpty(envCollector.recorded(), "no span reached the environment's collector")
	})

	t.Run("langfuse wins over an explicit endpoint", func(t *testing.T) {
		must := require.New(t)
		langfuse := &otlpCollector{}
		langfuseSrv := httptest.NewServer(langfuse.handler(t, http.StatusOK, ""))
		defer langfuseSrv.Close()
		otlp := &otlpCollector{}
		otlpSrv := httptest.NewServer(otlp.handler(t, http.StatusOK, ""))
		defer otlpSrv.Close()
		t.Setenv("LANGFUSE_PUBLIC_KEY", testLangfusePublic)
		t.Setenv("LANGFUSE_SECRET_KEY", testLangfuseSecret)
		isolateOTLPEnv(t)

		// Validate rejects this pair; newTracer's order still decides it.
		tc := langfuseTelemetry(langfuseSrv.URL + "/api/public/otel")
		tc.OTLPEndpoint = otlpSrv.URL
		exportOneSpan(t, tc)

		must.NotEmpty(langfuse.recorded(), "no span reached the Langfuse collector")
		must.Empty(otlp.recorded(), "the explicit OTLP endpoint received requests")
	})
}

// TestNewTracer_Failure_ReleasesErrorRoute: when the exporter cannot be
// built, newTracer unbinds the SDK error route it took, so later reports
// do not reach the failed run's stderr.
func TestNewTracer_Failure_ReleasesErrorRoute(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	isolateOTLPEnv(t)
	var buf bytes.Buffer
	_, _, err := newTracer(context.Background(), config.TelemetryConfig{OTLPEndpoint: "not a url"}, &buf)
	must.Error(err, "newTracer accepted an unusable endpoint")
	check.Nil(otelErrors.logger.Load(), "the SDK error route is still bound after newTracer failed")
	otelErrors.Handle(errors.New("x"))
	check.Empty(buf.String(), "a later report reached the failed run's stderr")
}

// reportOTelError passes err to the SDK error handler while a run is bound
// and returns what the run's stderr received.
func reportOTelError(err error) string {
	var buf bytes.Buffer
	restore := routeOTelErrors(&buf)
	otelErrors.Handle(err)
	restore()
	return buf.String()
}

// TestOTelError_Report_IsBounded: a report is scrubbed within a bounded
// prefix of the SDK's text, cut to 1 KiB of valid UTF-8, and marked
// truncated whenever either cut dropped text.
func TestOTelError_Report_IsBounded(t *testing.T) {
	const marker = " [truncated]\"\n"

	t.Run("cut at 1 KiB", func(t *testing.T) {
		must := require.New(t)
		out := reportOTelError(errors.New(strings.Repeat("x", 2000)))
		must.True(strings.HasSuffix(out, strings.Repeat("x", maxOTelErrorBytes)+marker), "report does not end with 1 KiB of the error and the marker:\n%s", out)
		must.NotContains(out, strings.Repeat("x", maxOTelErrorBytes+1), "report carries more than 1 KiB of the error")
	})

	t.Run("credential straddling the cut", func(t *testing.T) {
		must := require.New(t)
		basic := base64.StdEncoding.EncodeToString([]byte(testLangfusePublic + ":" + testLangfuseSecret))
		head := "failed to send: 401 Unauthorized (body: Authorization: Basic " + basic + " "
		body := head + strings.Repeat(".", 1000-len(head)) + testLangfuseSecret + " " + strings.Repeat("y", 4<<20)
		must.Equal(1000, strings.Index(body, testLangfuseSecret), "the secret key must start at byte 1000 to straddle the cut")
		out := reportOTelError(errors.New(body))
		must.False(strings.Contains(out, basic) || strings.Contains(out, testLangfuseSecret[:len("sk-lf-")+8]), "a credential survived the report:\n%.2000s", out)
		must.True(strings.HasSuffix(out, marker), "report lacks the truncation marker:\n%.2000s", out)
	})

	t.Run("rune split by the cut", func(t *testing.T) {
		must := require.New(t)
		out := reportOTelError(errors.New(strings.Repeat("x", maxOTelErrorBytes-1) + "é" + strings.Repeat("z", 100)))
		must.True(utf8.ValidString(out), "report is not valid UTF-8: %q", out)
		must.True(strings.HasSuffix(out, strings.Repeat("x", maxOTelErrorBytes-1)+marker), "the split rune was not dropped:\n%q", out)
	})

	t.Run("scan cut alone marks the report", func(t *testing.T) {
		must := require.New(t)
		const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
		out := reportOTelError(errors.New(strings.Repeat(alphabet, 300)))
		must.Contains(out, "[REDACTED:high-entropy]", "the scanned prefix was not scrubbed:\n%s", out)
		must.True(strings.HasSuffix(out, marker), "report lacks the truncation marker although the scan cut dropped text:\n%s", out)
	})

	t.Run("invalid UTF-8 with no cut", func(t *testing.T) {
		must := require.New(t)
		// slog's text handler escapes any invalid byte it is given into a
		// valid-UTF-8 \xNN sequence, so utf8.ValidString(out) alone cannot
		// tell whether Handle sanitized msg or slog merely escaped it: check
		// that the invalid bytes were dropped, not preserved as an escape.
		out := reportOTelError(errors.New("failed to send: 400 Bad Request (body: \xff\xfe bad bytes)"))
		must.True(utf8.ValidString(out), "report is not valid UTF-8: %q", out)
		must.NotContains(out, `\xff`, "invalid bytes were escaped rather than stripped by Handle:\n%q", out)
		must.NotContains(out, `\xfe`, "invalid bytes were escaped rather than stripped by Handle:\n%q", out)
		must.Contains(out, "failed to send", "report lost the valid text around the invalid bytes:\n%q", out)
		must.Contains(out, "bad bytes", "report lost the valid text around the invalid bytes:\n%q", out)
		must.NotContains(out, "[truncated]", "a short error was marked truncated:\n%s", out)
	})
}

// TestOTelError_WithoutRun_IsDropped: with no run bound, including after a
// run's route is restored, SDK error reports are dropped rather than
// written to the process stderr.
func TestOTelError_WithoutRun_IsDropped(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	r, w, err := os.Pipe()
	must.NoError(err, "os.Pipe")
	orig := os.Stderr
	os.Stderr = w
	t.Cleanup(func() {
		os.Stderr = orig
		w.Close()
		r.Close()
	})

	(&otelErrorHandler{}).Handle(nil)
	(&otelErrorHandler{}).Handle(errors.New("Authorization: Basic dXNlcjpwYXNz"))
	var buf bytes.Buffer
	restore := routeOTelErrors(&buf)
	restore()
	otelErrors.Handle(errors.New("late"))

	os.Stderr = orig
	w.Close()
	written, err := io.ReadAll(r)
	must.NoError(err, "reading the stderr pipe")
	check.Empty(string(written), "an unbound report reached the process stderr")
	check.Empty(buf.String(), "a report after restore reached the run's stderr")
}

// malformedHeaderSecret is the credential fragment every entry of
// malformedHeaderLists carries.
const malformedHeaderSecret = "b3RlbC11c2Vy"

// malformedHeaderLists are OTLP header variables the exporter cannot parse.
var malformedHeaderLists = []struct {
	name  string
	value string
}{
	{"missing equals", "Authorization: Basic b3RlbC11c2VyOlMzY3IzdCFwdw"},
	{"bad escape", "Authorization=Basic%ZZb3RlbC11c2VyOlMzY3IzdCFwdw"},
	{"credential in the name", "Authorization Basic b3RlbC11c2Vy=x"},
}

// TestOTLP_HeaderEnv_RefusesMalformed: on the Langfuse path and the
// environment-only path, a malformed header variable stops newTracer
// before an exporter or error route exists, and the error names the
// variable without its value.
func TestOTLP_HeaderEnv_RefusesMalformed(t *testing.T) {
	for _, tt := range malformedHeaderLists {
		for _, name := range otlpHeaderEnv {
			for _, path := range []string{"langfuse", "environment"} {
				t.Run(tt.name+"/"+name+"/"+path, func(t *testing.T) {
					must := require.New(t)
					collector := &otlpCollector{}
					srv := httptest.NewServer(collector.handler(t, http.StatusOK, ""))
					defer srv.Close()
					t.Setenv("LANGFUSE_PUBLIC_KEY", testLangfusePublic)
					t.Setenv("LANGFUSE_SECRET_KEY", testLangfuseSecret)
					isolateOTLPEnv(t)
					t.Setenv(name, tt.value)
					var tc config.TelemetryConfig
					if path == "langfuse" {
						tc = langfuseTelemetry(srv.URL + "/api/public/otel")
					} else {
						t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", srv.URL)
					}

					_, shutdown, err := newTracer(context.Background(), tc, io.Discard)
					if err == nil {
						_ = shutdown(context.Background())
					}
					must.Error(err, "newTracer accepted a malformed header list")
					must.True(strings.HasPrefix(err.Error(), name+":"), "err = %v, want it to name %s", err, name)
					must.NotContains(err.Error(), malformedHeaderSecret, "the error echoes the header value")
					must.Empty(collector.recorded(), "the collector received requests")
					must.Nil(otelErrors.logger.Load(), "a refused run left the SDK error route bound")
				})
			}
		}
	}
}

// TestOTLP_HeaderEnv_WellFormedReachesCollector: a well-formed list, blank
// entries included, passes the check and reaches the environment's
// collector decoded.
func TestOTLP_HeaderEnv_WellFormedReachesCollector(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	collector := &otlpCollector{}
	srv := httptest.NewServer(collector.handler(t, http.StatusOK, ""))
	defer srv.Close()
	isolateOTLPEnv(t)
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", srv.URL)
	t.Setenv("OTEL_EXPORTER_OTLP_HEADERS", "x-team=abc,Authorization=Basic%20Zm9vOmJhcg%3D%3D,")

	exportOneSpan(t, config.TelemetryConfig{})

	got := collector.recorded()
	must.NotEmpty(got, "no span reached the environment's collector")
	for _, r := range got {
		check.Equal("abc", r.header.Get("X-Team"), "x-team header")
		check.Equal("Basic Zm9vOmJhcg==", r.header.Get("Authorization"), "Authorization header, decoded")
	}
}

// malformedEndpointEnvSecret is the credential fragment the userinfo case
// of malformedEndpointEnvLists carries.
const malformedEndpointEnvSecret = "S3cr3tQk99"

// malformedEndpointEnvLists are OTLP endpoint variables url.Parse rejects,
// the same grammar the exporter's own envconfig applies.
var malformedEndpointEnvLists = []struct {
	name  string
	value string
}{
	{"bad escape with userinfo", "https://otel-user:" + malformedEndpointEnvSecret + "%ZZ@collector.example"},
	{"control character", "https://collector.example/\x7f"},
}

// TestOTLP_EndpointEnv_RefusesMalformed: on every OTel path, an endpoint
// variable url.Parse rejects stops newTracer before an exporter or error
// route exists, and the error names the variable without its value.
func TestOTLP_EndpointEnv_RefusesMalformed(t *testing.T) {
	for _, tt := range malformedEndpointEnvLists {
		for _, name := range otlpEndpointEnv {
			for _, path := range []string{"langfuse", "otlp-endpoint", "environment"} {
				t.Run(tt.name+"/"+name+"/"+path, func(t *testing.T) {
					must := require.New(t)
					collector := &otlpCollector{}
					srv := httptest.NewServer(collector.handler(t, http.StatusOK, ""))
					defer srv.Close()
					t.Setenv("LANGFUSE_PUBLIC_KEY", testLangfusePublic)
					t.Setenv("LANGFUSE_SECRET_KEY", testLangfuseSecret)
					isolateOTLPEnv(t)
					t.Setenv(name, tt.value)
					var tc config.TelemetryConfig
					switch path {
					case "langfuse":
						tc = langfuseTelemetry(srv.URL + "/api/public/otel")
					case "otlp-endpoint":
						tc = config.TelemetryConfig{OTLPEndpoint: srv.URL}
					}

					_, shutdown, err := newTracer(context.Background(), tc, io.Discard)
					if err == nil {
						_ = shutdown(context.Background())
					}
					must.Error(err, "newTracer accepted a malformed endpoint variable")
					must.True(strings.HasPrefix(err.Error(), name+":"), "err = %v, want it to name %s", err, name)
					must.NotContains(err.Error(), malformedEndpointEnvSecret, "the error echoes the endpoint value")
					must.Empty(collector.recorded(), "the collector received requests")
					must.Nil(otelErrors.logger.Load(), "a refused run left the SDK error route bound")
				})
			}
		}
	}
}

// TestOTLP_EndpointEnv_WellFormedReachesCollector: a well-formed endpoint
// variable passes the check, and the environment-only path still binds
// OTel and reaches its collector.
func TestOTLP_EndpointEnv_WellFormedReachesCollector(t *testing.T) {
	for _, tt := range []struct {
		name    string
		envVar  string
		envPath string
	}{
		{"generic endpoint", "OTEL_EXPORTER_OTLP_ENDPOINT", ""},
		{"traces endpoint", "OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "/v1/traces"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			must := require.New(t)
			collector := &otlpCollector{}
			srv := httptest.NewServer(collector.handler(t, http.StatusOK, ""))
			defer srv.Close()
			isolateOTLPEnv(t)
			t.Setenv(tt.envVar, srv.URL+tt.envPath)

			exportOneSpan(t, config.TelemetryConfig{})

			must.NotEmpty(collector.recorded(), "no span reached the environment's collector")
		})
	}
}

// headerChildRole selects the child-process role of
// TestMalformedOTLPHeaderEnvStaysOffProcessStderr.
const headerChildRole = "CHIRON_TEST_OTLP_HEADER_CHILD"

// TestMalformedOTLPHeaderEnvStaysOffProcessStderr runs in child processes
// because the SDK's logger writes to the stderr file the process started
// with, which swapping os.Stderr does not capture. The sdk child builds
// the exporter directly and shows the SDK prints a malformed value; the
// newTracer child shows the refusal keeps it off stderr.
func TestMalformedOTLPHeaderEnvStaysOffProcessStderr(t *testing.T) {
	must := require.New(t)
	switch os.Getenv(headerChildRole) {
	case "sdk":
		for _, tt := range malformedHeaderLists {
			t.Setenv("OTEL_EXPORTER_OTLP_HEADERS", tt.value)
			_, shutdown, err := trace.NewOTel(context.Background(), "http://127.0.0.1:1")
			must.NoError(err, "NewOTel")
			_ = shutdown(context.Background())
		}
		return
	case "newTracer":
		t.Setenv("LANGFUSE_PUBLIC_KEY", testLangfusePublic)
		t.Setenv("LANGFUSE_SECRET_KEY", testLangfuseSecret)
		for _, tt := range malformedHeaderLists {
			for _, name := range otlpHeaderEnv {
				for _, path := range []string{"langfuse", "environment"} {
					t.Run(tt.name+"/"+name+"/"+path, func(t *testing.T) {
						must := require.New(t)
						var tc config.TelemetryConfig
						if path == "langfuse" {
							tc = langfuseTelemetry("http://127.0.0.1:1/api/public/otel")
						}
						isolateOTLPEnv(t)
						t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://127.0.0.1:1")
						t.Setenv(name, tt.value)
						_, shutdown, err := newTracer(context.Background(), tc, io.Discard)
						if err == nil {
							_ = shutdown(context.Background())
						}
						must.Error(err, "newTracer accepted a malformed header list")
					})
				}
			}
		}
		return
	}

	for _, tt := range []struct {
		role     string
		wantLeak bool
	}{
		{"sdk", true},
		{"newTracer", false},
	} {
		t.Run(tt.role, func(t *testing.T) {
			must := require.New(t)
			cmd := exec.Command(os.Args[0], "-test.run=^TestMalformedOTLPHeaderEnvStaysOffProcessStderr$")
			cmd.Env = append(os.Environ(), headerChildRole+"="+tt.role)
			var stdout, stderr bytes.Buffer
			cmd.Stdout = &stdout
			cmd.Stderr = &stderr
			err := cmd.Run()
			must.NoError(err, "child stdout: %s\nstderr: %s", stdout.String(), stderr.String())
			leaked := strings.Contains(stderr.String(), malformedHeaderSecret)
			must.Equal(tt.wantLeak, leaked, "header value on the child's stderr\nstderr: %s", stderr.String())
		})
	}
}

// operatorHeaders is a deployment's OTLP header variable: a credential for
// the collector the environment names.
const operatorHeaders = "x-honeycomb-team=operator-team-key"

// TestOTLP_HeaderEnv_GoesOnlyToEnvironmentCollector: the environment's
// headers reach the collector the environment names and are replaced on
// the Langfuse path; beside an explicit OTLP endpoint, newTracer refuses
// to bind rather than send them there.
func TestOTLP_HeaderEnv_GoesOnlyToEnvironmentCollector(t *testing.T) {
	for _, name := range otlpHeaderEnv {
		t.Run("explicit endpoint refused/"+name, func(t *testing.T) {
			must := require.New(t)
			collector := &otlpCollector{}
			srv := httptest.NewServer(collector.handler(t, http.StatusOK, ""))
			defer srv.Close()
			isolateOTLPEnv(t)
			t.Setenv(name, operatorHeaders)

			_, shutdown, err := newTracer(context.Background(), config.TelemetryConfig{OTLPEndpoint: srv.URL}, io.Discard)
			if err == nil {
				_ = shutdown(context.Background())
			}
			must.ErrorContains(err, name, "newTracer should refuse an explicit endpoint beside an OTLP header variable")
			must.NotContains(err.Error(), "operator-team-key", "the error echoes the header value")
			must.Empty(collector.recorded(), "the explicit endpoint received requests")
		})

		t.Run("environment collector receives them/"+name, func(t *testing.T) {
			must := require.New(t)
			collector := &otlpCollector{}
			srv := httptest.NewServer(collector.handler(t, http.StatusOK, ""))
			defer srv.Close()
			isolateOTLPEnv(t)
			t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", srv.URL)
			t.Setenv(name, operatorHeaders)

			exportOneSpan(t, config.TelemetryConfig{})

			got := collector.recorded()
			must.NotEmpty(got, "no span reached the environment's collector")
			for _, r := range got {
				must.Equal("operator-team-key", r.header.Get("X-Honeycomb-Team"), "x-honeycomb-team should be the environment's value")
			}
		})

		t.Run("langfuse replaces them/"+name, func(t *testing.T) {
			must := require.New(t)
			langfuse := &otlpCollector{}
			srv := httptest.NewServer(langfuse.handler(t, http.StatusOK, ""))
			defer srv.Close()
			t.Setenv("LANGFUSE_PUBLIC_KEY", testLangfusePublic)
			t.Setenv("LANGFUSE_SECRET_KEY", testLangfuseSecret)
			isolateOTLPEnv(t)
			t.Setenv(name, operatorHeaders)

			exportOneSpan(t, langfuseTelemetry(srv.URL+"/api/public/otel"))

			got := langfuse.recorded()
			must.NotEmpty(got, "no span reached the Langfuse collector")
			basic := base64.StdEncoding.EncodeToString([]byte(testLangfusePublic + ":" + testLangfuseSecret))
			for _, r := range got {
				must.Equal("Basic "+basic, r.header.Get("Authorization"), "Authorization should be Basic base64(public:secret)")
				must.Empty(r.header.Get("X-Honeycomb-Team"), "x-honeycomb-team reached Langfuse")
			}
		})
	}
}

// TestOTLP_EndpointBesideHeaderEnv_FailsBeforeSpend: through the command
// path, --otlp-endpoint beside an OTLP header variable stops the run
// before any model, search or collector request, without echoing the
// header.
func TestOTLP_EndpointBesideHeaderEnv_FailsBeforeSpend(t *testing.T) {
	must := require.New(t)
	collector := &otlpCollector{}
	collectorSrv := httptest.NewServer(collector.handler(t, http.StatusOK, ""))
	defer collectorSrv.Close()
	searchSrv := searchtest.NewFakeServer(nil)
	defer searchSrv.Close()
	modelSrv := modeltest.NewFakeServer()
	defer modelSrv.Close()
	t.Setenv("MODEL_KEY", testModelKey)
	isolateOTLPEnv(t)
	t.Setenv("OTEL_EXPORTER_OTLP_HEADERS", operatorHeaders)

	_, stderr, err := execute(t, workerArgs(modelSrv, searchSrv, "-o", "none", "--otlp-endpoint", collectorSrv.URL)...)
	must.ErrorContains(err, "OTEL_EXPORTER_OTLP_HEADERS", "want a refusal naming the header variable")
	must.NotContains(err.Error(), "operator-team-key", "the header value leaked into the error")
	must.NotContains(stderr, "operator-team-key", "the header value leaked to stderr")
	must.Zero(modelSrv.CallCount(), "a refused run must not call the model")
	must.Zero(searchSrv.CallCount(), "a refused run must not call search")
	must.Empty(collector.recorded(), "a refused run must not send to the collector")
}
