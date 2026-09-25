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
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/rxbynerd/chiron/internal/config"
	"github.com/rxbynerd/chiron/internal/researcher/fleet/model"
	"github.com/rxbynerd/chiron/internal/researcher/fleet/search"
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
	path   string
	header http.Header
	body   []byte
}

// handler records each request and answers with status and reply.
func (c *otlpCollector) handler(t *testing.T, status int, reply string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var body io.Reader = r.Body
		if r.Header.Get("Content-Encoding") == "gzip" {
			zr, err := gzip.NewReader(r.Body)
			if err != nil {
				t.Errorf("collector: gzip body: %v", err)
				http.Error(w, "bad gzip body", http.StatusBadRequest)
				return
			}
			defer zr.Close()
			body = zr
		}
		raw, err := io.ReadAll(body)
		if err != nil {
			t.Errorf("collector: reading body: %v", err)
		}
		c.mu.Lock()
		c.requests = append(c.requests, otlpRequest{path: r.URL.Path, header: r.Header.Clone(), body: raw})
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

// langfuseArgs points a worker run's spans at a Langfuse endpoint with key
// references resolved from the environment.
func langfuseArgs(endpoint string) []string {
	return []string{
		"--langfuse-endpoint", endpoint,
		"--langfuse-public-key-ref", "secret://LANGFUSE_PUBLIC_KEY",
		"--langfuse-secret-key-ref", "secret://LANGFUSE_SECRET_KEY",
	}
}

// TestWorkerForwardsSpansToLangfuse drives a worker run through the command
// path with the Langfuse flags: spans reach <endpoint>/v1/traces with Basic
// authentication and ingestion version 4, carry the worker's spend
// attributes and the run core's metrics, and no credential transits a span
// body, stderr or stdout. The OTLP environment variable names a decoy
// collector that must receive nothing, since the flags win over it.
func TestWorkerForwardsSpansToLangfuse(t *testing.T) {
	for _, tt := range []struct {
		name        string
		compression string
	}{
		{"uncompressed", ""},
		{"gzip", "gzip"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			langfuse := &otlpCollector{}
			langfuseSrv := httptest.NewServer(langfuse.handler(t, http.StatusOK, ""))
			defer langfuseSrv.Close()
			decoy := &otlpCollector{}
			decoySrv := httptest.NewServer(decoy.handler(t, http.StatusOK, ""))
			defer decoySrv.Close()
			searchSrv := search.NewFakeServer([]search.Result{{Title: "Sky", URL: "https://example.org/sky", Snippet: "scattering"}})
			defer searchSrv.Close()
			modelSrv := model.NewFakeServer(
				model.FakeReply{Content: `{"action":"search","query":"sky"}`, FinishReason: "stop", Usage: model.Usage{InputTokens: 20, OutputTokens: 6, TotalTokens: 26}},
				model.FakeReply{
					Content:      `{"action":"final","answer":"# Answer\n\nRayleigh scattering.","citations":[{"url":"https://example.org/sky","title":"Sky"}]}`,
					FinishReason: "stop",
					Usage:        model.Usage{InputTokens: 40, OutputTokens: 12, TotalTokens: 52},
				},
			)
			defer modelSrv.Close()
			t.Setenv("MODEL_KEY", testModelKey)
			t.Setenv("LANGFUSE_PUBLIC_KEY", testLangfusePublic)
			t.Setenv("LANGFUSE_SECRET_KEY", testLangfuseSecret)
			t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", decoySrv.URL)
			t.Setenv("OTEL_EXPORTER_OTLP_COMPRESSION", tt.compression)

			args := workerArgs(modelSrv, searchSrv, append([]string{
				"-o", "text",
				"--fleet-price-input", "1.2",
				"--fleet-price-output", "9.6",
			}, langfuseArgs(langfuseSrv.URL+"/api/public/otel")...)...)
			stdout, stderr, err := execute(t, args...)
			if err != nil {
				t.Fatalf("research --agent worker: %v\nstderr: %s", err, stderr)
			}
			if !strings.Contains(stdout, "Rayleigh scattering") {
				t.Errorf("stdout lacks the report:\n%s", stdout)
			}

			requests := langfuse.recorded()
			if len(requests) == 0 {
				t.Fatal("no spans reached the Langfuse collector")
			}
			if got := decoy.recorded(); len(got) != 0 {
				t.Errorf("the OTLP environment collector received %d requests; the Langfuse flags must win", len(got))
			}
			basic := base64.StdEncoding.EncodeToString([]byte(testLangfusePublic + ":" + testLangfuseSecret))
			var bodies []byte
			for _, r := range requests {
				if r.path != "/api/public/otel/v1/traces" {
					t.Errorf("export path = %q, want /api/public/otel/v1/traces", r.path)
				}
				if got := r.header.Get("Authorization"); got != "Basic "+basic {
					t.Errorf("Authorization = %q, want Basic base64(public:secret)", got)
				}
				if got := r.header.Get("X-Langfuse-Ingestion-Version"); got != "4" {
					t.Errorf("x-langfuse-ingestion-version = %q, want 4", got)
				}
				if got := r.header.Get("Content-Encoding"); got != tt.compression {
					t.Errorf("Content-Encoding = %q, want %q", got, tt.compression)
				}
				bodies = append(bodies, r.body...)
			}

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
				if !bytes.Contains(bodies, want.wire) {
					t.Errorf("exported spans lack %s", want.name)
				}
			}

			for _, leaked := range []string{testLangfusePublic, testLangfuseSecret, basic, testModelKey} {
				if bytes.Contains(bodies, []byte(leaked)) {
					t.Errorf("a credential transited a span body")
				}
				if strings.Contains(stderr, leaked) || strings.Contains(stdout, leaked) {
					t.Errorf("a credential reached stderr or stdout")
				}
			}
			if strings.Contains(stderr, "telemetry error") {
				t.Errorf("a clean export reported an SDK error:\n%s", stderr)
			}
		})
	}
}

