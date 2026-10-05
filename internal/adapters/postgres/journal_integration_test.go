//go:build integration

package postgres_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Taino-Edu/Desafio-go-arqui/internal/adapters/postgres"
	"github.com/Taino-Edu/Desafio-go-arqui/internal/testsupport/pgtest"
)

// pendingBet grava uma BET em PENDING e devolve o id: base para gravar, à
// mão, lançamentos certos e errados.
func pendingBet(t *testing.T, db *pgtest.DB, w wallet, extID string) uuid.UUID {
	t.Helper()
	row := externalRow(w.ID, w.PlayerID, "BET", extID, 10_00)
	if err := inTx(t, db.App, func(tx pgx.Tx) error { return insertWager(ctx(t), tx, row) }); err != nil {
		t.Fatal(err)
	}
	return row.ID
}

// O banco recusa todo lançamento contábil errado, venha de onde vier.
func TestJournal_Invariants(t *testing.T) {
	db := pgtest.New(t)
	w := openWallet(t, db.App, 100_00)
	walletCode := "wallet:" + w.ID.String()
	// a conta do provedor já existe: cada caso esbarra na regra que testa,
	// não na FK
	if err := inTx(t, db.App, func(tx pgx.Tx) error {
		return ensureHouseAccount(ctx(t), tx, "provider:provider-a:BRL")
	}); err != nil {
		t.Fatal(err)
	}
	debitLedger := func(c context.Context, tx pgx.Tx, txID uuid.UUID) error {
		return insertLedger(c, tx, ledgerRow{WalletID: w.ID, TxID: txID, Version: 2,
			Direction: "DEBIT", Amount: 10_00, Before: 100_00, After: 90_00})
	}
	posting := func(c context.Context, tx pgx.Tx, txID uuid.UUID, code, dir string, amount int64) error {
		_, err := tx.Exec(c, `INSERT INTO journal_postings VALUES ($1, $2, $3, $4, 'BRL', $5)`, txID, code, dir, amount, t0)
		return err
	}

	tests := []struct {
		name  string
		write func(c context.Context, tx pgx.Tx, txID uuid.UUID) error
		code  string
	}{
		{"débitos ≠ créditos", func(c context.Context, tx pgx.Tx, id uuid.UUID) error {
			if err := debitLedger(c, tx, id); err != nil {
				return err
			}
			if err := posting(c, tx, id, walletCode, "DEBIT", 10_00); err != nil {
				return err
			}
			return posting(c, tx, id, "provider:provider-a:BRL", "CREDIT", 9_99)
		}, checkViolation},
		{"partida sozinha", func(c context.Context, tx pgx.Tx, id uuid.UUID) error {
			if err := debitLedger(c, tx, id); err != nil {
				return err
			}
			return posting(c, tx, id, walletCode, "DEBIT", 10_00)
		}, checkViolation},
		{"lançamento do ledger sem partidas", func(c context.Context, tx pgx.Tx, id uuid.UUID) error {
			return debitLedger(c, tx, id) // a aplicação antiga, depois da 000006
		}, checkViolation},
		{"partida da carteira sem lançamento no ledger", func(c context.Context, tx pgx.Tx, id uuid.UUID) error {
			if err := posting(c, tx, id, walletCode, "DEBIT", 10_00); err != nil {
				return err
			}
			return posting(c, tx, id, "provider:provider-a:BRL", "CREDIT", 10_00)
		}, checkViolation},
		{"partida da carteira diverge do ledger", func(c context.Context, tx pgx.Tx, id uuid.UUID) error {
			if err := debitLedger(c, tx, id); err != nil {
				return err
			}
			if err := posting(c, tx, id, walletCode, "CREDIT", 10_00); err != nil {
				return err
			}
			return posting(c, tx, id, "provider:provider-a:BRL", "DEBIT", 10_00)
		}, checkViolation},
		{"contrapartida em outro provedor", func(c context.Context, tx pgx.Tx, id uuid.UUID) error {
			if err := debitLedger(c, tx, id); err != nil {
				return err
			}
			if err := ensureHouseAccount(c, tx, "provider:provider-b:BRL"); err != nil {
				return err
			}
			if err := posting(c, tx, id, walletCode, "DEBIT", 10_00); err != nil {
				return err
			}
			return posting(c, tx, id, "provider:provider-b:BRL", "CREDIT", 10_00)
		}, checkViolation},
		{"caixa como contrapartida de aposta", func(c context.Context, tx pgx.Tx, id uuid.UUID) error {
			if err := debitLedger(c, tx, id); err != nil {
				return err
			}
			if err := posting(c, tx, id, walletCode, "DEBIT", 10_00); err != nil {
				return err
			}
			return posting(c, tx, id, "cash:BRL", "CREDIT", 10_00)
		}, checkViolation},
		{"conta inexistente", func(c context.Context, tx pgx.Tx, id uuid.UUID) error {
			if err := debitLedger(c, tx, id); err != nil {
				return err
			}
			if err := posting(c, tx, id, walletCode, "DEBIT", 10_00); err != nil {
				return err
			}
			return posting(c, tx, id, "provider:fantasma:BRL", "CREDIT", 10_00)
		}, foreignKeyViolation},
		{"moeda da partida ≠ moeda da conta", func(c context.Context, tx pgx.Tx, id uuid.UUID) error {
			_, err := tx.Exec(c, `INSERT INTO journal_postings VALUES ($1, $2, 'DEBIT', 1000, 'USD', $3)`, id, walletCode, t0)
			return err
		}, foreignKeyViolation},
	}
	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			id := pendingBet(t, db, w, "bet-"+string(rune('a'+i)))
			err := inTx(t, db.App, func(tx pgx.Tx) error { return tt.write(ctx(t), tx, id) })
			wantCode(t, err, tt.code)
		})
	}

	// o caminho certo continua passando
	if err := debit(t, db.App, w, "bet-ok", 10_00); err != nil {
		t.Fatalf("débito correto: %v", err)
	}
}

