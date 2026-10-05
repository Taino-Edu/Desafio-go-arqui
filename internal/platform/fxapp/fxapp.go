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
	"errors"
	"io"
	"log/slog"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/fx"
	"go.uber.org/fx/fxevent"

	"github.com/Taino-Edu/Desafio-go-arqui/internal/adapters/auth"
	"github.com/Taino-Edu/Desafio-go-arqui/internal/adapters/httpapi"
	"github.com/Taino-Edu/Desafio-go-arqui/internal/adapters/observability"
	"github.com/Taino-Edu/Desafio-go-arqui/internal/adapters/postgres"
	"github.com/Taino-Edu/Desafio-go-arqui/internal/adapters/sqsconsumer"
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
		ObservabilityModule,
		PostgresModule,
		AppModule,
		HTTPModule,
		WorkersModule,
		awsModule(cfg),
		sqsModule(cfg),
		outboxModule(cfg),
		fx.Options(extra...),
	)
}

// LoggingModule: logs JSON em stdout.
var LoggingModule = fx.Module("logging",
	fx.Provide(func(cfg config.Config) *slog.Logger {
		return NewLogger(os.Stdout, cfg.LogLevel, cfg.InstanceID)
	}),
)

// NewLogger cria o logger da aplicação: JSON em w, com service e instance em
// toda linha. O ContextHandler acrescenta o correlationId do contexto a todo
// log feito com ele (InfoContext, ErrorContext...).
func NewLogger(w io.Writer, level slog.Level, instanceID string) *slog.Logger {
	h := slog.NewJSONHandler(w, &slog.HandlerOptions{Level: level})
	return slog.New(observability.NewContextHandler(h)).With("service", "wallet", "instance", instanceID)
}

// ObservabilityModule: métricas Prometheus num registry próprio, servidas
// em GET /metrics. Os casos de uso recebem a porta app.Metrics; o consumidor
// SQS e o HTTP recebem as suas portas; nenhum deles conhece o Prometheus.
var ObservabilityModule = fx.Module("observability",
	fx.Provide(
		observability.NewPrometheus,
		func(p *observability.Prometheus) app.Metrics { return p },
		func(p *observability.Prometheus) sqsconsumer.Metrics { return p },
		func(p *observability.Prometheus, log *slog.Logger) httpapi.Observability {
			return httpapi.Observability{Requests: p, MetricsHandler: p.Handler(log)}
		},
	),
	// atraso da outbox e pendências: lidos do banco a cada coleta
	fx.Invoke(func(p *observability.Prometheus, store app.Store) error {
		return p.RegisterBacklog(func(ctx context.Context) (app.Backlog, error) {
			return app.ReadBacklog(ctx, store)
		}, collectTimeout)
	}),
)

// collectTimeout limita cada consulta feita durante uma coleta de métricas.
const collectTimeout = 2 * time.Second

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