// TestTelemetryExportErrorIsScrubbed: a collector that rejects the export
// and echoes the credentials in its error body does not fail the run, and
// the SDK's error report reaches stderr scrubbed.
func TestTelemetryExportErrorIsScrubbed(t *testing.T) {
	basic := base64.StdEncoding.EncodeToString([]byte(testLangfusePublic + ":" + testLangfuseSecret))
	echo := `{"message":"invalid credentials","authorization":"Basic ` + basic +
		`","publicKey":"` + testLangfusePublic + `","secretKey":"` + testLangfuseSecret + `"}`
	langfuse := &otlpCollector{}
	langfuseSrv := httptest.NewServer(langfuse.handler(t, http.StatusUnauthorized, echo))
	defer langfuseSrv.Close()
	searchSrv := search.NewFakeServer([]search.Result{{Title: "Sky", URL: "https://example.org/sky"}})
	defer searchSrv.Close()
	modelSrv := model.NewFakeServer(
		model.FakeReply{Content: `{"action":"search","query":"sky"}`, FinishReason: "stop"},
		model.FakeReply{Content: `{"action":"final","answer":"Blue.","citations":[{"url":"https://example.org/sky","title":"Sky"}]}`, FinishReason: "stop"},
	)
	defer modelSrv.Close()
	t.Setenv("MODEL_KEY", testModelKey)
	t.Setenv("LANGFUSE_PUBLIC_KEY", testLangfusePublic)
	t.Setenv("LANGFUSE_SECRET_KEY", testLangfuseSecret)

	args := workerArgs(modelSrv, searchSrv, append([]string{"-o", "none"}, langfuseArgs(langfuseSrv.URL+"/api/public/otel")...)...)
	_, stderr, err := execute(t, args...)
	if err != nil {
		t.Fatalf("a rejected export must not fail the run: %v\nstderr: %s", err, stderr)
	}
	if len(langfuse.recorded()) == 0 {
		t.Fatal("no export reached the collector")
	}
	if !strings.Contains(stderr, "telemetry error") || !strings.Contains(stderr, "401") {
		t.Errorf("stderr lacks the export failure:\n%s", stderr)
	}
	if !strings.Contains(stderr, "[REDACTED") {
		t.Errorf("stderr lacks redaction markers:\n%s", stderr)
	}
	for _, leaked := range []string{testLangfusePublic, testLangfuseSecret, basic} {
		if strings.Contains(stderr, leaked) {
			t.Errorf("a credential from the collector's reply reached stderr:\n%s", stderr)
		}
	}
}

