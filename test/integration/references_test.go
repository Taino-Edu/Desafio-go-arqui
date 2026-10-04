//go:build integration

package integration

import (
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Taino-Edu/Desafio-go-arqui/internal/platform/config"
	"github.com/Taino-Edu/Desafio-go-arqui/internal/testsupport/apptest"
	"github.com/Taino-Edu/Desafio-go-arqui/internal/testsupport/pgtest"
)

func workerOff(c *config.Config) { c.References.WorkerEnabled = false }

func refundOp(player, wallet, extID, ref, amount string) apptest.Op {
	return apptest.Op{Provider: "provider-a", ExternalID: extID, PlayerID: player, WalletID: wallet,
		Round: "round-1", Game: "g", Kind: "REFUND", Amount: amount, RefID: ref}
}

func betOp(player, wallet, extID, amount string) apptest.Op {
	return apptest.Op{Provider: "provider-a", ExternalID: extID, PlayerID: player, WalletID: wallet,
		Round: "round-1", Game: "g", Kind: "BET", Amount: amount}
}

// Teste obrigatório 7: REFUND chega antes da aposta; quando a aposta chega,
// o worker conclui o REFUND.
func TestPendingReference_ResolvedWhenReferenceArrives(t *testing.T) {
	f := newFixture(t, "100.00")

	r := apptest.Must(t, f.c.Submit(refundOp(f.playerID, f.walletID, "refund-1", "bet-1", "30.00")), http.StatusAccepted, "refund antes")
	if r.Str("status") != "PENDING_REFERENCE" {
		t.Fatalf("refund = %v", r.Body)
	}
	apptest.Must(t, f.c.Submit(betOp(f.playerID, f.walletID, "bet-1", "30.00")), http.StatusCreated, "bet")

	start := time.Now()
	done := f.c.WaitStatus("provider-a", "refund-1", "PROCESSED", 5*time.Second)
	t.Logf("pendência resolvida %v depois da chegada da aposta", time.Since(start))

	if done.Str("referenceTransactionId") == "" || done.Amount("balanceAfter") != "100.00" {
		t.Errorf("refund concluído = %v", done.Body)
	}
	if f.balance() != "100.00" {
		t.Errorf("saldo = %s (aposta debitada e devolvida)", f.balance())
	}
	// o replay do REFUND agora devolve o resultado final
	replay := apptest.Must(t, f.c.Submit(refundOp(f.playerID, f.walletID, "refund-1", "bet-1", "30.00")), http.StatusOK, "replay")
	if replay.Body["idempotentReplay"] != true || replay.Str("status") != "PROCESSED" {
		t.Errorf("replay = %v", replay.Body)
	}
	apptest.AssertReconciled(t, f.app.DB)
}

// Teste obrigatório 7 (expiração): a referência nunca chega.
func TestPendingReference_ExpiresAsReferenceNotFound(t *testing.T) {
	db := pgtest.New(t)
	a, _ := apptest.StartOn(t, db, func(c *config.Config) {
		c.References.BaseDelay, c.References.MaxDelay, c.References.MaxAttempts = 30*time.Millisecond, 100*time.Millisecond, 3
	})
	player := uuid.NewString()
	wallet := a.Client.OpenWallet(player, "100.00")

	apptest.Must(t, a.Client.Submit(refundOp(player, wallet, "refund-orphan", "bet-never", "10.00")), http.StatusAccepted, "refund órfão")
	r := a.Client.WaitStatus("provider-a", "refund-orphan", "REJECTED", 5*time.Second)
	if r.Str("failureCode") != "REFERENCE_NOT_FOUND" || r.Body["attempts"].(float64) != 3 || r.Body["failureCorrectable"] != false {
		t.Errorf("expirado = %v", r.Body)
	}
	events := apptest.Count(t, db, `SELECT count(*) FROM outbox_events
		WHERE event_type = 'WagerTransactionRejected' AND aggregate_id = $1`, r.Str("transactionId"))
	if events != 1 {
		t.Errorf("eventos de rejeição = %d", events)
	}
}