func TestJournal_AccountsAreConsistentAndImmutable(t *testing.T) {
	db := pgtest.New(t)
	openWallet(t, db.App, 100_00)
	c := ctx(t)

	shapes := map[string]string{
		"código não bate com a carteira": `INSERT INTO ledger_accounts (code, type, currency, wallet_id, created_at)
			SELECT 'wallet:outra', 'LIABILITY', 'BRL', id, now() FROM wallets LIMIT 1`,
		"caixa com nome de provedor": `INSERT INTO ledger_accounts (code, type, currency, created_at)
			VALUES ('provider:x:BRL', 'ASSET', 'BRL', now())`,
		"provedor sem provider_id": `INSERT INTO ledger_accounts (code, type, currency, created_at)
			VALUES ('provider::BRL', 'REVENUE', 'BRL', now())`,
		"tipo desconhecido": `INSERT INTO ledger_accounts (code, type, currency, created_at)
			VALUES ('equity:BRL', 'EQUITY', 'BRL', now())`,
	}
	for name, sql := range shapes {
		t.Run(name, func(t *testing.T) {
			_, err := db.App.Exec(c, sql)
			wantCode(t, err, checkViolation)
		})
	}

	for _, sql := range []string{
		`UPDATE journal_postings SET amount_minor = 1`,
		`DELETE FROM journal_postings`,
		`TRUNCATE journal_postings`,
		`UPDATE ledger_accounts SET type = 'ASSET'`,
		`DELETE FROM ledger_accounts`,
	} {
		t.Run("aplicação: "+sql, func(t *testing.T) {
			_, err := db.App.Exec(c, sql)
			wantCode(t, err, insufficientPriv)
		})
		t.Run("dono: "+sql, func(t *testing.T) {
			_, err := db.Owner.Exec(c, sql+map[bool]string{true: " CASCADE", false: ""}[sql == `TRUNCATE journal_postings`])
			wantCode(t, err, insufficientPriv)
		})
	}
}

