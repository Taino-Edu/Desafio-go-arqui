// Package httpapi expõe a API HTTP (net/http). Traduz JSON e status HTTP para
// chamadas dos casos de uso; não contém regra de negócio.
package httpapi

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/Taino-Edu/Desafio-go-arqui/internal/app"
)

// Config são os parâmetros do servidor HTTP.
type Config struct {
	Addr           string
	RequestTimeout time.Duration
	ReadTimeout    time.Duration
	WriteTimeout   time.Duration
	IdleTimeout    time.Duration
}

// NewHandler monta as rotas e os middlewares.
func NewHandler(log *slog.Logger, cfg Config, wallets *app.WalletService, wagers *app.WagerService,
	health *Health, verifier TokenVerifier, obs Observability) http.Handler {
	wh := walletHandlers{svc: wallets, log: log}
	gh := wagerHandlers{svc: wagers, log: log}

	// públicas (operacionais; sem dados de negócio)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health/live", health.live)
	mux.HandleFunc("GET /health/ready", health.ready)
	if obs.MetricsHandler != nil {
		mux.Handle("GET /metrics", obs.MetricsHandler)
	}

	// negócio: sempre autenticadas; a autorização fica em cada handler
	protect := func(h http.HandlerFunc) http.Handler { return withAuth(verifier, log, h) }

	mux.Handle("POST /wallets", protect(wh.open))
	mux.Handle("GET /wallets/{walletId}", protect(wh.get))
	mux.Handle("GET /wallets/{walletId}/ledger", protect(wh.ledger))
	mux.Handle("POST /wallets/{walletId}/reconciliation", protect(wh.reconcile))

	mux.Handle("POST /wagering/transactions", protect(gh.submit))
	mux.Handle("GET /wagering/transactions/{transactionId}", protect(gh.getByID))
	mux.Handle("GET /providers/{providerId}/wagering/transactions/{externalTransactionId}", protect(gh.getByExternalID))

	// Middlewares, de dentro para fora. A ordem importa: withRecover fica
	// dentro de withMetrics para que um panic seja medido como 500;
	// withMetrics fica colado no mux para ler r.Pattern; withCorrelation é o
	// primeiro a rodar, então tudo abaixo já tem o correlationId.
	var h http.Handler = mux
	h = withRecover(log, h)
	h = withMetrics(obs.Requests, h)
	h = withTimeout(cfg.RequestTimeout, h)
	h = withLogging(log, h)
	h = withCorrelation(h)
	return h
}

// Server envolve o http.Server com início e parada explícitos, para o ciclo
// de vida do Fx.
type Server struct {
	srv    *http.Server
	health *Health
	log    *slog.Logger
	ln     net.Listener
	done   chan error
}

func NewServer(cfg Config, handler http.Handler, health *Health, log *slog.Logger) *Server {
	return &Server{
		srv: &http.Server{
			Addr:              cfg.Addr,
			Handler:           handler,
			ReadHeaderTimeout: 5 * time.Second,
			ReadTimeout:       cfg.ReadTimeout,
			WriteTimeout:      cfg.WriteTimeout,
			IdleTimeout:       cfg.IdleTimeout,
			ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelWarn),
		},
		health: health,
		log:    log,
	}
}

// Start abre a porta (falha na hora se estiver ocupada) e serve em
// segundo plano.
func (s *Server) Start(context.Context) error {
	ln, err := net.Listen("tcp", s.srv.Addr)
	if err != nil {
		return fmt.Errorf("http listen %s: %w", s.srv.Addr, err)
	}
	s.ln = ln
	s.done = make(chan error, 1)
	go func() {
		err := s.srv.Serve(ln)
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		s.done <- err
	}()
	s.log.Info("http server started", "addr", ln.Addr().String())
	return nil
}

// Stop para de aceitar conexões, marca o readiness como "draining" e espera
// as requisições em andamento terminarem até o prazo de ctx.
func (s *Server) Stop(ctx context.Context) error {
	s.health.SetDraining()
	s.log.Info("http server stopping")
	err := s.srv.Shutdown(ctx)
	if err != nil {
		// prazo esgotado: fecha as conexões que restaram
		err = errors.Join(err, s.srv.Close())
	}
	if s.done != nil {
		err = errors.Join(err, <-s.done)
	}
	s.log.Info("http server stopped")
	return err
}

// Addr devolve o endereço efetivo (útil com porta ":0" nos testes).
func (s *Server) Addr() string {
	if s.ln == nil {
		return s.srv.Addr
	}
	return s.ln.Addr().String()
}