// TestLangfuseKeyResolutionFailsBeforeSpend: an unresolvable Langfuse key
// reference stops the run before any model, search or collector request,
// and the error names the field without echoing the other key's value.
func TestLangfuseKeyResolutionFailsBeforeSpend(t *testing.T) {
	for _, tt := range []struct {
		name  string
		unset string
		want  string
	}{
		{"public key", "LANGFUSE_PUBLIC_KEY", "telemetry.langfuse_public_key_ref"},
		{"secret key", "LANGFUSE_SECRET_KEY", "telemetry.langfuse_secret_key_ref"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			langfuse := &otlpCollector{}
			langfuseSrv := httptest.NewServer(langfuse.handler(t, http.StatusOK, ""))
			defer langfuseSrv.Close()
			searchSrv := search.NewFakeServer(nil)
			defer searchSrv.Close()
			modelSrv := model.NewFakeServer()
			defer modelSrv.Close()
			t.Setenv("MODEL_KEY", testModelKey)
			t.Setenv("LANGFUSE_PUBLIC_KEY", testLangfusePublic)
			t.Setenv("LANGFUSE_SECRET_KEY", testLangfuseSecret)
			t.Setenv(tt.unset, "")

			args := workerArgs(modelSrv, searchSrv, append([]string{"-o", "none"}, langfuseArgs(langfuseSrv.URL+"/api/public/otel")...)...)
			_, stderr, err := execute(t, args...)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want it to name %s", err, tt.want)
			}
			for _, leaked := range []string{testLangfusePublic, testLangfuseSecret, testModelKey} {
				if strings.Contains(err.Error(), leaked) || strings.Contains(stderr, leaked) {
					t.Errorf("a resolved key leaked: %v", err)
				}
			}
			if modelSrv.CallCount() != 0 || searchSrv.CallCount() != 0 || len(langfuse.recorded()) != 0 {
				t.Error("a run with an unresolvable telemetry key must not dial any endpoint")
			}
		})
	}
}

// exportOneSpan binds newTracer for tc, records one span and flushes it.
func exportOneSpan(t *testing.T, tc config.TelemetryConfig) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	tr, shutdown, err := newTracer(ctx, tc, io.Discard)
	if err != nil {
		t.Fatalf("newTracer: %v", err)
	}
	if shutdown == nil {
		t.Fatalf("newTracer bound %T, want the OTel binding", tr)
	}
	_, span := tr.StartSpan(ctx, trace.SpanResearch)
	span.End(nil)
	if err := shutdown(ctx); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
}

// TestNewTracerPrecedence: an explicit OTLP endpoint wins over the OTLP
// environment variables, the variables alone still bind OTel, and with
// neither the no-op tracer applies.
func TestNewTracerPrecedence(t *testing.T) {
	t.Run("no destination binds noop", func(t *testing.T) {
		t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
		t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "")
		tr, shutdown, err := newTracer(context.Background(), config.TelemetryConfig{}, io.Discard)
		if err != nil {
			t.Fatalf("newTracer: %v", err)
		}
		if _, ok := tr.(trace.Noop); !ok || shutdown != nil {
			t.Errorf("newTracer bound %T (shutdown set: %v), want trace.Noop with no shutdown", tr, shutdown != nil)
		}
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
			flagCollector := &otlpCollector{}
			flagSrv := httptest.NewServer(flagCollector.handler(t, http.StatusOK, ""))
			defer flagSrv.Close()
			envCollector := &otlpCollector{}
			envSrv := httptest.NewServer(envCollector.handler(t, http.StatusOK, ""))
			defer envSrv.Close()
			t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
			t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "")
			t.Setenv(tt.envVar, envSrv.URL+tt.envPath)

			exportOneSpan(t, config.TelemetryConfig{OTLPEndpoint: flagSrv.URL})

			got := flagCollector.recorded()
			if len(got) == 0 {
				t.Fatal("no span reached the flag's collector")
			}
			if got[0].path != tt.wantPath {
				t.Errorf("export path = %q, want %q", got[0].path, tt.wantPath)
			}
			if n := len(envCollector.recorded()); n != 0 {
				t.Errorf("the environment's collector received %d requests", n)
			}
		})
	}

	t.Run("environment alone binds OTel", func(t *testing.T) {
		envCollector := &otlpCollector{}
		envSrv := httptest.NewServer(envCollector.handler(t, http.StatusOK, ""))
		defer envSrv.Close()
		t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", envSrv.URL)
		t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "")

		exportOneSpan(t, config.TelemetryConfig{})

		if len(envCollector.recorded()) == 0 {
			t.Fatal("no span reached the environment's collector")
		}
	})
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

