//go:build integration

// Testes do schema contra um PostgreSQL real. Cada teste tenta violar uma
// invariante e confere que o próprio banco recusa, independentemente do
// código Go. Rodar:
//
//	docker compose up -d postgres
//	go test -tags=integration ./internal/adapters/postgres/...
package postgres_test

import (
	"context"
	"errors"
	"io/fs"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Taino-Edu/Desafio-go-arqui/internal/adapters/postgres"
	"github.com/Taino-Edu/Desafio-go-arqui/internal/testsupport/pgtest"
	"github.com/Taino-Edu/Desafio-go-arqui/migrations"
)

// Códigos SQLSTATE usados nas asserções.
const (
	checkViolation      = "23514"
	uniqueViolation     = "23505"
	foreignKeyViolation = "23503"
	notNullViolation    = "23502"
	insufficientPriv    = "42501"
)

var t0 = time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)

func ctx(t *testing.T) context.Context {
	c, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	return c
}

// wantCode falha o teste se err não for um erro do Postgres com o código esperado.
func wantCode(t *testing.T, err error, code string) {
	t.Helper()
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		t.Fatalf("esperava erro SQLSTATE %s, veio: %v", code, err)
	}
	if pgErr.Code != code {
		t.Fatalf("esperava SQLSTATE %s, veio %s: %s", code, pgErr.Code, pgErr.Message)
	}
}

func mustExec(t *testing.T, q interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
}, sql string, args ...any) {
	t.Helper()
	if _, err := q.Exec(ctx(t), sql, args...); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}

// inTx executa fn numa transação e devolve o erro de fn ou do COMMIT.
func inTx(t *testing.T, p *pgxpool.Pool, fn func(tx pgx.Tx) error) error {
	t.Helper()
	c := ctx(t)
	tx, err := p.Begin(c)
	if err != nil {
		t.Fatal(err)
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback(c)
		return err
	}
	return tx.Commit(c)
}

// ---------- helpers de inserção (SQL explícito, como a aplicação fará) ----------

type wagerRow struct {
	ID, WalletID, PlayerID uuid.UUID
	Origin, Kind, Status   string
	Amount                 int64
	Currency               string
	ProviderID, ExternalID *string
	IdemKey, Hash          *string
	RoundID, GameID        *string
	RefExternalID          *string
	RefTxID                *uuid.UUID
	FailureCode            *string
	BalanceAfter           *int64
	NextAttemptAt          *time.Time
}

func str(s string) *string { return &s }
func i64(v int64) *int64   { return &v }

// externalRow devolve uma operação externa válida em PENDING.
func externalRow(walletID, playerID uuid.UUID, kind, extID string, amount int64) wagerRow {
	return wagerRow{
		ID: uuid.New(), WalletID: walletID, PlayerID: playerID,
		Origin: "EXTERNAL", Kind: kind, Status: "PENDING", Amount: amount, Currency: "BRL",
		ProviderID: str("provider-a"), ExternalID: str(extID),
		IdemKey: str("provider-a:" + extID), Hash: str("hash-" + extID),
		RoundID: str("round-1"), GameID: str("fortune-chimp"),
	}
}

func insertWager(c context.Context, q pgx.Tx, r wagerRow) error {
	_, err := q.Exec(c, `
		INSERT INTO wager_transactions (
			id, origin, kind, status, wallet_id, player_id, amount_minor, currency,
			provider_id, external_transaction_id, idempotency_key, payload_hash,
			round_id, game_id, reference_external_transaction_id, reference_transaction_id,
			failure_code, balance_after_minor, next_attempt_at, created_at, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$20)`,
		r.ID, r.Origin, r.Kind, r.Status, r.WalletID, r.PlayerID, r.Amount, r.Currency,
		r.ProviderID, r.ExternalID, r.IdemKey, r.Hash, r.RoundID, r.GameID,
		r.RefExternalID, r.RefTxID, r.FailureCode, r.BalanceAfter, r.NextAttemptAt, t0)
	return err
}

