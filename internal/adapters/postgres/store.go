package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Taino-Edu/Desafio-go-arqui/internal/app"
)

// querier é o que pgxpool.Pool e pgx.Tx têm em comum. Os repositórios
// recebem um ou outro: dentro de WithinTx todos usam a MESMA pgx.Tx.
type querier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Store implementa app.Store sobre um pgxpool.
type Store struct {
	pool *pgxpool.Pool
}

var _ app.Store = (*Store)(nil)

func NewStore(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

// WithinTx delimita a transação SQL: BEGIN, fn, COMMIT. Se fn devolver erro
// ou o COMMIT falhar (por exemplo, a checagem adiada saldo = ledger), tudo é
// desfeito. Isolamento READ COMMITTED: a proteção contra lost update vem do
// lock de linha (FOR UPDATE) e da condição de versão no UPDATE.
func (s *Store) WithinTx(ctx context.Context, fn func(ctx context.Context, r app.Repositories) error) (err error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return classify(fmt.Errorf("begin: %w", err))
	}
	defer func() {
		if err != nil {
			// usa um contexto próprio: o ctx original pode já ter expirado
			rbCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), rollbackTimeout)
			defer cancel()
			if rbErr := tx.Rollback(rbCtx); rbErr != nil && !errors.Is(rbErr, pgx.ErrTxClosed) {
				err = errors.Join(err, fmt.Errorf("rollback: %w", rbErr))
			}
		}
	}()

	if err := fn(ctx, repositories{q: tx}); err != nil {
		return classify(err)
	}
	if err := tx.Commit(ctx); err != nil {
		return classify(fmt.Errorf("commit: %w", err))
	}
	return nil
}

// ReadSnapshot abre uma transação REPEATABLE READ somente leitura: a foto do
// banco é tirada na primeira consulta e vale para todas as seguintes, mesmo
// que outras transações confirmem no meio. Termina com ROLLBACK: não há nada
// a confirmar, e qualquer tentativa de escrita já teria falhado.
func (s *Store) ReadSnapshot(ctx context.Context, fn func(ctx context.Context, r app.Repositories) error) error {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return classify(fmt.Errorf("begin: %w", err))
	}
	defer func() {
		rbCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), rollbackTimeout)
		defer cancel()
		_ = tx.Rollback(rbCtx)
	}()
	return classify(fn(ctx, repositories{q: tx}))
}

// Reader devolve repositórios que usam o pool diretamente (sem transação).
func (s *Store) Reader() app.Repositories { return repositories{q: s.pool} }

type repositories struct{ q querier }

func (r repositories) Wallets() app.WalletRepository           { return walletRepo(r) }
func (r repositories) Transactions() app.TransactionRepository { return transactionRepo(r) }
func (r repositories) Ledger() app.LedgerRepository            { return ledgerRepo(r) }
func (r repositories) Journal() app.JournalRepository          { return journalRepo(r) }
func (r repositories) Outbox() app.OutboxRepository            { return outboxRepo(r) }
func (r repositories) Inbox() app.InboxRepository              { return inboxRepo(r) }