// TestOTelErrorReportIsBounded: a report is scrubbed within a bounded
// prefix of the SDK's text, cut to 1 KiB of valid UTF-8, and marked
// truncated whenever either cut dropped text.
func TestOTelErrorReportIsBounded(t *testing.T) {
	const marker = " [truncated]\"\n"

	t.Run("cut at 1 KiB", func(t *testing.T) {
		out := reportOTelError(errors.New(strings.Repeat("x", 2000)))
		if !strings.HasSuffix(out, strings.Repeat("x", maxOTelErrorBytes)+marker) {
			t.Errorf("report does not end with 1 KiB of the error and the marker:\n%s", out)
		}
		if strings.Contains(out, strings.Repeat("x", maxOTelErrorBytes+1)) {
			t.Error("report carries more than 1 KiB of the error")
		}
	})

	t.Run("credential straddling the cut", func(t *testing.T) {
		basic := base64.StdEncoding.EncodeToString([]byte(testLangfusePublic + ":" + testLangfuseSecret))
		head := "failed to send: 401 Unauthorized (body: Authorization: Basic " + basic + " "
		body := head + strings.Repeat(".", 1000-len(head)) + testLangfuseSecret + " " + strings.Repeat("y", 4<<20)
		if strings.Index(body, testLangfuseSecret) != 1000 {
			t.Fatal("the secret key must start at byte 1000 to straddle the cut")
		}
		out := reportOTelError(errors.New(body))
		if strings.Contains(out, basic) || strings.Contains(out, testLangfuseSecret[:len("sk-lf-")+8]) {
			t.Errorf("a credential survived the report:\n%.2000s", out)
		}
		if !strings.HasSuffix(out, marker) {
			t.Errorf("report lacks the truncation marker:\n%.2000s", out)
		}
	})

	t.Run("rune split by the cut", func(t *testing.T) {
		out := reportOTelError(errors.New(strings.Repeat("x", maxOTelErrorBytes-1) + "é" + strings.Repeat("z", 100)))
		if !utf8.ValidString(out) {
			t.Errorf("report is not valid UTF-8: %q", out)
		}
		if !strings.HasSuffix(out, strings.Repeat("x", maxOTelErrorBytes-1)+marker) {
			t.Errorf("the split rune was not dropped:\n%q", out)
		}
	})

	t.Run("scan cut alone marks the report", func(t *testing.T) {
		const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
		out := reportOTelError(errors.New(strings.Repeat(alphabet, 300)))
		if !strings.Contains(out, "[REDACTED:high-entropy]") {
			t.Errorf("the scanned prefix was not scrubbed:\n%s", out)
		}
		if !strings.HasSuffix(out, marker) {
			t.Errorf("report lacks the truncation marker although the scan cut dropped text:\n%s", out)
		}
	})
}

// TestOTelErrorsDroppedWithoutRun: with no run bound, including after a
// run's route is restored, SDK error reports are dropped rather than
// written to the process stderr.
func TestOTelErrorsDroppedWithoutRun(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
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
	if err != nil {
		t.Fatalf("reading the stderr pipe: %v", err)
	}
	if len(written) != 0 {
		t.Errorf("an unbound report reached the process stderr: %q", written)
	}
	if buf.Len() != 0 {
		t.Errorf("a report after restore reached the run's stderr: %q", buf.String())
	}
}