type ledgerRow struct {
	ID, WalletID, TxID uuid.UUID
	Version            int64
	Direction          string
	Amount             int64
	Currency           string
	Before, After      int64
}

func insertLedger(c context.Context, q pgx.Tx, r ledgerRow) error {
	if r.ID == uuid.Nil {
		r.ID = uuid.New()
	}
	if r.Currency == "" {
		r.Currency = "BRL"
	}
	_, err := q.Exec(c, `
		INSERT INTO wallet_ledger_entries (
			id, wallet_id, transaction_id, wallet_version, direction, amount_minor, currency,
			balance_before_minor, balance_after_minor, created_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`,
		r.ID, r.WalletID, r.TxID, r.Version, r.Direction, r.Amount, r.Currency, r.Before, r.After, t0)
	return err
}

// insertPostings grava as partidas dobradas de uma movimentação de
// carteira, como a aplicação: a conta da carteira repete o lançamento do
// ledger e a contrapartida (caixa ou provedor) fica do lado oposto. As
// contas são criadas na primeira vez.
func insertPostings(c context.Context, q pgx.Tx, txID, walletID uuid.UUID, direction string, amount int64, counterpart string) error {
	opposite := map[string]string{"DEBIT": "CREDIT", "CREDIT": "DEBIT"}[direction]
	walletCode := "wallet:" + walletID.String()
	if _, err := q.Exec(c, `INSERT INTO ledger_accounts (code, type, currency, wallet_id, created_at)
		VALUES ($1, 'LIABILITY', 'BRL', $2, $3) ON CONFLICT DO NOTHING`, walletCode, walletID, t0); err != nil {
		return err
	}
	if err := ensureHouseAccount(c, q, counterpart); err != nil {
		return err
	}
	_, err := q.Exec(c, `INSERT INTO journal_postings (transaction_id, account_code, direction, amount_minor, currency, created_at)
		VALUES ($1, $2, $3, $5, 'BRL', $6), ($1, $4, $7, $5, 'BRL', $6)`,
		txID, walletCode, direction, counterpart, amount, t0, opposite)
	return err
}

// ensureHouseAccount cria cash:BRL ou provider:<p>:BRL.
func ensureHouseAccount(c context.Context, q pgx.Tx, code string) error {
	if code == "cash:BRL" {
		_, err := q.Exec(c, `INSERT INTO ledger_accounts (code, type, currency, created_at)
			VALUES ('cash:BRL', 'ASSET', 'BRL', $1) ON CONFLICT DO NOTHING`, t0)
		return err
	}
	provider := strings.TrimSuffix(strings.TrimPrefix(code, "provider:"), ":BRL")
	_, err := q.Exec(c, `INSERT INTO ledger_accounts (code, type, currency, provider_id, created_at)
		VALUES ($1, 'REVENUE', 'BRL', $2, $3) ON CONFLICT DO NOTHING`, code, provider, t0)
	return err
}

type wallet struct{ ID, PlayerID uuid.UUID }

// openWallet abre uma carteira como a aplicação fará: carteira + OPENING +
// lançamento de crédito na mesma transação (saldo zero: só a carteira).
func openWallet(t *testing.T, p *pgxpool.Pool, balance int64) wallet {
	t.Helper()
	w := wallet{ID: uuid.New(), PlayerID: uuid.New()}
	err := inTx(t, p, func(tx pgx.Tx) error {
		c := ctx(t)
		if _, err := tx.Exec(c, `INSERT INTO wallets (id, player_id, currency, balance_minor, version, created_at, updated_at)
			VALUES ($1, $2, 'BRL', $3, 1, $4, $4)`, w.ID, w.PlayerID, balance, t0); err != nil {
			return err
		}
		if balance == 0 {
			return nil
		}
		opening := wagerRow{
			ID: uuid.New(), WalletID: w.ID, PlayerID: w.PlayerID, Origin: "INTERNAL", Kind: "OPENING",
			Status: "PROCESSED", Amount: balance, Currency: "BRL", BalanceAfter: i64(balance),
		}
		if err := insertWager(c, tx, opening); err != nil {
			return err
		}
		if err := insertLedger(c, tx, ledgerRow{WalletID: w.ID, TxID: opening.ID, Version: 1,
			Direction: "CREDIT", Amount: balance, Before: 0, After: balance}); err != nil {
			return err
		}
		return insertPostings(c, tx, opening.ID, w.ID, "CREDIT", balance, "cash:BRL")
	})
	if err != nil {
		t.Fatalf("openWallet: %v", err)
	}
	return w
}

