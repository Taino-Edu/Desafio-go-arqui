package app

import "time"

// Metrics é a porta de métricas de negócio e de confiabilidade. A
// implementação (Prometheus) fica no adaptador de observabilidade; os casos
// de uso não conhecem a biblioteca.
type Metrics interface {
	// WagerOutcome: operação NOVA com desfecho gravado, por porta de entrada
	// (source), tipo e status. Replays não entram aqui.
	WagerOutcome(source, kind, status string)
	// IdempotentReplay: repetição respondida com o resultado salvo.
	IdempotentReplay(source string)
	// IdempotencyConflict: chave reutilizada com outro conteúdo
	// (ConflictKeyReused) ou operação já registrada com outra chave
	// (ConflictDuplicateTransaction).
	IdempotencyConflict(source, reason string)
	// ProcessingDuration: latência do caso de uso, incluindo esperas por lock
	// e novas tentativas.
	ProcessingDuration(source string, d time.Duration)
	// TransientRetry: nova tentativa automática depois de falha transitória.
	TransientRetry(source string)
	// ConcurrencyConflict: disputa de escrita detectada no banco (lock_timeout,
	// deadlock, falha de serialização, versão desatualizada).
	ConcurrencyConflict(source string)
	// ReferenceResolution: rodada do worker de referências (RESOLVED,
	// RESCHEDULED, EXPIRED).
	ReferenceResolution(outcome string)
	// OutboxPublished e OutboxFailed: resultado da publicação de eventos.
	OutboxPublished(n int)
	OutboxFailed(n int)
	// Reconciliation: resultado de uma reconciliação.
	Reconciliation(consistent bool)
}

// Portas de entrada (rótulo "source").
const (
	SourceHTTP   = "http"
	SourceSQS    = "sqs"
	SourceWorker = "worker"
)

// Motivos de conflito de idempotência (rótulo "reason").
const (
	ConflictKeyReused            = "key_reused"
	ConflictDuplicateTransaction = "duplicate_transaction"
)

// NopMetrics descarta tudo. É o padrão quando nenhuma implementação é
// injetada (testes, ferramentas).
type NopMetrics struct{}

func (NopMetrics) WagerOutcome(string, string, string)      {}
func (NopMetrics) IdempotentReplay(string)                  {}
func (NopMetrics) IdempotencyConflict(string, string)       {}
func (NopMetrics) ProcessingDuration(string, time.Duration) {}
func (NopMetrics) TransientRetry(string)                    {}
func (NopMetrics) ConcurrencyConflict(string)               {}
func (NopMetrics) ReferenceResolution(string)               {}
func (NopMetrics) OutboxPublished(int)                      {}
func (NopMetrics) OutboxFailed(int)                         {}
func (NopMetrics) Reconciliation(bool)                      {}

// Option configura um serviço da camada de aplicação. Opções são o jeito de
// acrescentar dependências opcionais sem quebrar os construtores existentes.
type Option func(*options)

type options struct {
	metrics Metrics
}

// WithMetrics injeta a implementação de métricas (nil mantém NopMetrics).
func WithMetrics(m Metrics) Option {
	return func(o *options) {
		if m != nil {
			o.metrics = m
		}
	}
}

func applyOptions(opts []Option) options {
	o := options{metrics: NopMetrics{}}
	for _, opt := range opts {
		opt(&o)
	}
	return o
}
