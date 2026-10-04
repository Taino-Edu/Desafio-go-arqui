package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Taino-Edu/Desafio-go-arqui/internal/app"
	"github.com/Taino-Edu/Desafio-go-arqui/internal/domain/money"
	"github.com/Taino-Edu/Desafio-go-arqui/internal/domain/wagering"
	"github.com/Taino-Edu/Desafio-go-arqui/internal/domain/wallet"
)

const rollbackTimeout = 5 * time.Second

// ---------------------------------------------------------------------
// wallets
// ---------------------------------------------------------------------

type walletRepo struct{ q querier }

func (r walletRepo) Insert(ctx context.Context, w *wallet.Wallet) error {
	_, err := r.q.Exec(ctx, `
		INSERT INTO wallets (id, player_id, currency, balance_minor, version, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		w.ID(), w.PlayerID(), w.Currency().Code(), w.Balance().MinorUnits(), w.Version(),
		w.CreatedAt(), w.UpdatedAt())
	if c, ok := uniqueViolation(err); ok && c == "wallets_player_currency_uq" {
		return app.ErrWalletAlreadyExists
	}
	return classify(err)
}

const selectWallet = `
	SELECT id, player_id, currency, balance_minor, version, created_at, updated_at
	  FROM wallets WHERE id = $1`

func (r walletRepo) Get(ctx context.Context, id uuid.UUID) (*wallet.Wallet, error) {
	return scanWallet(r.q.QueryRow(ctx, selectWallet, id))
}

func (r walletRepo) GetForUpdate(ctx context.Context, id uuid.UUID) (*wallet.Wallet, error) {
	return scanWallet(r.q.QueryRow(ctx, selectWallet+` FOR UPDATE`, id))
}

// Update grava o novo saldo. A condição "version = nova - 1" é uma segunda
// barreira contra lost update (além do FOR UPDATE): se outro escritor tiver
// confirmado antes, nenhuma linha é afetada e nada é sobrescrito.
func (r walletRepo) Update(ctx context.Context, w *wallet.Wallet) error {
	tag, err := r.q.Exec(ctx, `
		UPDATE wallets SET balance_minor = $2, version = $3, updated_at = $4
		 WHERE id = $1 AND version = $3 - 1`,
		w.ID(), w.Balance().MinorUnits(), w.Version(), w.UpdatedAt())
	if err != nil {
		return classify(err)
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("%w: wallet %s was modified concurrently", app.ErrTransient, w.ID())
	}
	return nil
}

func scanWallet(row pgx.Row) (*wallet.Wallet, error) {
	var (
		id, player       uuid.UUID
		currency         string
		balance, version int64
		created, updated time.Time
	)
	if err := row.Scan(&id, &player, &currency, &balance, &version, &created, &updated); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, app.ErrWalletNotFound
		}
		return nil, classify(err)
	}
	cur, err := money.NewCurrency(currency)
	if err != nil {
		return nil, err
	}
	bal, err := money.FromMinorUnits(balance, cur)
	if err != nil {
		return nil, err
	}
	return wallet.Rehydrate(wallet.RehydrateParams{
		ID: id, PlayerID: player, Balance: bal, Version: version, CreatedAt: created, UpdatedAt: updated,
	})
}

// ---------------------------------------------------------------------
// wager_transactions
// ---------------------------------------------------------------------

type transactionRepo struct{ q querier }

func (r transactionRepo) Insert(ctx context.Context, t *wagering.WagerTransaction) error {
	var (
		provider, extID, key, hash, round, game, refExt *string
	)
	if ext := t.External(); ext != nil {
		provider, extID, key, hash = &ext.ProviderID, &ext.ExternalTransactionID, &ext.IdempotencyKey, &ext.PayloadHash
		round, game = &ext.RoundID, &ext.GameID
		if ext.HasReference() {
			refExt = &ext.ReferenceExternalTransactionID
		}
	}
	var refID *uuid.UUID
	if id, ok := t.ReferenceTransactionID(); ok {
		refID = &id
	}
	var failure *string
	if fc := t.FailureCode(); fc != "" {
		s := string(fc)
		failure = &s
	}
	var balanceAfter *int64
	if b, ok := t.BalanceAfter(); ok {
		v := b.MinorUnits()
		balanceAfter = &v
	}
	var next *time.Time
	if n, ok := t.NextAttemptAt(); ok {
		next = &n
	}

	_, err := r.q.Exec(ctx, `
		INSERT INTO wager_transactions (
			id, origin, kind, status, wallet_id, player_id, amount_minor, currency,
			provider_id, external_transaction_id, idempotency_key, payload_hash,
			round_id, game_id, reference_external_transaction_id, reference_transaction_id,
			failure_code, balance_after_minor, attempts, next_attempt_at, created_at, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22)`,
		t.ID(), string(t.Origin()), string(t.Kind()), string(t.Status()), t.WalletID(), t.PlayerID(),
		t.Amount().MinorUnits(), t.Amount().Currency().Code(),
		provider, extID, key, hash, round, game, refExt, refID,
		failure, balanceAfter, t.Attempts(), next, t.CreatedAt(), t.UpdatedAt())
	return classify(err)
}

// ---------------------------------------------------------------------
// wallet_ledger_entries
// ---------------------------------------------------------------------

type ledgerRepo struct{ q querier }

func (r ledgerRepo) Insert(ctx context.Context, e wallet.LedgerEntry) error {
	_, err := r.q.Exec(ctx, `
		INSERT INTO wallet_ledger_entries (
			id, wallet_id, transaction_id, wallet_version, direction, amount_minor, currency,
			balance_before_minor, balance_after_minor, created_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`,
		e.ID(), e.WalletID(), e.TransactionID(), e.WalletVersion(), string(e.Direction()),
		e.Amount().MinorUnits(), e.Amount().Currency().Code(),
		e.BalanceBefore().MinorUnits(), e.BalanceAfter().MinorUnits(), e.CreatedAt())
	return classify(err)
}

func (r ledgerRepo) List(ctx context.Context, walletID uuid.UUID, afterVersion int64, limit int) ([]wallet.LedgerEntry, error) {
	rows, err := r.q.Query(ctx, `
		SELECT id, wallet_id, transaction_id, wallet_version, direction, amount_minor, currency,
		       balance_before_minor, balance_after_minor, created_at
		  FROM wallet_ledger_entries
		 WHERE wallet_id = $1 AND wallet_version > $2
		 ORDER BY wallet_version
		 LIMIT $3`, walletID, afterVersion, limit)
	if err != nil {
		return nil, classify(err)
	}
	defer rows.Close()

	var out []wallet.LedgerEntry
	for rows.Next() {
		var (
			id, wid, txID                uuid.UUID
			version, amount, before, aft int64
			direction, currency          string
			created                      time.Time
		)
		if err := rows.Scan(&id, &wid, &txID, &version, &direction, &amount, &currency, &before, &aft, &created); err != nil {
			return nil, classify(err)
		}
		cur, err := money.NewCurrency(currency)
		if err != nil {
			return nil, err
		}
		amt, _ := money.FromMinorUnits(amount, cur)
		bef, _ := money.FromMinorUnits(before, cur)
		af, _ := money.FromMinorUnits(aft, cur)
		e, err := wallet.NewLedgerEntry(wallet.LedgerEntryParams{
			ID: id, WalletID: wid, TransactionID: txID, WalletVersion: version,
			Direction: wallet.Direction(direction), Amount: amt, BalanceBefore: bef, BalanceAfter: af, CreatedAt: created,
		})
		if err != nil {
			return nil, fmt.Errorf("ledger entry %s: %w", id, err)
		}
		out = append(out, e)
	}
	return out, classify(rows.Err())
}

// ---------------------------------------------------------------------
// outbox_events
// ---------------------------------------------------------------------

type outboxRepo struct{ q querier }

// Append grava os eventos já serializados. next_attempt_at = occurred_at:
// ficam disponíveis para o publisher assim que a transação confirmar.
func (r outboxRepo) Append(ctx context.Context, records ...app.OutboxRecord) error {
	for _, rec := range records {
		_, err := r.q.Exec(ctx, `
			INSERT INTO outbox_events (
				event_id, aggregate_type, aggregate_id, event_type, event_version,
				correlation_id, causation_id, payload, occurred_at, next_attempt_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$9)`,
			rec.EventID, rec.AggregateType, rec.AggregateID, rec.EventType, rec.EventVersion,
			rec.CorrelationID, rec.CausationID, rec.Payload, rec.OccurredAt)
		if err != nil {
			return classify(err)
		}
	}
	return nil
}