// debit aplica um débito processado: transação + lançamento + saldo.
func debit(t *testing.T, p *pgxpool.Pool, w wallet, extID string, amount int64) error {
	t.Helper()
	return inTx(t, p, func(tx pgx.Tx) error {
		c := ctx(t)
		var balance, version int64
		if err := tx.QueryRow(c, `SELECT balance_minor, version FROM wallets WHERE id = $1 FOR UPDATE`, w.ID).
			Scan(&balance, &version); err != nil {
			return err
		}
		row := externalRow(w.ID, w.PlayerID, "BET", extID, amount)
		row.Status, row.BalanceAfter = "PROCESSED", i64(balance-amount)
		if err := insertWager(c, tx, row); err != nil {
			return err
		}
		if err := insertLedger(c, tx, ledgerRow{WalletID: w.ID, TxID: row.ID, Version: version + 1,
			Direction: "DEBIT", Amount: amount, Before: balance, After: balance - amount}); err != nil {
			return err
		}
		if err := insertPostings(c, tx, row.ID, w.ID, "DEBIT", amount, "provider:provider-a:BRL"); err != nil {
			return err
		}
		_, err := tx.Exec(c, `UPDATE wallets SET balance_minor = $2, version = version + 1, updated_at = $3 WHERE id = $1`,
			w.ID, balance-amount, t0.Add(time.Second))
		return err
	})
}

// ---------- migrations ----------

// latestMigration conta os arquivos .up.sql: a versão esperada após "up".
func latestMigration(t *testing.T) uint {
	ups, err := fs.Glob(migrations.FS, "*.up.sql")
	if err != nil || len(ups) == 0 {
		t.Fatalf("migrations: %v", err)
	}
	return uint(len(ups))
}

func TestMigrations_UpDownUp(t *testing.T) {
	db := pgtest.New(t)
	c := ctx(t)
	latest := latestMigration(t)

	if v, dirty, err := postgres.MigrationVersion(db.OwnerURL); err != nil || v != latest || dirty {
		t.Fatalf("versão após up = %d dirty=%t err=%v", v, dirty, err)
	}
	db.App.Close() // libera conexões antes do DROP TABLE
	if err := postgres.MigrateDown(db.OwnerURL, 0); err != nil {
		t.Fatalf("down: %v", err)
	}
	var tables int
	if err := db.Owner.QueryRow(c, `SELECT count(*) FROM information_schema.tables
		WHERE table_schema = 'public' AND table_name <> 'schema_migrations'`).Scan(&tables); err != nil {
		t.Fatal(err)
	}
	if tables != 0 {
		t.Errorf("após down restaram %d tabelas", tables)
	}
	if err := postgres.MigrateUp(db.OwnerURL); err != nil {
		t.Fatalf("up de novo: %v", err)
	}
	if v, _, _ := postgres.MigrationVersion(db.OwnerURL); v != latest {
		t.Errorf("versão após reaplicar = %d", v)
	}
}

// ---------- wallets ----------

