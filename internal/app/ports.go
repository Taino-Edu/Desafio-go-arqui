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
	// Reader devolve repositórios fora de transação, para consultas.
	Reader() Repositories
}

// Repositories agrupa os repositórios de um mesmo contexto transacional.
type Repositories interface {
	Wallets() WalletRepository
	Transactions() TransactionRepository
	Ledger() LedgerRepository
	Outbox() OutboxRepository
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
}

// LedgerRepository persiste e lê lançamentos (append-only).
type LedgerRepository interface {
	Insert(ctx context.Context, e wallet.LedgerEntry) error
	// List devolve até limit lançamentos com wallet_version > afterVersion,
	// em ordem crescente de versão.
	List(ctx context.Context, walletID uuid.UUID, afterVersion int64, limit int) ([]wallet.LedgerEntry, error)
}

// OutboxRepository grava eventos a publicar.
type OutboxRepository interface {
	Append(ctx context.Context, records ...OutboxRecord) error
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
