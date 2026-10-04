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

// ---------------------------------------------------------------------
// wager_transactions: idempotência, leitura e mudança de estado
// ---------------------------------------------------------------------

const txColumns = `
	id, origin, kind, status, wallet_id, player_id, amount_minor, currency,
	provider_id, external_transaction_id, idempotency_key, payload_hash,
	round_id, game_id, reference_external_transaction_id, reference_transaction_id,
	failure_code, balance_after_minor, attempts, next_attempt_at, created_at, updated_at`

// InsertIfAbsent usa ON CONFLICT DO NOTHING sobre os índices únicos
// (provedor, chave) e (provedor, id externo). Se a linha conflitante pertence
// a uma transação ainda aberta, o Postgres ESPERA ela terminar: se ela
// confirmar, este INSERT não faz nada; se ela for desfeita, este INSERT
// acontece. É isso que serializa requisições duplicadas simultâneas.
func (r transactionRepo) InsertIfAbsent(ctx context.Context, t *wagering.WagerTransaction) (bool, error) {
	ext := t.External()
	if ext == nil {
		return false, fmt.Errorf("InsertIfAbsent requires an external transaction")
	}
	var refExt *string
	if ext.HasReference() {
		refExt = &ext.ReferenceExternalTransactionID
	}
	tag, err := r.q.Exec(ctx, `
		INSERT INTO wager_transactions (
			id, origin, kind, status, wallet_id, player_id, amount_minor, currency,
			provider_id, external_transaction_id, idempotency_key, payload_hash,
			round_id, game_id, reference_external_transaction_id, attempts, created_at, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18)
		ON CONFLICT DO NOTHING`,
		t.ID(), string(t.Origin()), string(t.Kind()), string(t.Status()), t.WalletID(), t.PlayerID(),
		t.Amount().MinorUnits(), t.Amount().Currency().Code(),
		ext.ProviderID, ext.ExternalTransactionID, ext.IdempotencyKey, ext.PayloadHash,
		ext.RoundID, ext.GameID, refExt, t.Attempts(), t.CreatedAt(), t.UpdatedAt())
	if err != nil {
		return false, classify(err)
	}
	return tag.RowsAffected() == 1, nil
}

func (r transactionRepo) FindExisting(ctx context.Context, providerID, key, externalID string) (*wagering.WagerTransaction, error) {
	t, err := scanTransaction(r.q.QueryRow(ctx, `SELECT `+txColumns+`
		  FROM wager_transactions
		 WHERE provider_id = $1 AND (idempotency_key = $2 OR external_transaction_id = $3)
		 ORDER BY (idempotency_key = $2) DESC
		 LIMIT 1`, providerID, key, externalID))
	if errors.Is(err, app.ErrTransactionNotFound) {
		return nil, nil
	}
	return t, err
}

func (r transactionRepo) GetByID(ctx context.Context, id uuid.UUID) (*wagering.WagerTransaction, error) {
	return scanTransaction(r.q.QueryRow(ctx, `SELECT `+txColumns+` FROM wager_transactions WHERE id = $1`, id))
}

func (r transactionRepo) GetByExternalID(ctx context.Context, providerID, externalID string) (*wagering.WagerTransaction, error) {
	return scanTransaction(r.q.QueryRow(ctx, `SELECT `+txColumns+`
		  FROM wager_transactions WHERE provider_id = $1 AND external_transaction_id = $2`, providerID, externalID))
}

// Update grava o desfecho. O trigger do banco recusa transições inválidas e
// qualquer mudança em estado terminal.
func (r transactionRepo) Update(ctx context.Context, t *wagering.WagerTransaction) error {
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
	tag, err := r.q.Exec(ctx, `
		UPDATE wager_transactions
		   SET status = $2, failure_code = $3, balance_after_minor = $4, reference_transaction_id = $5,
		       attempts = $6, next_attempt_at = $7, updated_at = $8
		 WHERE id = $1`,
		t.ID(), string(t.Status()), failure, balanceAfter, refID, t.Attempts(), next, t.UpdatedAt())
	if c, ok := uniqueViolation(err); ok && c == "wager_tx_one_reversal_per_reference" {
		// outra reversão da mesma referência confirmou antes: a nova tentativa
		// verá a reversão existente e rejeitará com ALREADY_REVERSED
		return fmt.Errorf("%w: reference reversed concurrently", app.ErrTransient)
	}
	if err != nil {
		return classify(err)
	}
	if tag.RowsAffected() != 1 {
		return app.ErrTransactionNotFound
	}
	return nil
}

