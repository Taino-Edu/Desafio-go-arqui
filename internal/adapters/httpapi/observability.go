package httpapi

import (
	"net/http"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

// RequestObserver recebe a medição de cada requisição. route é o PADRÃO da
// rota (ex.: "/wallets/{walletId}"), nunca o caminho com os ids: um rótulo
// por carteira criaria uma série nova a cada carteira. "unmatched" quando
// nenhuma rota casou (404/405).
type RequestObserver interface {
	ObserveRequest(method, route string, status int, d time.Duration)
}

// Observability liga métricas e tracing ao servidor HTTP. Campos nil
// desligam.
type Observability struct {
	Requests       RequestObserver
	MetricsHandler http.Handler // servido em GET /metrics
	Tracer         trace.Tracer
	Propagator     propagation.TextMapPropagator // lê o traceparent recebido
}

// RouteUnmatched é o rótulo de rota das requisições que não casaram.
const RouteUnmatched = "unmatched"

// withMetrics mede cada requisição e dá ao span dela o nome da rota.
// Precisa envolver o mux sem nenhum
// r.WithContext no meio: o mux grava o padrão casado em r.Pattern do
// *http.Request que recebe, e só o MESMO ponteiro mostra o padrão depois.
func withMetrics(obs RequestObserver, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		route := routeOf(r)
		if obs != nil {
			obs.ObserveRequest(r.Method, route, rec.status, time.Since(start))
		}
		// o span da requisição só ganha o nome da rota aqui, depois do mux
		span := trace.SpanFromContext(r.Context())
		span.SetName(r.Method + " " + route)
		span.SetAttributes(attribute.String("http.route", route))
	})
}

// withTracing abre o span da requisição (o primeiro middleware a rodar),
// continuando o trace do cliente quando ele manda traceparent. O nome
// definitivo ("POST /wagering/transactions") é dado por withMetrics.
// Health checks e coletas de /metrics ficam de fora (como nos logs): chegam
// a cada poucos segundos e enterrariam os traces que interessam.
func withTracing(tr trace.Tracer, prop propagation.TextMapPropagator, next http.Handler) http.Handler {
	if tr == nil {
		return next
	}
	if prop == nil {
		prop = propagation.TraceContext{}
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isOperational(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		ctx := prop.Extract(r.Context(), propagation.HeaderCarrier(r.Header))
		ctx, span := tr.Start(ctx, r.Method+" "+RouteUnmatched,
			trace.WithSpanKind(trace.SpanKindServer),
			trace.WithAttributes(
				attribute.String("http.request.method", r.Method),
				attribute.String("url.path", r.URL.Path),
			))
		defer span.End()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r.WithContext(ctx))
		span.SetAttributes(attribute.Int("http.response.status_code", rec.status))
		if rec.status >= 500 {
			span.SetStatus(codes.Error, http.StatusText(rec.status))
		}
	})
}

// routeOf tira o método do padrão: "GET /wallets/{walletId}" vira
// "/wallets/{walletId}" (o método já é outro rótulo).
func routeOf(r *http.Request) string {
	if r.Pattern == "" {
		return RouteUnmatched
	}
	if _, path, ok := strings.Cut(r.Pattern, " "); ok {
		return path
	}
	return r.Pattern
}
