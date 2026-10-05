package httpapi

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

const (
	clientTraceID = "4bf92f3577b34da6a3ce929d0e0e4736"
	clientSpanID  = "00f067aa0ba902b7"
)

func attrOf(s sdktrace.ReadOnlySpan, key attribute.Key) attribute.Value {
	for _, kv := range s.Attributes() {
		if kv.Key == key {
			return kv.Value
		}
	}
	return attribute.Value{}
}

// O span da requisição continua o trace do cliente (traceparent), leva o
// nome da ROTA (não o caminho com o id), o status e o correlationId, e o
// contexto dele chega até o handler.
func TestTracing_ContinuesClientTraceAndNamesByRoute(t *testing.T) {
	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	var seen trace.SpanContext
	mux := http.NewServeMux()
	mux.HandleFunc("GET /wallets/{walletId}", func(w http.ResponseWriter, r *http.Request) {
		seen = trace.SpanContextFromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("GET /boom", func(http.ResponseWriter, *http.Request) { panic("bug") })
	mux.HandleFunc("GET /health/ready", func(http.ResponseWriter, *http.Request) {})

	var h http.Handler = mux
	h = withRecover(log, h)
	h = withMetrics(nil, h)
	h = withTimeout(time.Second, h)
	h = withLogging(log, h)
	h = withCorrelation(h)
	h = withTracing(tp.Tracer("test"), propagation.TraceContext{}, h)

	req := httptest.NewRequest("GET", "/wallets/0192f291-27dd-7d3f-8071-5f8685deef37", nil)
	req.Header.Set("traceparent", "00-"+clientTraceID+"-"+clientSpanID+"-01")
	req.Header.Set(HeaderCorrelationID, "corr-7")
	h.ServeHTTP(httptest.NewRecorder(), req)
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/boom", nil))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/nada", nil))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/health/ready", nil)) // sem span

	spans := rec.Ended()
	if len(spans) != 3 {
		t.Fatalf("spans = %d", len(spans))
	}
	s := spans[0]
	if s.Name() != "GET /wallets/{walletId}" || s.SpanKind() != trace.SpanKindServer {
		t.Errorf("span = %q kind %v", s.Name(), s.SpanKind())
	}
	if s.SpanContext().TraceID().String() != clientTraceID || s.Parent().SpanID().String() != clientSpanID {
		t.Errorf("não continuou o trace do cliente: trace %s parent %s", s.SpanContext().TraceID(), s.Parent().SpanID())
	}
	if !s.Parent().IsRemote() {
		t.Error("o pai deveria ser remoto (veio pelo cabeçalho)")
	}
	if seen.SpanID() != s.SpanContext().SpanID() {
		t.Errorf("o handler não recebeu o span da requisição: %s", seen.SpanID())
	}
	if attrOf(s, "http.route").AsString() != "/wallets/{walletId}" ||
		attrOf(s, "http.response.status_code").AsInt64() != 200 ||
		attrOf(s, "correlation.id").AsString() != "corr-7" {
		t.Errorf("atributos = %v", s.Attributes())
	}
	if s.Status().Code == codes.Error {
		t.Error("200 não é erro")
	}

	boom := spans[1]
	if boom.Name() != "GET /boom" || boom.Status().Code != codes.Error ||
		attrOf(boom, "http.response.status_code").AsInt64() != 500 {
		t.Errorf("panic: %q %v %v", boom.Name(), boom.Status(), boom.Attributes())
	}
	if boom.Parent().IsValid() {
		t.Error("sem traceparent o span deveria ser raiz")
	}
	if spans[2].Name() != "GET "+RouteUnmatched {
		t.Errorf("404 = %q", spans[2].Name())
	}
}

// Sem Tracer (tracing desligado na montagem manual) o middleware some.
func TestTracing_NilTracerIsPassThrough(t *testing.T) {
	called := false
	h := withTracing(nil, nil, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/", nil))
	if !called {
		t.Fatal("handler não chamado")
	}
}