func TestWallet_OpenAndDebitHappyPath(t *testing.T) {
	db := pgtest.New(t)
	w := openWallet(t, db.App, 100_00)
	if err := debit(t, db.App, w, "bet-1", 80_00); err != nil {
		t.Fatalf("débito válido: %v", err)
	}

	var stored, fromLedger, version int64
	err := db.App.QueryRow(ctx(t), `
		SELECT w.balance_minor, w.version,
		       COALESCE(SUM(CASE l.direction WHEN 'CREDIT' THEN l.amount_minor ELSE -l.amount_minor END), 0)
		  FROM wallets w LEFT JOIN wallet_ledger_entries l ON l.wallet_id = w.id
		 WHERE w.id = $1 GROUP BY w.id`, w.ID).Scan(&stored, &version, &fromLedger)
	if err != nil {
		t.Fatal(err)
	}
	if stored != 20_00 || fromLedger != 20_00 || version != 2 {
		t.Errorf("saldo=%d ledger=%d versão=%d", stored, fromLedger, version)
	}
}

func TestWallet_ZeroOpeningHasNoLedger(t *testing.T) {
	db := pgtest.New(t)
	openWallet(t, db.App, 0) // falharia no COMMIT se exigisse lançamento
}

func TestWallet_BalanceWithoutLedgerFailsAtCommit(t *testing.T) {
	db := pgtest.New(t)
	err := inTx(t, db.App, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx(t), `INSERT INTO wallets (id, player_id, currency, balance_minor, version, created_at, updated_at)
			VALUES ($1, $2, 'BRL', 100, 1, $3, $3)`, uuid.New(), uuid.New(), t0)
		return err // o INSERT passa; o COMMIT tem que falhar
	})
	wantCode(t, err, checkViolation)
}

func TestWallet_UpdateBalanceWithoutLedgerFailsAtCommit(t *testing.T) {
	db := pgtest.New(t)
	w := openWallet(t, db.App, 100)
	err := inTx(t, db.App, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx(t), `UPDATE wallets SET balance_minor = 1000000, version = 2 WHERE id = $1`, w.ID)
		return err
	})
	wantCode(t, err, checkViolation)
}

func TestWallet_Constraints(t *testing.T) {
	db := pgtest.New(t)
	w := openWallet(t, db.App, 100)
	c := ctx(t)

	t.Run("jogador e moeda únicos", func(t *testing.T) {
		_, err := db.App.Exec(c, `INSERT INTO wallets VALUES ($1, $2, 'BRL', 0, 1, $3, $3)`, uuid.New(), w.PlayerID, t0)
		wantCode(t, err, uniqueViolation)
	})
	t.Run("mesmo jogador em outra moeda é permitido", func(t *testing.T) {
		mustExec(t, db.App, `INSERT INTO wallets VALUES ($1, $2, 'USD', 0, 1, $3, $3)`, uuid.New(), w.PlayerID, t0)
	})
	t.Run("saldo negativo", func(t *testing.T) {
		_, err := db.App.Exec(c, `INSERT INTO wallets VALUES ($1, $2, 'BRL', -1, 1, $3, $3)`, uuid.New(), uuid.New(), t0)
		wantCode(t, err, checkViolation)
	})
	t.Run("moeda fora do padrão", func(t *testing.T) {
		_, err := db.App.Exec(c, `INSERT INTO wallets VALUES ($1, $2, 'brl', 0, 1, $3, $3)`, uuid.New(), uuid.New(), t0)
		wantCode(t, err, checkViolation)
	})
	t.Run("saldo muda sem subir versão", func(t *testing.T) {
		_, err := db.App.Exec(c, `UPDATE wallets SET balance_minor = 50 WHERE id = $1`, w.ID)
		wantCode(t, err, checkViolation)
	})
	t.Run("versão pula dois", func(t *testing.T) {
		_, err := db.App.Exec(c, `UPDATE wallets SET balance_minor = 50, version = version + 2 WHERE id = $1`, w.ID)
		wantCode(t, err, checkViolation)
	})
	t.Run("versão sobe sem mudar saldo", func(t *testing.T) {
		_, err := db.App.Exec(c, `UPDATE wallets SET version = version + 1 WHERE id = $1`, w.ID)
		wantCode(t, err, checkViolation)
	})
	t.Run("moeda é imutável", func(t *testing.T) {
		_, err := db.App.Exec(c, `UPDATE wallets SET currency = 'USD' WHERE id = $1`, w.ID)
		wantCode(t, err, checkViolation)
	})
	t.Run("aplicação não apaga carteira", func(t *testing.T) {
		_, err := db.App.Exec(c, `DELETE FROM wallets WHERE id = $1`, w.ID)
		wantCode(t, err, insufficientPriv)
	})
	t.Run("nem o dono apaga carteira", func(t *testing.T) {
		_, err := db.Owner.Exec(c, `DELETE FROM wallets WHERE id = $1`, w.ID)
		wantCode(t, err, insufficientPriv)
	})
}

