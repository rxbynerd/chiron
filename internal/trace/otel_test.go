package trace

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// newRecordedOTel returns an OTel tracer whose spans land in an
// in-memory recorder instead of an OTLP endpoint.
func newRecordedOTel() (*OTel, *tracetest.SpanRecorder) {
	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
	return &OTel{tr: tp.Tracer(instrumentationName)}, rec
}

func TestOTelSpansNestAndRecord(t *testing.T) {
	tr, rec := newRecordedOTel()

	rctx, root := tr.StartSpan(context.Background(), SpanResearch)
	actx, await := tr.StartSpan(rctx, SpanAwait)
	await.SetAttr("attempt", 3)
	tr.Metric(actx, MetricPollCount, 3)
	await.End(nil)
	tr.Metric(rctx, MetricInputTokens, 4096)
	root.End(nil)

	spans := rec.Ended()
	if len(spans) != 2 {
		t.Fatalf("got %d spans, want 2", len(spans))
	}
	awaitSpan, rootSpan := spans[0], spans[1]
	if awaitSpan.Name() != SpanAwait || rootSpan.Name() != SpanResearch {
		t.Fatalf("span names = %q, %q", awaitSpan.Name(), rootSpan.Name())
	}
	if awaitSpan.Parent().SpanID() != rootSpan.SpanContext().SpanID() {
		t.Fatal("await span does not nest beneath research")
	}

	attrs := attrMap(awaitSpan)
	if attrs["attempt"] != "3" {
		t.Fatalf("attempt attr = %q, want 3", attrs["attempt"])
	}
	if attrs["metric."+MetricPollCount] != "3" {
		t.Fatalf("metric attr = %q, want 3", attrs["metric."+MetricPollCount])
	}
	if attrMap(rootSpan)["metric."+MetricInputTokens] != "4096" {
		t.Fatal("root span missing input_tokens metric attribute")
	}
}

// TestOTelScrubsEverything proves a credential cannot reach the SDK via
// span name, attribute key, attribute value, stringified value, error,
// or metric name.
func TestOTelScrubsEverything(t *testing.T) {
	tr, rec := newRecordedOTel()

	ctx, span := tr.StartSpan(context.Background(), "research for "+fakeKey)
	span.SetAttr("header "+fakeKey, "x-goog-api-key: "+fakeKey)
	span.SetAttr("err_detail", errors.New("denied for "+fakeKey))
	tr.Metric(ctx, "tokens for "+fakeKey, 7)
	span.End(errors.New("auth failed: " + fakeKey))

	for _, s := range rec.Ended() {
		if strings.Contains(s.Name(), fakeKey) {
			t.Fatalf("credential in span name %q", s.Name())
		}
		for _, kv := range s.Attributes() {
			if strings.Contains(string(kv.Key), fakeKey) || strings.Contains(kv.Value.String(), fakeKey) {
				t.Fatalf("credential in attribute %s=%s", kv.Key, kv.Value.String())
			}
		}
		if strings.Contains(s.Status().Description, fakeKey) {
			t.Fatalf("credential in status %q", s.Status().Description)
		}
		for _, ev := range s.Events() {
			if strings.Contains(ev.Name, fakeKey) {
				t.Fatalf("credential in event name %q", ev.Name)
			}
			for _, kv := range ev.Attributes {
				if strings.Contains(kv.Value.String(), fakeKey) {
					t.Fatalf("credential in event attribute %s=%s", kv.Key, kv.Value.String())
				}
			}
		}
	}
}

func TestNewOTelConstructsAndShutsDown(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	tr, shutdown, err := NewOTel(ctx, "http://localhost:4318")
	if err != nil {
		t.Fatalf("NewOTel: %v", err)
	}
	if tr == nil || shutdown == nil {
		t.Fatal("NewOTel returned nil tracer or shutdown")
	}
	// No spans were recorded, so shutdown flushes nothing and must not
	// touch the network.
	if err := shutdown(ctx); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
}

// TestNewOTelExportsToTracesPathWithHeaders: endpointURL is an OTLP base
// URL, so spans arrive at <base>/v1/traces carrying the static headers as
// they were when the option was built.
func TestNewOTelExportsToTracesPathWithHeaders(t *testing.T) {
	const auth = "Basic dGVzdDp0ZXN0"
	for _, tt := range []struct {
		name     string
		basePath string
		wantPath string
	}{
		{"host only", "", "/v1/traces"},
		{"base path", "/api/public/otel", "/api/public/otel/v1/traces"},
		{"trailing slash", "/api/public/otel/", "/api/public/otel/v1/traces"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var (
				mu      sync.Mutex
				paths   []string
				headers []http.Header
			)
			collector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				mu.Lock()
				defer mu.Unlock()
				paths = append(paths, r.URL.Path)
				headers = append(headers, r.Header.Clone())
			}))
			defer collector.Close()

			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			sent := map[string]string{"Authorization": auth, "x-langfuse-ingestion-version": "4"}
			tr, shutdown, err := NewOTel(ctx, collector.URL+tt.basePath, WithHeaders(sent))
			if err != nil {
				t.Fatalf("NewOTel: %v", err)
			}
			sent["Authorization"] = "changed after construction"

			_, span := tr.StartSpan(ctx, SpanResearch)
			span.End(nil)
			if err := shutdown(ctx); err != nil {
				t.Fatalf("shutdown: %v", err)
			}

			mu.Lock()
			defer mu.Unlock()
			if len(paths) == 0 {
				t.Fatal("no export reached the collector")
			}
			for i, path := range paths {
				if path != tt.wantPath {
					t.Errorf("export path = %q, want %q", path, tt.wantPath)
				}
				if got := headers[i].Get("Authorization"); got != auth {
					t.Errorf("Authorization = %q, want %q", got, auth)
				}
				if got := headers[i].Get("X-Langfuse-Ingestion-Version"); got != "4" {
					t.Errorf("x-langfuse-ingestion-version = %q, want 4", got)
				}
			}
		})
	}
}

// TestNewOTelRejectsInvalidEndpoint: an endpoint that is not an absolute
// http(s) URL fails construction without echoing the value.
func TestNewOTelRejectsInvalidEndpoint(t *testing.T) {
	for _, tt := range []struct {
		name     string
		endpoint string
	}{
		{"relative", "collector.internal/otel"},
		{"userinfo without host", "https://user:hunter2-pw@/otel"},
		{"unsupported scheme", "ftp://collector.internal/otel"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, _, err := NewOTel(context.Background(), tt.endpoint)
			if err == nil {
				t.Fatal("NewOTel accepted the endpoint")
			}
			if strings.Contains(err.Error(), tt.endpoint) || strings.Contains(err.Error(), "hunter2") {
				t.Errorf("error echoes the endpoint: %v", err)
			}
		})
	}
}

func attrMap(s sdktrace.ReadOnlySpan) map[string]string {
	m := make(map[string]string)
	for _, kv := range s.Attributes() {
		m[string(kv.Key)] = fmt.Sprint(kv.Value.AsInterface())
	}
	return m
}
