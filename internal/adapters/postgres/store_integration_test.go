//go:build integration

package postgres_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Taino-Edu/Desafio-go-arqui/internal/adapters/postgres"
	"github.com/Taino-Edu/Desafio-go-arqui/internal/app"
	"github.com/Taino-Edu/Desafio-go-arqui/internal/domain/money"
	"github.com/Taino-Edu/Desafio-go-arqui/internal/domain/wagering"
	"github.com/Taino-Edu/Desafio-go-arqui/internal/testsupport/pgtest"
)

func newService(t *testing.T) (*app.WalletService, *postgres.Store, *pgtest.DB) {
	db := pgtest.New(t)
	pool, err := postgres.NewPool(postgres.PoolConfig{URL: db.AppURL, MaxConns: 4, LockTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	store := postgres.NewStore(pool)
	return app.NewWalletService(store, app.SystemClock{}, app.UUIDv7{}), store, db
}

func brl(t *testing.T, s string) money.Money {
	m, err := money.Parse(s, money.BRL)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// bet aplica uma aposta como o caso de uso da fase 5 fará: tudo dentro de
// uma transação, com a carteira travada.
func bet(ctx context.Context, store app.Store, walletID uuid.UUID, extID, amount string, t *testing.T) error {
	return store.WithinTx(ctx, func(ctx context.Context, r app.Repositories) error {
		w, err := r.Wallets().GetForUpdate(ctx, walletID)
		if err != nil {
			return err
		}
		tx, err := wagering.NewExternal(wagering.NewExternalParams{
			ID: uuid.New(), Kind: wagering.KindBet, WalletID: w.ID(), PlayerID: w.PlayerID(),
			Amount: brl(t, amount), Now: time.Now(),
			External: wagering.External{ProviderID: "provider-a", ExternalTransactionID: extID,
				IdempotencyKey: "provider-a:" + extID, PayloadHash: "h", RoundID: "r", GameID: "g"},
		})
		if err != nil {
			return err
		}
		out, err := wagering.Apply(wagering.ApplyInput{Transaction: tx, Wallet: w, LedgerEntryID: uuid.New(), Now: time.Now()})
		if err != nil {
			return err
		}
		if err := r.Transactions().Insert(ctx, tx); err != nil {
			return err
		}
		if out.Entry == nil {
			return nil
		}
		if err := r.Wallets().Update(ctx, w); err != nil {
			return err
		}
		return r.Ledger().Insert(ctx, *out.Entry)
	})
}

func TestStore_LedgerPagination(t *testing.T) {
	svc, store, _ := newService(t)
	ctx := context.Background()
	w, err := svc.OpenWallet(ctx, app.OpenWalletInput{PlayerID: uuid.New(), InitialBalance: brl(t, "100.00")})
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 5; i++ { // 1 abertura + 5 apostas = 6 lançamentos
		if err := bet(ctx, store, w.ID(), fmt.Sprintf("bet-%d", i), "1.00", t); err != nil {
			t.Fatalf("bet %d: %v", i, err)
		}
	}

	var versions []int64
	cursor, pages := "", 0
	for {
		page, err := svc.ListLedger(ctx, w.ID(), cursor, 2)
		if err != nil {
			t.Fatal(err)
		}
		pages++
		for _, e := range page.Entries {
			versions = append(versions, e.WalletVersion())
		}
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}
	if fmt.Sprint(versions) != "[1 2 3 4 5 6]" || pages != 3 {
		t.Errorf("versões = %v em %d páginas", versions, pages)
	}

	got, _ := svc.GetWallet(ctx, w.ID())
	if got.Balance().Amount() != "95.00" || got.Version() != 6 {
		t.Errorf("carteira = %v v%d", got.Balance(), got.Version())
	}
	if _, err := svc.ListLedger(ctx, uuid.New(), "", 10); !errors.Is(err, app.ErrWalletNotFound) {
		t.Errorf("carteira inexistente: %v", err)
	}
}

// Dois escritores leem a mesma versão sem lock. O primeiro confirma; o
// segundo não pode sobrescrever (lost update): o UPDATE condicionado à versão
// não afeta nenhuma linha e a transação inteira é desfeita.
func TestStore_UpdateRejectsStaleVersion(t *testing.T) {
	svc, store, db := newService(t)
	ctx := context.Background()
	w, err := svc.OpenWallet(ctx, app.OpenWalletInput{PlayerID: uuid.New(), InitialBalance: brl(t, "100.00")})
	if err != nil {
		t.Fatal(err)
	}

	stale1, _ := store.Reader().Wallets().Get(ctx, w.ID())
	stale2, _ := store.Reader().Wallets().Get(ctx, w.ID())
	if _, err := stale1.Debit(uuid.New(), uuid.New(), brl(t, "30.00"), time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := stale2.Debit(uuid.New(), uuid.New(), brl(t, "50.00"), time.Now()); err != nil {
		t.Fatal(err)
	}

	first := store.WithinTx(ctx, func(ctx context.Context, r app.Repositories) error {
		return r.Wallets().Update(ctx, stale1)
	})
	// sem lançamento, o COMMIT do primeiro também falha (saldo = ledger);
	// isso mostra a checagem adiada funcionando junto com a de versão
	if first == nil {
		t.Fatal("update sem lançamento não pode confirmar")
	}

	// agora um primeiro escritor completo e correto
	if err := bet(ctx, store, w.ID(), "bet-1", "30.00", t); err != nil {
		t.Fatal(err)
	}
	second := store.WithinTx(ctx, func(ctx context.Context, r app.Repositories) error {
		return r.Wallets().Update(ctx, stale2) // ainda acha que a versão é 1
	})
	if !errors.Is(second, app.ErrTransient) {
		t.Fatalf("escritor atrasado: err = %v, want ErrTransient", second)
	}

	var balance int64
	if err := db.Owner.QueryRow(ctx, `SELECT balance_minor FROM wallets WHERE id = $1`, w.ID()).Scan(&balance); err != nil {
		t.Fatal(err)
	}
	if balance != 70_00 {
		t.Errorf("saldo = %d, want 7000 (o débito confirmado não pode ser perdido)", balance)
	}
}

// Um lock_timeout estourado vira ErrTransient (HTTP 503), não erro interno.
func TestStore_LockTimeoutIsTransient(t *testing.T) {
	svc, store, db := newService(t)
	ctx := context.Background()
	w, err := svc.OpenWallet(ctx, app.OpenWalletInput{PlayerID: uuid.New(), InitialBalance: brl(t, "10.00")})
	if err != nil {
		t.Fatal(err)
	}

	// outra conexão segura o lock da carteira
	holder, err := pgxpool.New(ctx, db.AppURL)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Close()
	tx, err := holder.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SELECT 1 FROM wallets WHERE id = $1 FOR UPDATE`, w.ID()); err != nil {
		t.Fatal(err)
	}

	err = store.WithinTx(ctx, func(ctx context.Context, r app.Repositories) error {
		_, err := r.Wallets().GetForUpdate(ctx, w.ID()) // espera até lock_timeout (1s)
		return err
	})
	if !errors.Is(err, app.ErrTransient) {
		t.Fatalf("err = %v, want ErrTransient", err)
	}
}