// ---------- ledger ----------

func TestLedger_AppendOnly(t *testing.T) {
	db := pgtest.New(t)
	openWallet(t, db.App, 100)
	c := ctx(t)

	tests := []struct {
		name string
		pool *pgxpool.Pool
		sql  string
	}{
		{"aplicação: UPDATE", db.App, `UPDATE wallet_ledger_entries SET amount_minor = 1`},
		{"aplicação: DELETE", db.App, `DELETE FROM wallet_ledger_entries`},
		{"aplicação: TRUNCATE", db.App, `TRUNCATE wallet_ledger_entries`},
		{"dono: UPDATE", db.Owner, `UPDATE wallet_ledger_entries SET amount_minor = 1`},
		{"dono: DELETE", db.Owner, `DELETE FROM wallet_ledger_entries`},
		{"dono: TRUNCATE", db.Owner, `TRUNCATE wallet_ledger_entries CASCADE`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := tt.pool.Exec(c, tt.sql)
			wantCode(t, err, insufficientPriv)
		})
	}

	var n int
	if err := db.App.QueryRow(c, `SELECT count(*) FROM wallet_ledger_entries`).Scan(&n); err != nil || n != 1 {
		t.Errorf("ledger deveria continuar com 1 lançamento: %d, %v", n, err)
	}
}

func TestLedger_Integrity(t *testing.T) {
	db := pgtest.New(t)
	w := openWallet(t, db.App, 100_00)
	other := openWallet(t, db.App, 100_00)

	// cria uma BET pendente para servir de transaction_id
	bet := externalRow(w.ID, w.PlayerID, "BET", "bet-1", 10_00)
	if err := inTx(t, db.App, func(tx pgx.Tx) error { return insertWager(ctx(t), tx, bet) }); err != nil {
		t.Fatal(err)
	}
	valid := ledgerRow{WalletID: w.ID, TxID: bet.ID, Version: 2, Direction: "DEBIT", Amount: 10_00, Before: 100_00, After: 90_00}

	tests := []struct {
		name   string
		mutate func(r *ledgerRow)
		code   string
	}{
		{"conta não fecha", func(r *ledgerRow) { r.After = 91_00 }, checkViolation},
		{"direção trocada", func(r *ledgerRow) { r.Direction = "CREDIT" }, checkViolation},
		{"valor zero", func(r *ledgerRow) { r.Amount, r.After = 0, 100_00 }, checkViolation},
		{"saldo posterior negativo", func(r *ledgerRow) { r.Amount, r.After = 200_00, -100_00 }, checkViolation},
		{"quebra o encadeamento", func(r *ledgerRow) { r.Before, r.After = 50_00, 40_00 }, checkViolation},
		{"moeda diferente da carteira", func(r *ledgerRow) { r.Currency = "USD" }, foreignKeyViolation},
		{"transação de outra carteira", func(r *ledgerRow) { r.WalletID = other.ID }, checkViolation},
		// o trigger roda antes da FK, então a transação inexistente é barrada por ele
		{"transação inexistente", func(r *ledgerRow) { r.TxID = uuid.New() }, checkViolation},
		{"versão já usada", func(r *ledgerRow) { r.Version = 1; r.Before, r.After = 0, -10_00 }, checkViolation},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := valid
			tt.mutate(&r)
			err := inTx(t, db.App, func(tx pgx.Tx) error { return insertLedger(ctx(t), tx, r) })
			wantCode(t, err, tt.code)
		})
	}

	t.Run("um lançamento por transação", func(t *testing.T) {
		err := inTx(t, db.App, func(tx pgx.Tx) error {
			c := ctx(t)
			if err := insertLedger(c, tx, valid); err != nil {
				return err
			}
			dup := ledgerRow{WalletID: w.ID, TxID: bet.ID, Version: 3, Direction: "DEBIT", Amount: 10_00, Before: 90_00, After: 80_00}
			return insertLedger(c, tx, dup)
		})
		wantCode(t, err, uniqueViolation)
	})
}

