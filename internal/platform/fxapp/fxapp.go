// Package fxapp compõe a aplicação com Uber Fx. É o ÚNICO pacote que
// importa Fx: domínio, casos de uso e adaptadores recebem dependências por
// construtores comuns e não sabem que o Fx existe.
//
// Ordem do ciclo de vida (o Fx para na ordem inversa da partida):
//
//	partida: pool do Postgres (ping) -> servidor HTTP
//	parada:  servidor HTTP (para de aceitar, conclui requisições) -> pool
//
// Assim as dependências só fecham depois que os componentes que as usam
// terminaram.
package fxapp

import (
	"context"
	"log/slog"
	"os"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/fx"
	"go.uber.org/fx/fxevent"

	"github.com/Taino-Edu/Desafio-go-arqui/internal/adapters/httpapi"
	"github.com/Taino-Edu/Desafio-go-arqui/internal/adapters/postgres"
	"github.com/Taino-Edu/Desafio-go-arqui/internal/app"
	"github.com/Taino-Edu/Desafio-go-arqui/internal/domain/wagering"
	"github.com/Taino-Edu/Desafio-go-arqui/internal/platform/config"
	"github.com/Taino-Edu/Desafio-go-arqui/internal/worker"
)

// New devolve todas as opções do Fx para uma configuração já validada.
// extra permite aos testes substituir peças (ex.: fx.Replace de um logger).
func New(cfg config.Config, extra ...fx.Option) fx.Option {
	return fx.Options(
		fx.Supply(cfg),
		fx.StartTimeout(cfg.StartTimeout),
		fx.StopTimeout(cfg.ShutdownTimeout),
		LoggingModule,
		fx.WithLogger(func(l *slog.Logger) fxevent.Logger {
			return &fxevent.SlogLogger{Logger: l.With("component", "fx")}
		}),
		PostgresModule,
		AppModule,
		HTTPModule,
		WorkersModule,
		fx.Options(extra...),
	)
}

// LoggingModule: logs JSON em stdout.
var LoggingModule = fx.Module("logging",
	fx.Provide(func(cfg config.Config) *slog.Logger {
		h := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: cfg.LogLevel})
		return slog.New(h).With("service", "wallet", "instance", cfg.InstanceID)
	}),
)

// PostgresModule: pool de conexões e Store transacional.
var PostgresModule = fx.Module("postgres",
	fx.Provide(newPool),
	fx.Provide(func(p *pgxpool.Pool) app.Store { return postgres.NewStore(p) }),
)

func newPool(lc fx.Lifecycle, cfg config.Config, log *slog.Logger) (*pgxpool.Pool, error) {
	pool, err := postgres.NewPool(postgres.PoolConfig{
		URL:              cfg.Database.URL,
		MaxConns:         cfg.Database.MaxConns,
		StatementTimeout: cfg.Database.StatementTimeout,
		LockTimeout:      cfg.Database.LockTimeout,
		ApplicationName:  "wallet-" + cfg.InstanceID,
	})
	if err != nil {
		return nil, err
	}
	lc.Append(fx.Hook{
		// valida a dependência na partida: sem banco, a aplicação não sobe
		OnStart: func(ctx context.Context) error {
			if err := postgres.Ping(ctx, pool); err != nil {
				return err
			}
			log.Info("postgres connected")
			return nil
		},
		OnStop: func(context.Context) error {
			pool.Close()
			log.Info("postgres pool closed")
			return nil
		},
	})
	return pool, nil
}

// AppModule: casos de uso.
var AppModule = fx.Module("app",
	fx.Provide(
		func() app.Clock { return app.SystemClock{} },
		func() app.IDGenerator { return app.UUIDv7{} },
		app.NewWalletService,
		func(cfg config.Config) (wagering.ReferenceRetryPolicy, error) {
			p := wagering.ReferenceRetryPolicy{
				BaseDelay: cfg.References.BaseDelay, MaxDelay: cfg.References.MaxDelay,
				MaxAttempts: cfg.References.MaxAttempts,
			}
			return p, p.Validate()
		},
		app.NewWagerService,
	),
)

// HTTPModule: health, rotas e servidor.
var HTTPModule = fx.Module("http",
	fx.Provide(
		func(cfg config.Config) httpapi.Config {
			return httpapi.Config{
				Addr: cfg.HTTP.Addr, RequestTimeout: cfg.HTTP.RequestTimeout,
				ReadTimeout: cfg.HTTP.ReadTimeout, WriteTimeout: cfg.HTTP.WriteTimeout,
				IdleTimeout: cfg.HTTP.IdleTimeout,
			}
		},
		func(pool *pgxpool.Pool) *httpapi.Health {
			return httpapi.NewHealth(2*time.Second, postgres.HealthChecker{Pool: pool})
		},
		httpapi.NewHandler,
		httpapi.NewServer,
	),
	fx.Invoke(func(lc fx.Lifecycle, s *httpapi.Server) {
		lc.Append(fx.Hook{OnStart: s.Start, OnStop: s.Stop})
	}),
)

// WorkersModule: trabalho em segundo plano. Os workers dependem do Store
// (e portanto do pool), então o Fx os para ANTES de fechar o pool.
var WorkersModule = fx.Module("workers",
	fx.Invoke(registerReferenceWorker),
)

func registerReferenceWorker(lc fx.Lifecycle, cfg config.Config, svc *app.WagerService, log *slog.Logger) {
	if !cfg.References.WorkerEnabled {
		log.Info("reference worker disabled")
		return
	}
	loop := worker.New(worker.Config{
		Name:        "pending-references",
		Interval:    cfg.References.PollInterval,
		ItemTimeout: cfg.HTTP.RequestTimeout,
	}, log, func(ctx context.Context) (bool, error) {
		res, err := svc.ResolveNextPending(ctx)
		if err != nil || !res.Found {
			return false, err
		}
		tx := res.Transaction
		log.Info("pending reference processed",
			"transactionId", tx.ID(), "walletId", tx.WalletID(), "providerId", tx.External().ProviderID,
			"outcome", res.Outcome, "status", tx.Status(), "failureCode", tx.FailureCode(), "attempts", tx.Attempts())
		return true, nil
	})
	lc.Append(fx.Hook{OnStart: loop.Start, OnStop: loop.Stop})
}
