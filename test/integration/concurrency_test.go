//go:build integration

package integration

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Taino-Edu/Desafio-go-arqui/internal/testsupport/apptest"
)

// parallel dispara fn(i) em n goroutines que começam juntas (barreira), para
// maximizar a sobreposição real das requisições.
func parallel(n int, fn func(i int)) {
	var ready, done sync.WaitGroup
	start := make(chan struct{})
	ready.Add(n)
	done.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer done.Done()
			ready.Done()
			<-start
			fn(i)
		}()
	}
	ready.Wait()
	close(start)
	done.Wait()
}

// Teste obrigatório 1: a mesma aposta 50 vezes em paralelo gera UM débito.
// Os recebimentos repetidos são reais (50 requisições HTTP); quem deduplica é
// a aplicação (INSERT ... ON CONFLICT sobre o índice único), não o cliente.
func TestSameBet50TimesInParallel(t *testing.T) {
	f := newFixture(t, "100.00")
	bet := f.op("BET", "bet-dup", "10.00")

	results := make([]apptest.Response, 50)
	parallel(50, func(i int) { results[i] = f.c.Submit(bet) })

	created, replays := 0, 0
	ids := map[string]bool{}
	for _, r := range results {
		switch {
		case r.Status == http.StatusCreated && r.Body["idempotentReplay"] == false:
			created++
		case r.Status == http.StatusOK && r.Body["idempotentReplay"] == true:
			replays++
		default:
			t.Errorf("resposta inesperada: %d %v", r.Status, r.Body)
		}
		ids[r.Str("transactionId")] = true
		if r.Amount("balance") != "90.00" {
			t.Errorf("saldo devolvido = %s", r.Amount("balance"))
		}
	}
	if created != 1 || replays != 49 || len(ids) != 1 {
		t.Errorf("criadas %d, replays %d, ids distintos %d", created, replays, len(ids))
	}
	debits := apptest.Count(t, f.app.DB, `SELECT count(*) FROM wallet_ledger_entries WHERE wallet_id = $1 AND direction = 'DEBIT'`, f.walletID)
	if debits != 1 || f.balance() != "90.00" {
		t.Errorf("débitos %d, saldo %s", debits, f.balance())
	}
	apptest.AssertReconciled(t, f.app.DB)
}

// Teste obrigatório 2: carteira com 100.00 recebe ao mesmo tempo duas
// apostas distintas de 80.00. Repetido em 15 carteiras simultaneamente.
func TestTwoBetsOf80On100Concurrently(t *testing.T) {
	a := apptest.Start(t)
	const wallets = 15

	type pair struct{ player, wallet string }
	ws := make([]pair, wallets)
	for i := range ws {
		p := uuid.NewString()
		ws[i] = pair{p, a.Client.OpenWallet(p, "100.00")}
	}
	op := func(w pair, ext string) apptest.Op {
		return apptest.Op{Provider: "provider-a", ExternalID: ext, PlayerID: w.player, WalletID: w.wallet,
			Round: "r", Game: "g", Kind: "BET", Amount: "80.00"}
	}

	results := make([]apptest.Response, wallets*2)
	parallel(wallets*2, func(i int) {
		w := ws[i/2]
		results[i] = a.Client.Submit(op(w, fmt.Sprintf("%s-bet-%d", w.wallet, i%2)))
	})

	check := func(label string, results []apptest.Response) {
		for i := 0; i < wallets; i++ {
			r1, r2 := results[2*i], results[2*i+1]
			processed, rejected := 0, 0
			for _, r := range []apptest.Response{r1, r2} {
				switch r.Str("status") {
				case "PROCESSED":
					processed++
				case "REJECTED":
					if r.Str("failureCode") == "INSUFFICIENT_FUNDS" {
						rejected++
					}
				}
			}
			if processed != 1 || rejected != 1 {
				t.Errorf("%s carteira %d: %v / %v", label, i, r1.Body, r2.Body)
			}
		}
	}
	check("primeira rodada", results)

	// reenvios não podem alterar o resultado
	replays := make([]apptest.Response, wallets*2)
	parallel(wallets*2, func(i int) {
		w := ws[i/2]
		replays[i] = a.Client.Submit(op(w, fmt.Sprintf("%s-bet-%d", w.wallet, i%2)))
	})
	check("reenvio", replays)

	for i, w := range ws {
		got := a.Client.Do("GET", "/wallets/"+w.wallet, nil)
		debits := apptest.Count(t, a.DB, `SELECT count(*) FROM wallet_ledger_entries WHERE wallet_id = $1 AND direction = 'DEBIT'`, w.wallet)
		if got.Amount("balance") != "20.00" || debits != 1 {
			t.Errorf("carteira %d: saldo %s, débitos %d", i, got.Amount("balance"), debits)
		}
		if replays[2*i].Str("transactionId") != results[2*i].Str("transactionId") {
			t.Errorf("carteira %d: replay devolveu outra transação", i)
		}
	}
	apptest.AssertReconciled(t, a.DB)
}