// ---------- wager_transactions ----------

func TestWagerTransactions_Shape(t *testing.T) {
	db := pgtest.New(t)
	w := openWallet(t, db.App, 100_00)

	tests := []struct {
		name   string
		mutate func(r *wagerRow)
		code   string
	}{
		{"OPENING externo", func(r *wagerRow) { r.Kind = "OPENING" }, checkViolation},
		{"INTERNAL com provedor", func(r *wagerRow) { r.Origin = "INTERNAL"; r.Kind = "OPENING" }, checkViolation},
		{"externo sem provedor", func(r *wagerRow) { r.ProviderID = nil }, checkViolation},
		{"externo com rodada vazia", func(r *wagerRow) { r.RoundID = str("") }, checkViolation},
		{"BET zero", func(r *wagerRow) { r.Amount = 0 }, checkViolation},
		{"LOSS diferente de zero", func(r *wagerRow) { r.Kind = "LOSS"; r.Amount = 1 }, checkViolation},
		{"REFUND sem referência", func(r *wagerRow) { r.Kind = "REFUND" }, checkViolation},
		{"BET com referência", func(r *wagerRow) { r.RefExternalID = str("x") }, checkViolation},
		{"PROCESSED sem saldo", func(r *wagerRow) { r.Status = "PROCESSED" }, checkViolation},
		{"REJECTED sem código", func(r *wagerRow) { r.Status = "REJECTED" }, checkViolation},
		{"PENDING_REFERENCE sem agenda", func(r *wagerRow) {
			r.Kind, r.RefExternalID, r.Status = "REFUND", str("bet-0"), "PENDING_REFERENCE"
		}, checkViolation},
		{"status desconhecido", func(r *wagerRow) { r.Status = "DONE" }, checkViolation},
		{"tipo desconhecido", func(r *wagerRow) { r.Kind = "DEPOSIT" }, checkViolation},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := externalRow(w.ID, w.PlayerID, "BET", "tx-"+uuid.NewString(), 10_00)
			tt.mutate(&r)
			err := inTx(t, db.App, func(tx pgx.Tx) error { return insertWager(ctx(t), tx, r) })
			wantCode(t, err, tt.code)
		})
	}
}

