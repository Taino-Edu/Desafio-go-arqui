package httpapi

import (
	"context"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

// Checker verifica uma dependência para o readiness.
type Checker interface {
	Name() string
	Check(ctx context.Context) error
}

// Health responde liveness e readiness.
//
//   - /health/live: o processo está de pé (sem dependências).
//   - /health/ready: as dependências respondem e o serviço não está
//     desligando. Ao iniciar o shutdown, passa a responder 503 para que o
//     balanceador pare de mandar requisições novas.
type Health struct {
	checkers []Checker
	timeout  time.Duration
	draining atomic.Bool
}

func NewHealth(timeout time.Duration, checkers ...Checker) *Health {
	return &Health{checkers: checkers, timeout: timeout}
}

// SetDraining marca o início do shutdown.
func (h *Health) SetDraining() { h.draining.Store(true) }

type healthBody struct {
	Status string            `json:"status"`
	Checks map[string]string `json:"checks,omitempty"`
}

func (h *Health) live(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, healthBody{Status: "ok"})
}

func (h *Health) ready(w http.ResponseWriter, r *http.Request) {
	if h.draining.Load() {
		writeJSON(w, http.StatusServiceUnavailable, healthBody{Status: "draining"})
		return
	}
	// as verificações rodam em paralelo, cada uma com o próprio prazo: uma
	// dependência lenta (SQS retentando) não pode consumir o tempo das outras
	// e fazer o Postgres parecer fora do ar
	results := make([]error, len(h.checkers))
	var wg sync.WaitGroup
	for i, c := range h.checkers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(r.Context(), h.timeout)
			defer cancel()
			results[i] = c.Check(ctx)
		}()
	}
	wg.Wait()

	body := healthBody{Status: "ok", Checks: map[string]string{}}
	status := http.StatusOK
	for i, c := range h.checkers {
		if results[i] != nil {
			body.Checks[c.Name()] = "unavailable"
			body.Status, status = "unavailable", http.StatusServiceUnavailable
			continue
		}
		body.Checks[c.Name()] = "ok"
	}
	writeJSON(w, status, body)
}