// Carteiras diferentes avançam em paralelo: com a carteira A travada por
// outra conexão, uma aposta na carteira B conclui na hora. Não há lock global.
func TestDifferentWalletsAreNotBlocked(t *testing.T) {
	a := apptest.Start(t)
	pa, pb := uuid.NewString(), uuid.NewString()
	wa, wb := a.Client.OpenWallet(pa, "100.00"), a.Client.OpenWallet(pb, "100.00")

	holder, err := pgxpool.New(context.Background(), a.DB.AppURL)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Close()
	tx, err := holder.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(context.Background(), `SELECT 1 FROM wallets WHERE id = $1 FOR UPDATE`, wa); err != nil {
		t.Fatal(err)
	}

	blocked := make(chan apptest.Response, 1)
	go func() {
		blocked <- a.Client.Submit(apptest.Op{Provider: "provider-a", ExternalID: "a-1", PlayerID: pa, WalletID: wa,
			Round: "r", Game: "g", Kind: "BET", Amount: "1.00"})
	}()

	start := time.Now()
	r := a.Client.Submit(apptest.Op{Provider: "provider-a", ExternalID: "b-1", PlayerID: pb, WalletID: wb,
		Round: "r", Game: "g", Kind: "BET", Amount: "1.00"})
	elapsed := time.Since(start)
	if r.Status != http.StatusCreated || elapsed > time.Second {
		t.Errorf("carteira B: %d em %v (não deveria esperar a carteira A)", r.Status, elapsed)
	}

	select {
	case r := <-blocked:
		t.Fatalf("carteira A deveria estar esperando o lock, respondeu %d", r.Status)
	case <-time.After(300 * time.Millisecond):
	}
	_ = tx.Rollback(context.Background()) // libera A
	if r := <-blocked; r.Status != http.StatusCreated {
		t.Errorf("carteira A depois de liberada: %d %v", r.Status, r.Body)
	}
}

// Muitas carteiras e muitas apostas ao mesmo tempo, sem perda de atualização.
func TestManyWalletsManyBetsInParallel(t *testing.T) {
	a := apptest.Start(t)
	const wallets, betsPerWallet = 20, 10

	type pair struct{ player, wallet string }
	ws := make([]pair, wallets)
	for i := range ws {
		p := uuid.NewString()
		ws[i] = pair{p, a.Client.OpenWallet(p, "100.00")}
	}
	parallel(wallets*betsPerWallet, func(i int) {
		w := ws[i%wallets]
		r := a.Client.Submit(apptest.Op{Provider: "provider-a", ExternalID: fmt.Sprintf("%s-%d", w.wallet, i),
			PlayerID: w.player, WalletID: w.wallet, Round: "r", Game: "g", Kind: "BET", Amount: "1.00"})
		if r.Status != http.StatusCreated {
			t.Errorf("aposta %d: %d %v", i, r.Status, r.Body)
		}
	})
	for i, w := range ws {
		got := a.Client.Do("GET", "/wallets/"+w.wallet, nil)
		if got.Amount("balance") != "90.00" || got.Body["version"].(float64) != 1+betsPerWallet {
			t.Errorf("carteira %d: %v", i, got.Body)
		}
	}
	apptest.AssertReconciled(t, a.DB)
}