// AppModule: casos de uso. Os construtores recebem as métricas como opção
// (app.WithMetrics); o Fx ignora parâmetros variádicos, então a injeção é
// explícita aqui.
var AppModule = fx.Module("app",
	fx.Provide(
		func() app.Clock { return app.SystemClock{} },
		func() app.IDGenerator { return app.UUIDv7{} },
		func(s app.Store, c app.Clock, ids app.IDGenerator, m app.Metrics) *app.WalletService {
			return app.NewWalletService(s, c, ids, app.WithMetrics(m))
		},
		func(cfg config.Config) (wagering.ReferenceRetryPolicy, error) {
			p := wagering.ReferenceRetryPolicy{
				BaseDelay: cfg.References.BaseDelay, MaxDelay: cfg.References.MaxDelay,
				MaxAttempts: cfg.References.MaxAttempts,
			}
			return p, p.Validate()
		},
		func(s app.Store, c app.Clock, ids app.IDGenerator, p wagering.ReferenceRetryPolicy, m app.Metrics) *app.WagerService {
			return app.NewWagerService(s, c, ids, p, app.WithMetrics(m))
		},
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
		fx.Annotate(
			func(pool *pgxpool.Pool) httpapi.Checker { return postgres.HealthChecker{Pool: pool} },
			fx.ResultTags(`group:"readiness"`),
		),
		fx.Annotate(
			func(checkers []httpapi.Checker) *httpapi.Health { return httpapi.NewHealth(2*time.Second, checkers...) },
			fx.ParamTags(`group:"readiness"`),
		),
		func(cfg config.Config) (httpapi.TokenVerifier, error) {
			return auth.NewVerifier(auth.Config{
				Issuer: cfg.Auth.Issuer, Audience: cfg.Auth.Audience, JWKSURL: cfg.Auth.JWKSURL,
			})
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

// awsModule: cliente SQS, compartilhado pelo consumidor e pelo publicador da
// outbox (só quando algum dos dois está ligado).
func awsModule(cfg config.Config) fx.Option {
	if !cfg.SQS.Enabled && !cfg.Outbox.Enabled {
		return fx.Options()
	}
	return fx.Module("aws",
		fx.Provide(func(cfg config.Config) (*sqs.Client, error) {
			return sqsconsumer.NewClient(context.Background(), sqsconsumer.ClientConfig{
				Region: cfg.SQS.Region, Endpoint: cfg.SQS.Endpoint,
				AccessKeyID: cfg.SQS.AccessKeyID, SecretAccessKey: cfg.SQS.SecretAccessKey,
			})
		}),
	)
}

// outboxModule: publicador da outbox (só quando OUTBOX_PUBLISHER_ENABLED).
func outboxModule(cfg config.Config) fx.Option {
	if !cfg.Outbox.Enabled {
		return fx.Options()
	}
	return fx.Module("outbox", fx.Invoke(registerOutboxPublisher))
}

func registerOutboxPublisher(lc fx.Lifecycle, cfg config.Config, client *sqs.Client, store app.Store,
	clock app.Clock, metrics app.Metrics, log *slog.Logger) {
	var eventsURL atomic.Pointer[string]
	svc := app.NewOutboxService(store,
		sqsconsumer.Publisher{API: client, QueueURL: func() string {
			if p := eventsURL.Load(); p != nil {
				return *p
			}
			return ""
		}},
		clock, app.OutboxConfig{
			Owner: cfg.InstanceID, BatchSize: cfg.Outbox.BatchSize, Lease: cfg.Outbox.Lease,
			PerAggregate: cfg.Outbox.PerAggregate, Parallelism: cfg.Outbox.Parallelism,
			RetryBase: cfg.Outbox.RetryBaseDelay, RetryMax: cfg.Outbox.RetryMaxDelay,
		}, app.OutboxHooks{}, app.WithMetrics(metrics))

	loop := worker.New(worker.Config{
		Name: "outbox-publisher", Interval: cfg.Outbox.PollInterval,
		// o lote inteiro precisa caber no arrendamento
		ItemTimeout: cfg.Outbox.Lease,
	}, log, func(ctx context.Context) (bool, error) {
		res, err := svc.PublishBatch(ctx)
		if res.Claimed > 0 {
			log.Info("outbox batch", "claimed", res.Claimed, "published", res.Published, "failed", res.Failed)
		}
		return res.Claimed > 0, err
	})
	lc.Append(fx.Hook{
		// valida a dependência na partida: a fila de eventos precisa existir
		OnStart: func(ctx context.Context) error {
			url, err := sqsconsumer.ResolveQueueURL(ctx, client, cfg.Outbox.Queue)
			if err != nil {
				return err
			}
			eventsURL.Store(&url)
			return loop.Start(ctx)
		},
		OnStop: loop.Stop,
	})
}

// sqsModule: consumidor da fila de entrada (só quando SQS_ENABLED).
func sqsModule(cfg config.Config) fx.Option {
	if !cfg.SQS.Enabled {
		return fx.Options()
	}
	return fx.Module("sqs",
		fx.Provide(
			func() *sqsQueues { return &sqsQueues{} },
			fx.Annotate(
				func(c *sqs.Client, q *sqsQueues) httpapi.Checker {
					return sqsconsumer.HealthChecker{Client: c, QueueURL: q.inputURL}
				},
				fx.ResultTags(`group:"readiness"`),
			),
		),
		fx.Invoke(registerSQSConsumer, registerQueueDepth),
	)
}

// sqsQueues guarda as URLs resolvidas na partida.
type sqsQueues struct {
	mu         sync.RWMutex
	input, dlq string
}

func (q *sqsQueues) inputURL() string {
	q.mu.RLock()
	defer q.mu.RUnlock()
	return q.input
}

func (q *sqsQueues) urls() (input, dlq string) {
	q.mu.RLock()
	defer q.mu.RUnlock()
	return q.input, q.dlq
}

// registerQueueDepth expõe quantas mensagens esperam na fila de entrada e
// na DLQ, lidas do SQS a cada coleta. A DLQ inclui o que o próprio SQS move
// pela redrive policy, que o consumidor não vê.
func registerQueueDepth(p *observability.Prometheus, client *sqs.Client, queues *sqsQueues) error {
	return p.RegisterQueueDepth(func(ctx context.Context) (map[string]int64, error) {
		input, dlq := queues.urls()
		if input == "" || dlq == "" {
			return nil, errors.New("sqs queues not resolved yet")
		}
		depths := map[string]int64{}
		for name, url := range map[string]string{"input": input, "dlq": dlq} {
			n, err := sqsconsumer.QueueDepth(ctx, client, url)
			if err != nil {
				return nil, err
			}
			depths[name] = n
		}
		return depths, nil
	}, collectTimeout)
}

func registerSQSConsumer(lc fx.Lifecycle, cfg config.Config, client *sqs.Client, queues *sqsQueues,
	svc *app.WagerService, metrics sqsconsumer.Metrics, log *slog.Logger) {
	var consumer *sqsconsumer.Consumer
	lc.Append(fx.Hook{
		// valida a dependência na partida: as filas precisam existir
		OnStart: func(ctx context.Context) error {
			input, err := sqsconsumer.ResolveQueueURL(ctx, client, cfg.SQS.InputQueue)
			if err != nil {
				return err
			}
			dlq, err := sqsconsumer.ResolveQueueURL(ctx, client, cfg.SQS.DLQ)
			if err != nil {
				return err
			}
			queues.mu.Lock()
			queues.input, queues.dlq = input, dlq
			queues.mu.Unlock()

			allowed := map[string]bool{}
			for _, p := range cfg.SQS.AllowedProviders {
				allowed[p] = true
			}
			consumer = sqsconsumer.New(sqsconsumer.Config{
				ConsumerName: cfg.SQS.ConsumerName, QueueURL: input, DLQURL: dlq,
				Pollers: cfg.SQS.Pollers, MaxMessages: int32(cfg.SQS.MaxMessages), WaitTime: cfg.SQS.WaitTime,
				ItemTimeout: cfg.HTTP.RequestTimeout, RetryBaseDelay: cfg.SQS.RetryBaseDelay,
				RetryMaxDelay: cfg.SQS.RetryMaxDelay, AllowedProviders: allowed, Metrics: metrics,
			}, client, svc, log, sqsconsumer.Hooks{})
			return consumer.Start(ctx)
		},
		OnStop: func(ctx context.Context) error {
			if consumer == nil {
				return nil
			}
			return consumer.Stop(ctx)
		},
	})
}