func (r transactionRepo) HasProcessedReversal(ctx context.Context, referenceID uuid.UUID) (bool, error) {
	var exists bool
	err := r.q.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM wager_transactions
			 WHERE reference_transaction_id = $1
			   AND kind IN ('REFUND', 'ROLLBACK') AND status = 'PROCESSED')`, referenceID).Scan(&exists)
	return exists, classify(err)
}

func scanTransaction(row pgx.Row) (*wagering.WagerTransaction, error) {
	var (
		id, walletID, playerID                  uuid.UUID
		origin, kind, status, currency          string
		amount                                  int64
		provider, extID, key, hash, round, game *string
		refExt, failure                         *string
		refID                                   *uuid.UUID
		balanceAfter                            *int64
		attempts                                int
		next                                    *time.Time
		created, updated                        time.Time
	)
	err := row.Scan(&id, &origin, &kind, &status, &walletID, &playerID, &amount, &currency,
		&provider, &extID, &key, &hash, &round, &game, &refExt, &refID,
		&failure, &balanceAfter, &attempts, &next, &created, &updated)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, app.ErrTransactionNotFound
	}
	if err != nil {
		return nil, classify(err)
	}

	cur, err := money.NewCurrency(currency)
	if err != nil {
		return nil, err
	}
	amt, _ := money.FromMinorUnits(amount, cur)
	p := wagering.RehydrateParams{
		ID: id, Origin: wagering.Origin(origin), Kind: wagering.Kind(kind), Status: wagering.Status(status),
		WalletID: walletID, PlayerID: playerID, Amount: amt, ReferenceTransactionID: refID,
		Attempts: attempts, NextAttemptAt: next, CreatedAt: created, UpdatedAt: updated,
	}
	if provider != nil {
		p.External = &wagering.External{
			ProviderID: *provider, ExternalTransactionID: deref(extID), IdempotencyKey: deref(key),
			PayloadHash: deref(hash), RoundID: deref(round), GameID: deref(game),
			ReferenceExternalTransactionID: deref(refExt),
		}
	}
	if failure != nil {
		p.FailureCode = wagering.FailureCode(*failure)
	}
	if balanceAfter != nil {
		b, _ := money.FromMinorUnits(*balanceAfter, cur)
		p.BalanceAfter = &b
	}
	return wagering.Rehydrate(p)
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// ClaimDuePendingReference: FOR UPDATE SKIP LOCKED faz cada instância pegar
// uma pendência diferente, sem esperar pelas que outra já está processando.
func (r transactionRepo) ClaimDuePendingReference(ctx context.Context, now time.Time) (*wagering.WagerTransaction, error) {
	t, err := scanTransaction(r.q.QueryRow(ctx, `SELECT `+txColumns+`
		  FROM wager_transactions
		 WHERE status = 'PENDING_REFERENCE' AND next_attempt_at <= $1
		 ORDER BY next_attempt_at
		 LIMIT 1
		 FOR UPDATE SKIP LOCKED`, now))
	if errors.Is(err, app.ErrTransactionNotFound) {
		return nil, nil
	}
	return t, err
}

func (r transactionRepo) NudgePendingReferences(ctx context.Context, providerID, externalID string, now time.Time) error {
	_, err := r.q.Exec(ctx, `
		UPDATE wager_transactions SET next_attempt_at = $3, updated_at = GREATEST(updated_at, $3)
		 WHERE id IN (
			SELECT id FROM wager_transactions
			 WHERE provider_id = $1 AND reference_external_transaction_id = $2
			   AND status = 'PENDING_REFERENCE' AND next_attempt_at > $3
			 FOR UPDATE SKIP LOCKED)`, providerID, externalID, now)
	return classify(err)
}

// ---------------------------------------------------------------------
// inbox_messages
// ---------------------------------------------------------------------

type inboxRepo struct{ q querier }

// Register: o INSERT espera se outra transação aberta já registrou o mesmo
// (consumidor, messageId). Quando não insere, lê o registro travando-o.
func (r inboxRepo) Register(ctx context.Context, consumer, messageID, hash string, now time.Time) (app.InboxEntry, bool, error) {
	tag, err := r.q.Exec(ctx, `
		INSERT INTO inbox_messages (consumer_name, message_id, payload_hash, received_at)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (consumer_name, message_id) DO NOTHING`, consumer, messageID, hash, now)
	if err != nil {
		return app.InboxEntry{}, false, classify(err)
	}
	if tag.RowsAffected() == 1 {
		return app.InboxEntry{}, true, nil
	}
	var e app.InboxEntry
	var completed *time.Time
	err = r.q.QueryRow(ctx, `
		SELECT payload_hash, completed_at FROM inbox_messages
		 WHERE consumer_name = $1 AND message_id = $2 FOR UPDATE`, consumer, messageID).Scan(&e.PayloadHash, &completed)
	if err != nil {
		return app.InboxEntry{}, false, classify(err)
	}
	e.Completed = completed != nil
	return e, false, nil
}

func (r inboxRepo) Complete(ctx context.Context, consumer, messageID string, now time.Time) error {
	_, err := r.q.Exec(ctx, `
		UPDATE inbox_messages SET completed_at = GREATEST($3, received_at)
		 WHERE consumer_name = $1 AND message_id = $2`, consumer, messageID, now)
	return classify(err)
}
