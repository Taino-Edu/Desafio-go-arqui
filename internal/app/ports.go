// Package app contém os casos de uso. Orquestra o domínio e define as
// interfaces (portas) que os adaptadores implementam. Não importa Fx, HTTP,
// SQS nem bibliotecas de banco.
package app

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/Taino-Edu/Desafio-go-arqui/internal/domain/wagering"
	"github.com/Taino-Edu/Desafio-go-arqui/internal/domain/wallet"
)

// Store dá acesso aos repositórios, dentro ou fora de uma transação SQL.
//
// WithinTx abre UMA transação e entrega repositórios que compartilham essa
// transação: tudo o que fn gravar é confirmado junto no COMMIT, ou desfeito
// junto se fn devolver erro. É assim que saldo, ledger, estado da operação e
// outbox ficam atômicos.
type Store interface {
	WithinTx(ctx context.Context, fn func(ctx context.Context, r Repositories) error) error
	// ReadSnapshot roda fn numa transação SOMENTE LEITURA em que todas as
	// consultas enxergam a mesma foto do banco (REPEATABLE READ), mesmo que
	// outras transações confirmem no meio. Qualquer escrita falha.
	ReadSnapshot(ctx context.Context, fn func(ctx context.Context, r Repositories) error) error
	// Reader devolve repositórios fora de transação, para consultas.
	Reader() Repositories
}

// Repositories agrupa os repositórios de um mesmo contexto transacional.
type Repositories interface {
	Wallets() WalletRepository
	Transactions() TransactionRepository
	Ledger() LedgerRepository
	Outbox() OutboxRepository
	Inbox() InboxRepository
}

// WalletRepository persiste carteiras.
type WalletRepository interface {
	// Insert grava uma carteira nova. ErrWalletAlreadyExists se o par
	// (jogador, moeda) já tem carteira.
	Insert(ctx context.Context, w *wallet.Wallet) error
	// Get lê uma carteira. ErrWalletNotFound se não existir.
	Get(ctx context.Context, id uuid.UUID) (*wallet.Wallet, error)
	// GetForUpdate lê e trava a linha até o fim da transação (SELECT ... FOR UPDATE).
	GetForUpdate(ctx context.Context, id uuid.UUID) (*wallet.Wallet, error)
	// Update grava saldo, versão e updated_at.
	Update(ctx context.Context, w *wallet.Wallet) error
}

// TransactionRepository persiste operações financeiras.
type TransactionRepository interface {
	// Insert grava uma transação nova (usado pela abertura interna).
	Insert(ctx context.Context, t *wagering.WagerTransaction) error
	// InsertIfAbsent grava a transação se não houver outra com o mesmo
	// (provedor, chave) ou (provedor, id externo). Se uma concorrente ainda
	// não confirmada ocupa a chave, ESPERA o desfecho dela. Devolve false
	// quando já existia.
	InsertIfAbsent(ctx context.Context, t *wagering.WagerTransaction) (inserted bool, err error)
	// FindExisting busca a transação que ocupa a chave ou o id externo do
	// provedor (prioriza a chave). nil se nenhuma.
	FindExisting(ctx context.Context, providerID, idempotencyKey, externalID string) (*wagering.WagerTransaction, error)
	// GetByID e GetByExternalID devolvem ErrTransactionNotFound se não existir.
	GetByID(ctx context.Context, id uuid.UUID) (*wagering.WagerTransaction, error)
	GetByExternalID(ctx context.Context, providerID, externalID string) (*wagering.WagerTransaction, error)
	// Update grava o novo estado (status, resultado, tentativas).
	Update(ctx context.Context, t *wagering.WagerTransaction) error
	// HasProcessedReversal informa se a transação já tem REFUND ou ROLLBACK
	// concluído apontando para ela.
	HasProcessedReversal(ctx context.Context, referenceID uuid.UUID) (bool, error)
	// ClaimDuePendingReference trava e devolve UMA operação em
	// PENDING_REFERENCE cuja próxima tentativa já venceu, pulando as que
	// outra instância já travou (SKIP LOCKED). nil se não houver nenhuma.
	ClaimDuePendingReference(ctx context.Context, now time.Time) (*wagering.WagerTransaction, error)
	// NudgePendingReferences antecipa para agora a próxima tentativa das
	// pendências que esperam (providerID, externalID). Não espera por linhas
	// travadas por outra transação (SKIP LOCKED), então não causa deadlock.
	NudgePendingReferences(ctx context.Context, providerID, externalID string, now time.Time) error
	// CountPendingReferences conta as operações em PENDING_REFERENCE.
	CountPendingReferences(ctx context.Context) (int64, error)
}

// LedgerRepository persiste e lê lançamentos (append-only).
type LedgerRepository interface {
	Insert(ctx context.Context, e wallet.LedgerEntry) error
	// List devolve até limit lançamentos com wallet_version > afterVersion,
	// em ordem crescente de versão.
	List(ctx context.Context, walletID uuid.UUID, afterVersion int64, limit int) ([]wallet.LedgerEntry, error)
	// Totals soma TODOS os lançamentos da carteira, abertura incluída.
	Totals(ctx context.Context, walletID uuid.UUID) (LedgerTotals, error)
}

