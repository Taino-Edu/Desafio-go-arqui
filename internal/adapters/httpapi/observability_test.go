package httpapi

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

type observed struct {
	method, route string
	status        int
}

type fakeObserver struct {
	mu  sync.Mutex
	got []observed
}

func (f *fakeObserver) ObserveRequest(method, route string, status int, _ time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.got = append(f.got, observed{method, route, status})
}

// A rota medida é o PADRÃO, nunca o caminho com o id, mesmo atravessando a
// pilha completa de middlewares (que troca o *http.Request com WithContext).
func TestMetricsMiddleware_UsesRoutePattern(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	mux := http.NewServeMux()
	mux.HandleFunc("GET /wallets/{walletId}", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	})
	mux.HandleFunc("GET /boom", func(http.ResponseWriter, *http.Request) { panic("bug") })

	obs := &fakeObserver{}
	var h http.Handler = mux
	h = withRecover(log, h)
	h = withMetrics(obs, h)
	h = withTimeout(time.Second, h)
	h = withLogging(log, h)
	h = withCorrelation(h)

	for _, path := range []string{"/wallets/0192f291-27dd-7d3f-8071-5f8685deef37", "/nada", "/boom"} {
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", path, nil))
	}
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("DELETE", "/wallets/x", nil))

	want := []observed{
		{"GET", "/wallets/{walletId}", http.StatusTeapot},
		{"GET", RouteUnmatched, http.StatusNotFound},
		{"GET", "/boom", http.StatusInternalServerError}, // panic medido como 500
		{"DELETE", RouteUnmatched, http.StatusMethodNotAllowed},
	}
	if len(obs.got) != len(want) {
		t.Fatalf("observado = %+v", obs.got)
	}
	for i := range want {
		if obs.got[i] != want[i] {
			t.Errorf("[%d] = %+v, want %+v", i, obs.got[i], want[i])
		}
	}
}