// Dados gravados pela versão antiga (só o ledger) ganham partidas na 000006,
// e o razão resultante fecha: implantação sobre um banco com histórico.
func TestMigrations_DoubleEntryBackfill(t *testing.T) {
	db := pgtest.New(t)
	c := ctx(t)
	if err := postgres.MigrateTo(db.OwnerURL, 4); err != nil {
		t.Fatal(err)
	}

	// versão antiga: carteira + abertura + aposta + prêmio, sem razão
	walletID, playerID := uuid.New(), uuid.New()
	opening, bet, win := uuid.New(), externalRow(walletID, playerID, "BET", "bet-1", 30_00), externalRow(walletID, playerID, "WIN", "win-1", 50_00)
	bet.Status, bet.BalanceAfter = "PROCESSED", i64(70_00)
	win.Status, win.BalanceAfter = "PROCESSED", i64(120_00)
	win.ProviderID, win.IdemKey = str("provider-b"), str("provider-b:win-1")
	err := inTx(t, db.App, func(tx pgx.Tx) error {
		if _, err := tx.Exec(c, `INSERT INTO wallets VALUES ($1, $2, 'BRL', 120_00, 3, $3, $3)`, walletID, playerID, t0); err != nil {
			return err
		}
		if err := insertWager(c, tx, wagerRow{ID: opening, WalletID: walletID, PlayerID: playerID, Origin: "INTERNAL",
			Kind: "OPENING", Status: "PROCESSED", Amount: 100_00, Currency: "BRL", BalanceAfter: i64(100_00)}); err != nil {
			return err
		}
		for _, r := range []wagerRow{bet, win} {
			if err := insertWager(c, tx, r); err != nil {
				return err
			}
		}
		for _, l := range []ledgerRow{
			{WalletID: walletID, TxID: opening, Version: 1, Direction: "CREDIT", Amount: 100_00, Before: 0, After: 100_00},
			{WalletID: walletID, TxID: bet.ID, Version: 2, Direction: "DEBIT", Amount: 30_00, Before: 100_00, After: 70_00},
			{WalletID: walletID, TxID: win.ID, Version: 3, Direction: "CREDIT", Amount: 50_00, Before: 70_00, After: 120_00},
		} {
			if err := insertLedger(c, tx, l); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("dados da versão antiga: %v", err)
	}

	if err := postgres.MigrateUp(db.OwnerURL); err != nil {
		t.Fatalf("000005 + 000006 sobre o histórico: %v", err)
	}

	type bal struct {
		code    string
		balance int64 // créditos − débitos
	}
	rows, err := db.App.Query(c, `
		SELECT account_code, SUM(CASE direction WHEN 'CREDIT' THEN amount_minor ELSE -amount_minor END)::BIGINT
		  FROM journal_postings GROUP BY 1 ORDER BY 1`)
	if err != nil {
		t.Fatal(err)
	}
	got, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (bal, error) {
		var b bal
		return b, r.Scan(&b.code, &b.balance)
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []bal{
		{"cash:BRL", -100_00},                   // ativo: 100,00 a débito
		{"provider:provider-a:BRL", 30_00},      // GGR: recebeu a aposta
		{"provider:provider-b:BRL", -50_00},     // GGR: pagou o prêmio
		{"wallet:" + walletID.String(), 120_00}, // passivo = saldo da carteira
	}
	if len(got) != len(want) {
		t.Fatalf("contas = %+v", got)
	}
	var sum int64
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("[%d] = %+v, want %+v", i, got[i], want[i])
		}
		sum += got[i].balance
	}
	if sum != 0 {
		t.Errorf("Σ créditos − Σ débitos = %d, want 0", sum)
	}

	// e daqui em diante a regra vale: lançamento sem partida não confirma
	err = inTx(t, db.App, func(tx pgx.Tx) error {
		id := uuid.New()
		r := externalRow(walletID, playerID, "BET", "bet-2", 10_00)
		r.ID = id
		if err := insertWager(c, tx, r); err != nil {
			return err
		}
		return insertLedger(c, tx, ledgerRow{WalletID: walletID, TxID: id, Version: 4,
			Direction: "DEBIT", Amount: 10_00, Before: 120_00, After: 110_00})
	})
	wantCode(t, err, checkViolation)
}

// Um lançamento já confirmado não pode ganhar partidas depois: a partida
// acrescentada noutra transação dispara a conferência do lançamento inteiro.
func TestJournal_CannotAppendToCommittedEntry(t *testing.T) {
	db := pgtest.New(t)
	w := openWallet(t, db.App, 100_00)
	if err := debit(t, db.App, w, "bet-1", 10_00); err != nil {
		t.Fatal(err)
	}
	var txID uuid.UUID
	if err := db.App.QueryRow(ctx(t), `SELECT id FROM wager_transactions WHERE external_transaction_id = 'bet-1'`).Scan(&txID); err != nil {
		t.Fatal(err)
	}
	err := inTx(t, db.App, func(tx pgx.Tx) error {
		c := ctx(t)
		// "zzz" ordena depois das partidas existentes
		if err := ensureHouseAccount(c, tx, "provider:zzz:BRL"); err != nil {
			return err
		}
		_, err := tx.Exec(c, `INSERT INTO journal_postings VALUES ($1, 'provider:zzz:BRL', 'CREDIT', 500, 'BRL', $2)`, txID, t0)
		return err
	})
	wantCode(t, err, checkViolation)
}