func TestWagerTransactions_Idempotency(t *testing.T) {
	db := pgtest.New(t)
	w := openWallet(t, db.App, 100_00)
	insert := func(r wagerRow) error {
		return inTx(t, db.App, func(tx pgx.Tx) error { return insertWager(ctx(t), tx, r) })
	}

	first := externalRow(w.ID, w.PlayerID, "BET", "tx-1", 10_00)
	if err := insert(first); err != nil {
		t.Fatal(err)
	}

	t.Run("mesmo id externo com outra chave", func(t *testing.T) {
		r := externalRow(w.ID, w.PlayerID, "BET", "tx-1", 10_00)
		r.IdemKey = str("outra-chave")
		wantCode(t, insert(r), uniqueViolation)
	})
	t.Run("mesma chave com outro id externo", func(t *testing.T) {
		r := externalRow(w.ID, w.PlayerID, "BET", "tx-2", 10_00)
		r.IdemKey = first.IdemKey
		wantCode(t, insert(r), uniqueViolation)
	})
	t.Run("mesmo id externo em outro provedor é outra operação", func(t *testing.T) {
		r := externalRow(w.ID, w.PlayerID, "BET", "tx-1", 10_00)
		r.ProviderID = str("provider-b")
		if err := insert(r); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("segundo OPENING na mesma carteira", func(t *testing.T) {
		r := wagerRow{ID: uuid.New(), WalletID: w.ID, PlayerID: w.PlayerID, Origin: "INTERNAL", Kind: "OPENING",
			Status: "PROCESSED", Amount: 1, Currency: "BRL", BalanceAfter: i64(1)}
		wantCode(t, insert(r), uniqueViolation)
	})
}

func TestWagerTransactions_StateMachine(t *testing.T) {
	db := pgtest.New(t)
	w := openWallet(t, db.App, 100_00)
	c := ctx(t)

	newRow := func(kind, extID string, ref *string) wagerRow {
		r := externalRow(w.ID, w.PlayerID, kind, extID, 10_00)
		r.RefExternalID = ref
		if err := inTx(t, db.App, func(tx pgx.Tx) error { return insertWager(ctx(t), tx, r) }); err != nil {
			t.Fatal(err)
		}
		return r
	}

	t.Run("PENDING -> PENDING_REFERENCE -> reagendar -> REJECTED", func(t *testing.T) {
		r := newRow("REFUND", "refund-1", str("bet-x"))
		mustExec(t, db.App, `UPDATE wager_transactions SET status='PENDING_REFERENCE', next_attempt_at=$2 WHERE id=$1`, r.ID, t0)
		mustExec(t, db.App, `UPDATE wager_transactions SET attempts=attempts+1, next_attempt_at=$2 WHERE id=$1`, r.ID, t0.Add(time.Second))
		_, err := db.App.Exec(c, `UPDATE wager_transactions SET status='PENDING' WHERE id=$1`, r.ID)
		wantCode(t, err, checkViolation) // não volta para PENDING
		mustExec(t, db.App, `UPDATE wager_transactions SET status='REJECTED', failure_code='REFERENCE_NOT_FOUND', next_attempt_at=NULL WHERE id=$1`, r.ID)

		_, err = db.App.Exec(c, `UPDATE wager_transactions SET attempts=attempts+1 WHERE id=$1`, r.ID)
		wantCode(t, err, checkViolation) // terminal não muda nada
	})
	t.Run("terminal não muda de estado", func(t *testing.T) {
		r := newRow("BET", "bet-1", nil)
		mustExec(t, db.App, `UPDATE wager_transactions SET status='PROCESSED', balance_after_minor=9000 WHERE id=$1`, r.ID)
		_, err := db.App.Exec(c, `UPDATE wager_transactions SET status='REJECTED', failure_code='X', balance_after_minor=NULL WHERE id=$1`, r.ID)
		wantCode(t, err, checkViolation)
	})
	t.Run("campos de negócio são imutáveis", func(t *testing.T) {
		r := newRow("BET", "bet-2", nil)
		_, err := db.App.Exec(c, `UPDATE wager_transactions SET amount_minor = 1 WHERE id=$1`, r.ID)
		wantCode(t, err, checkViolation)
		_, err = db.App.Exec(c, `UPDATE wager_transactions SET payload_hash = 'outro' WHERE id=$1`, r.ID)
		wantCode(t, err, checkViolation)
	})
	t.Run("transações não são apagadas", func(t *testing.T) {
		_, err := db.App.Exec(c, `DELETE FROM wager_transactions`)
		wantCode(t, err, insufficientPriv)
		_, err = db.Owner.Exec(c, `DELETE FROM wager_transactions`)
		wantCode(t, err, insufficientPriv)
	})
}

func TestWagerTransactions_OneReversalPerReference(t *testing.T) {
	db := pgtest.New(t)
	w := openWallet(t, db.App, 100_00)
	bet := externalRow(w.ID, w.PlayerID, "BET", "bet-1", 10_00)
	bet.Status, bet.BalanceAfter = "PROCESSED", i64(90_00)

	reversal := func(kind, extID string) wagerRow {
		r := externalRow(w.ID, w.PlayerID, kind, extID, 10_00)
		r.RefExternalID, r.RefTxID = str("bet-1"), &bet.ID
		r.Status, r.BalanceAfter = "PROCESSED", i64(100_00)
		return r
	}
	insert := func(r wagerRow) error {
		return inTx(t, db.App, func(tx pgx.Tx) error { return insertWager(ctx(t), tx, r) })
	}

	if err := insert(bet); err != nil {
		t.Fatal(err)
	}
	if err := insert(reversal("REFUND", "refund-1")); err != nil {
		t.Fatalf("primeiro REFUND: %v", err)
	}
	wantCode(t, insert(reversal("ROLLBACK", "rollback-1")), uniqueViolation)
	wantCode(t, insert(reversal("REFUND", "refund-2")), uniqueViolation)

	// uma reversão REJEITADA não ocupa o lugar
	rejected := reversal("REFUND", "refund-3")
	rejected.Status, rejected.BalanceAfter, rejected.RefTxID = "REJECTED", nil, &bet.ID
	rejected.FailureCode = str("ALREADY_REVERSED")
	if err := insert(rejected); err != nil {
		t.Fatalf("reversão rejeitada deveria ser gravada: %v", err)
	}
}

// ---------- inbox / outbox ----------

func TestInboxAndOutbox(t *testing.T) {
	db := pgtest.New(t)
	c := ctx(t)

	mustExec(t, db.App, `INSERT INTO inbox_messages VALUES ('wager-consumer', 'msg-1', 'h', $1, NULL)`, t0)
	_, err := db.App.Exec(c, `INSERT INTO inbox_messages VALUES ('wager-consumer', 'msg-1', 'h', $1, NULL)`, t0)
	wantCode(t, err, uniqueViolation)
	mustExec(t, db.App, `INSERT INTO inbox_messages VALUES ('outro-consumer', 'msg-1', 'h', $1, NULL)`, t0)

	eventID := uuid.New()
	mustExec(t, db.App, `INSERT INTO outbox_events (event_id, aggregate_type, aggregate_id, event_type, event_version,
		correlation_id, payload, occurred_at, next_attempt_at)
		VALUES ($1, 'wallet', $2, 'WalletBalanceChanged', 1, 'corr', '{"a":1}', $3, $3)`, eventID, uuid.New(), t0)

	_, err = db.App.Exec(c, `UPDATE outbox_events SET payload = '{"a":2}' WHERE event_id = $1`, eventID)
	wantCode(t, err, checkViolation)

	mustExec(t, db.App, `UPDATE outbox_events SET attempts = 1, locked_until = $2, locked_by = 'w1' WHERE event_id = $1`, eventID, t0)
	mustExec(t, db.App, `UPDATE outbox_events SET published_at = $2 WHERE event_id = $1`, eventID, t0)
	_, err = db.App.Exec(c, `UPDATE outbox_events SET published_at = NULL WHERE event_id = $1`, eventID)
	wantCode(t, err, checkViolation)
}

// ---------- permissões ----------

func TestAppRoleCannotChangeSchema(t *testing.T) {
	db := pgtest.New(t)
	c := ctx(t)
	for _, sql := range []string{
		`CREATE TABLE hack (id int)`,
		`ALTER TABLE wallet_ledger_entries DISABLE TRIGGER ALL`,
		`DROP TABLE wallet_ledger_entries`,
	} {
		_, err := db.App.Exec(c, sql)
		wantCode(t, err, insufficientPriv)
	}
}