// A referência chega, mas termina rejeitada: a pendência é rejeitada com
// REFERENCE_NOT_PROCESSED (não fica esperando até expirar).
func TestPendingReference_ReferenceRejected(t *testing.T) {
	f := newFixture(t, "100.00")
	apptest.Must(t, f.c.Submit(refundOp(f.playerID, f.walletID, "refund-1", "bet-big", "500.00")), http.StatusAccepted, "refund")
	apptest.Must(t, f.c.Submit(betOp(f.playerID, f.walletID, "bet-big", "500.00")), http.StatusUnprocessableEntity, "aposta sem saldo")

	r := f.c.WaitStatus("provider-a", "refund-1", "REJECTED", 5*time.Second)
	if r.Str("failureCode") != "REFERENCE_NOT_PROCESSED" || f.balance() != "100.00" {
		t.Errorf("refund = %v, saldo %s", r.Body, f.balance())
	}
}

// Teste obrigatório 8 (parte): a pendência sobrevive ao reinício. Uma
// instância sem worker registra a pendência e cai; outra instância, nova,
// retoma do banco.
func TestPendingReference_ResumedAfterRestart(t *testing.T) {
	db := pgtest.New(t)
	first, stopFirst := apptest.StartOn(t, db, workerOff)
	player := uuid.NewString()
	wallet := first.Client.OpenWallet(player, "100.00")

	apptest.Must(t, first.Client.Submit(refundOp(player, wallet, "refund-1", "bet-1", "40.00")), http.StatusAccepted, "refund")
	apptest.Must(t, first.Client.Submit(betOp(player, wallet, "bet-1", "40.00")), http.StatusCreated, "bet")
	time.Sleep(200 * time.Millisecond)
	if got := first.Client.Do("GET", "/providers/provider-a/wagering/transactions/refund-1", nil); got.Str("status") != "PENDING_REFERENCE" {
		t.Fatalf("sem worker a pendência deveria continuar: %v", got.Body)
	}
	stopFirst()

	second, _ := apptest.StartOn(t, db, nil) // outra instância, memória zerada
	r := second.Client.WaitStatus("provider-a", "refund-1", "PROCESSED", 5*time.Second)
	if r.Amount("balanceAfter") != "100.00" {
		t.Errorf("refund = %v", r.Body)
	}
	apptest.AssertReconciled(t, db)
}

// Várias instâncias com worker disputando as mesmas pendências: cada uma é
// concluída exatamente uma vez (FOR UPDATE SKIP LOCKED).
func TestPendingReference_CompetingWorkers(t *testing.T) {
	db := pgtest.New(t)
	setup, stopSetup := apptest.StartOn(t, db, workerOff)

	const n = 30
	type item struct{ player, wallet string }
	items := make([]item, n)
	for i := range items {
		p := uuid.NewString()
		items[i] = item{p, setup.Client.OpenWallet(p, "50.00")}
	}
	for i, it := range items {
		apptest.Must(t, setup.Client.Submit(refundOp(it.player, it.wallet, fmt.Sprintf("refund-%d", i), fmt.Sprintf("bet-%d", i), "20.00")), 202, "refund")
		apptest.Must(t, setup.Client.Submit(betOp(it.player, it.wallet, fmt.Sprintf("bet-%d", i), "20.00")), 201, "bet")
	}
	stopSetup()

	// 3 instâncias com worker ligado, ao mesmo tempo
	var wg sync.WaitGroup
	workers := make([]*apptest.App, 3)
	for i := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			workers[i], _ = apptest.StartOn(t, db, nil)
		}()
	}
	wg.Wait()

	for i := range items {
		workers[i%3].Client.WaitStatus("provider-a", fmt.Sprintf("refund-%d", i), "PROCESSED", 10*time.Second)
	}
	credits := apptest.Count(t, db, `SELECT count(*) FROM wallet_ledger_entries l
		JOIN wager_transactions t ON t.id = l.transaction_id WHERE t.kind = 'REFUND'`)
	events := apptest.Count(t, db, `SELECT count(*) FROM outbox_events o
		JOIN wager_transactions t ON t.id = o.aggregate_id
		WHERE t.kind = 'REFUND' AND o.event_type = 'WagerTransactionProcessed'`)
	if credits != n || events != n {
		t.Errorf("créditos de REFUND = %d, eventos = %d, want %d (cada pendência uma única vez)", credits, events, n)
	}
	for i, it := range items {
		if got := workers[0].Client.Do("GET", "/wallets/"+it.wallet, nil); got.Amount("balance") != "50.00" {
			t.Errorf("carteira %d: %v", i, got.Body)
		}
	}
	apptest.AssertReconciled(t, db)
}
