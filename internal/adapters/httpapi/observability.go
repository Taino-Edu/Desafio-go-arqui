package httpapi

import (
	"net/http"
	"strings"
	"time"
)

// RequestObserver recebe a medição de cada requisição. route é o PADRÃO da
// rota (ex.: "/wallets/{walletId}"), nunca o caminho com os ids: um rótulo
// por carteira criaria uma série nova a cada carteira. "unmatched" quando
// nenhuma rota casou (404/405).
type RequestObserver interface {
	ObserveRequest(method, route string, status int, d time.Duration)
}

// Observability liga as métricas ao servidor HTTP. Campos nil desligam.
type Observability struct {
	Requests       RequestObserver
	MetricsHandler http.Handler // servido em GET /metrics
}

// RouteUnmatched é o rótulo de rota das requisições que não casaram.
const RouteUnmatched = "unmatched"

// withMetrics mede cada requisição. Precisa envolver o mux sem nenhum
// r.WithContext no meio: o mux grava o padrão casado em r.Pattern do
// *http.Request que recebe, e só o MESMO ponteiro mostra o padrão depois.
func withMetrics(obs RequestObserver, next http.Handler) http.Handler {
	if obs == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		obs.ObserveRequest(r.Method, routeOf(r), rec.status, time.Since(start))
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