// LedgerTotals resume o ledger de uma carteira, em unidades mínimas.
type LedgerTotals struct {
	Credits int64 // Σ créditos
	Debits  int64 // Σ débitos
	Entries int64 // quantidade de lançamentos
}

// InboxEntry é o registro de uma mensagem recebida por um consumidor.
type InboxEntry struct {
	PayloadHash string
	Completed   bool
}

// InboxRepository deduplica mensagens por (consumidor, messageId).
type InboxRepository interface {
	// Register grava a mensagem se for nova. Se outro processo estiver
	// tratando a mesma mensagem (transação aberta), ESPERA o desfecho.
	// Devolve o registro existente quando já havia um (inserted = false).
	Register(ctx context.Context, consumer, messageID, payloadHash string, now time.Time) (existing InboxEntry, inserted bool, err error)
	// Complete marca a conclusão durável do tratamento.
	Complete(ctx context.Context, consumer, messageID string, now time.Time) error
}

// OutboxRepository grava eventos a publicar e controla a publicação.
type OutboxRepository interface {
	Append(ctx context.Context, records ...OutboxRecord) error

	// Claim reivindica eventos prontos para publicar, com um arrendamento
	// (lease) até now+lease em nome de owner. Escolhe até heads agregados cujo
	// evento pendente mais antigo (a "cabeça") está pronto, pulando os que
	// outra instância já travou (SKIP LOCKED) e retomando arrendamentos
	// vencidos (trabalho abandonado). Quem fica com a cabeça é dono do
	// agregado nesta rodada: leva também os eventos seguintes dele, em
	// sequência, até perAggregate no total. Devolve em ordem de gravação.
	Claim(ctx context.Context, owner string, now time.Time, lease time.Duration, heads, perAggregate int) ([]OutboxMessage, error)
	// Release devolve eventos arrendados por owner e ainda não publicados,
	// sem agendar nova tentativa (ficam prontos para a próxima rodada).
	Release(ctx context.Context, owner string, eventIDs []uuid.UUID) error
	// MarkPublished registra a publicação. Não faz nada se já estiver
	// publicado (outra instância concluiu primeiro).
	MarkPublished(ctx context.Context, eventID uuid.UUID, now time.Time) error
	// MarkPublishedMany faz o mesmo para vários eventos, num comando só.
	MarkPublishedMany(ctx context.Context, eventIDs []uuid.UUID, now time.Time) error
	// MarkFailed libera o evento para nova tentativa em nextAttemptAt.
	MarkFailed(ctx context.Context, eventID uuid.UUID, owner string, nextAttemptAt time.Time, cause string) error
	// Backlog conta os eventos não publicados e devolve o occurred_at do mais
	// antigo deles (nil quando não há nenhum).
	Backlog(ctx context.Context) (pending int64, oldest *time.Time, err error)
}

// OutboxMessage é um evento reivindicado para publicação.
type OutboxMessage struct {
	EventID       uuid.UUID
	AggregateType string
	AggregateID   uuid.UUID
	EventType     string
	EventVersion  int
	CorrelationID string
	Payload       []byte
	OccurredAt    time.Time
	Attempts      int // já contando esta tentativa
}

// EventPublisher entrega um evento ao destino externo (SQS).
type EventPublisher interface {
	Publish(ctx context.Context, m OutboxMessage) error
}

// BatchPublisher é opcional: entrega vários eventos numa chamada e devolve um
// erro por evento (nil = entregue), na mesma ordem. O publicador da outbox
// só manda num mesmo lote eventos de agregados diferentes.
type BatchPublisher interface {
	PublishBatch(ctx context.Context, msgs []OutboxMessage) []error
}

// OutboxRecord é um evento serializado pronto para a tabela outbox_events.
type OutboxRecord struct {
	EventID       uuid.UUID
	AggregateType string // "wallet" ou "wager_transaction"
	AggregateID   uuid.UUID
	EventType     string
	EventVersion  int
	CorrelationID string
	CausationID   *uuid.UUID
	Payload       []byte // envelope JSON completo (snapshot imutável)
	OccurredAt    time.Time
}

// Clock e IDGenerator são injetados para que os testes sejam determinísticos.
type Clock interface{ Now() time.Time }

type IDGenerator interface{ NewID() (uuid.UUID, error) }

// SystemClock usa o relógio do sistema, em UTC, truncado em microssegundos:
// é a precisão do TIMESTAMPTZ do Postgres. Assim o instante devolvido na
// criação é idêntico ao lido depois (inclusive em replays).
type SystemClock struct{}

func (SystemClock) Now() time.Time { return time.Now().UTC().Truncate(time.Microsecond) }

// UUIDv7 gera UUIDs versão 7 (ordenáveis pelo tempo, bons para índices).
type UUIDv7 struct{}

func (UUIDv7) NewID() (uuid.UUID, error) { return uuid.NewV7() }
