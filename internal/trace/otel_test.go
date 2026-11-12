package trace

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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

func TestOTel_Spans_NestAndRecord(t *testing.T) {
	must := require.New(t)
	tr, rec := newRecordedOTel()

	rctx, root := tr.StartSpan(context.Background(), SpanResearch)
	actx, await := tr.StartSpan(rctx, SpanAwait)
	await.SetAttr("attempt", 3)
	tr.Metric(actx, MetricPollCount, 3)
	await.End(nil)
	tr.Metric(rctx, MetricInputTokens, 4096)
	root.End(nil)

	spans := rec.Ended()
	must.Len(spans, 2, "ended spans")
	awaitSpan, rootSpan := spans[0], spans[1]
	must.Equal(SpanAwait, awaitSpan.Name(), "await span name")
	must.Equal(SpanResearch, rootSpan.Name(), "research span name")
	must.Equal(rootSpan.SpanContext().SpanID(), awaitSpan.Parent().SpanID(), "await span parent")

	attrs := attrMap(awaitSpan)
	must.Equal("3", attrs["attempt"], "attempt attribute")
	must.Equal("3", attrs["metric."+MetricPollCount], "poll count metric attribute")
	must.Equal("4096", attrMap(rootSpan)["metric."+MetricInputTokens], "input tokens metric attribute on root span")
}

// Proves a credential cannot reach the SDK via span name, attribute key,
// attribute value, stringified value, error, or metric name.
func TestOTel_Scrub_KeepsKeyOutOfAllFields(t *testing.T) {
	must := require.New(t)
	tr, rec := newRecordedOTel()

	ctx, span := tr.StartSpan(context.Background(), "research for "+fakeKey)
	span.SetAttr("header "+fakeKey, "x-goog-api-key: "+fakeKey)
	span.SetAttr("err_detail", errors.New("denied for "+fakeKey))
	tr.Metric(ctx, "tokens for "+fakeKey, 7)
	span.End(errors.New("auth failed: " + fakeKey))

	for _, s := range rec.Ended() {
		must.NotContains(s.Name(), fakeKey, "span name")
		for _, kv := range s.Attributes() {
			must.NotContains(string(kv.Key), fakeKey, "attribute key")
			must.NotContains(kv.Value.String(), fakeKey, "value of attribute %s", kv.Key)
		}
		must.NotContains(s.Status().Description, fakeKey, "span status")
		for _, ev := range s.Events() {
			must.NotContains(ev.Name, fakeKey, "event name")
			for _, kv := range ev.Attributes {
				must.NotContains(kv.Value.String(), fakeKey, "value of event attribute %s", kv.Key)
			}
		}
	}
}

func TestNewOTel_Construct_ShutsDown(t *testing.T) {
	must := require.New(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	tr, shutdown, err := NewOTel(ctx, "http://localhost:4318")
	must.NoError(err, "NewOTel")
	must.NotNil(tr, "tracer")
	must.NotNil(shutdown, "shutdown")
	// No spans were recorded, so shutdown flushes nothing and must not
	// touch the network.
	must.NoError(shutdown(ctx), "shutdown")
}

// endpointURL is an OTLP base URL, so spans arrive at <base>/v1/traces
// carrying the static headers as they were when the option was built.
func TestNewOTel_Export_UsesTracesPathAndHeaders(t *testing.T) {
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
			must := require.New(t)
			check := assert.New(t)
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
			must.NoError(err, "NewOTel")
			sent["Authorization"] = "changed after construction"

			_, span := tr.StartSpan(ctx, SpanResearch)
			span.End(nil)
			must.NoError(shutdown(ctx), "shutdown")

			mu.Lock()
			defer mu.Unlock()
			must.NotEmpty(paths, "no export reached the collector")
			for i, path := range paths {
				check.Equal(tt.wantPath, path, "export %d path", i)
				check.Equal(auth, headers[i].Get("Authorization"), "export %d Authorization", i)
				check.Equal("4", headers[i].Get("X-Langfuse-Ingestion-Version"), "export %d x-langfuse-ingestion-version", i)
			}
		})
	}
}

// An explicit endpoint that redirects to another port on the same host
// fails the export, so the target never receives the request or its
// Authorization header.
func TestNewOTel_Export_RefusesRedirects(t *testing.T) {
	for _, tt := range []struct {
		name   string
		status int
	}{
		{"307", http.StatusTemporaryRedirect},
		{"308", http.StatusPermanentRedirect},
	} {
		t.Run(tt.name, func(t *testing.T) {
			must := require.New(t)
			var (
				mu                  sync.Mutex
				redirected, reached int
			)
			target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				mu.Lock()
				defer mu.Unlock()
				reached++
			}))
			defer target.Close()
			endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				mu.Lock()
				redirected++
				mu.Unlock()
				http.Redirect(w, r, target.URL+"/v1/traces", tt.status)
			}))
			defer endpoint.Close()

			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			tr, shutdown, err := NewOTel(ctx, endpoint.URL, WithHeaders(map[string]string{"Authorization": "Basic cGstbGY6c2stbGY="}))
			must.NoError(err, "NewOTel")
			_, span := tr.StartSpan(ctx, SpanResearch)
			span.End(nil)
			_ = shutdown(ctx)

			mu.Lock()
			defer mu.Unlock()
			must.NotZero(redirected, "no export reached the endpoint")
			must.Zero(reached, "requests received by the redirect target")
		})
	}
}

// An endpoint that is not an absolute http(s) URL fails construction
// without echoing the value.
func TestNewOTel_RejectsInvalidEndpoint(t *testing.T) {
	for _, tt := range []struct {
		name     string
		endpoint string
	}{
		{"relative", "collector.internal/otel"},
		{"userinfo without host", "https://user:hunter2-pw@/otel"},
		{"unsupported scheme", "ftp://collector.internal/otel"},
		{"userinfo", "https://user:pw@collector.example/otel"},
		{"query", "https://collector.example/otel?x=1"},
		{"empty query", "https://collector.example/otel?"},
		{"fragment", "https://collector.example/otel#f"},
		{"empty hostname", "https://:443/otel"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			must := require.New(t)
			_, _, err := NewOTel(context.Background(), tt.endpoint)
			must.Error(err, "NewOTel accepted the endpoint")
			must.NotContains(err.Error(), tt.endpoint, "error echoes the endpoint")
			must.NotContains(err.Error(), "pw", "error echoes the endpoint")
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
