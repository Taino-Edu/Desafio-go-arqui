//go:build integration

// Testes de ponta a ponta da API de operações: HTTP real, Fx, Postgres real.
//
//	docker compose up -d postgres
//	go test -tags=integration -race ./test/integration/...
package integration

import (
	"net/http"
	"testing"

	"github.com/google/uuid"

	"github.com/Taino-Edu/Desafio-go-arqui/internal/testsupport/apptest"
)

// fixture: uma aplicação e uma carteira prontas.
type fixture struct {
	t        *testing.T
	app      *apptest.App
	c        *apptest.Client
	playerID string
	walletID string
}

func newFixture(t *testing.T, balance string) *fixture {
	a := apptest.Start(t)
	player := uuid.NewString()
	return &fixture{t: t, app: a, c: a.Client, playerID: player, walletID: a.Client.OpenWallet(player, balance)}
}

func (f *fixture) op(kind, extID, amount string) apptest.Op {
	return apptest.Op{Provider: "provider-a", ExternalID: extID, PlayerID: f.playerID, WalletID: f.walletID,
		Round: "round-1", Game: "fortune-chimp", Kind: kind, Amount: amount}
}

func (f *fixture) balance() string {
	r := apptest.Must(f.t, f.c.Do("GET", "/wallets/"+f.walletID, nil), 200, "get wallet")
	return r.Amount("balance")
}

func (f *fixture) ledgerCount() int {
	return apptest.Count(f.t, f.app.DB, `SELECT count(*) FROM wallet_ledger_entries WHERE wallet_id = $1`, f.walletID)
}

// ------------------------------------------------------------------

func TestBet_ProcessedThenReplayKeepsOriginalBalance(t *testing.T) {
	f := newFixture(t, "1000.00")

	first := apptest.Must(t, f.c.Submit(f.op("BET", "bet-1", "25.00")), http.StatusCreated, "bet")
	if first.Str("status") != "PROCESSED" || first.Amount("balance") != "975.00" || first.Body["idempotentReplay"] != false {
		t.Fatalf("bet = %v", first.Body)
	}

	apptest.Must(t, f.c.Submit(f.op("BET", "bet-2", "100.00")), http.StatusCreated, "outra aposta")

	// replay da primeira: devolve o saldo do processamento ORIGINAL (975), não o atual (875)
	replay := apptest.Must(t, f.c.Submit(f.op("BET", "bet-1", "25.00")), http.StatusOK, "replay")
	if replay.Body["idempotentReplay"] != true || replay.Amount("balance") != "975.00" ||
		replay.Str("transactionId") != first.Str("transactionId") {
		t.Errorf("replay = %v", replay.Body)
	}
	if f.balance() != "875.00" || f.ledgerCount() != 3 {
		t.Errorf("saldo %s, lançamentos %d", f.balance(), f.ledgerCount())
	}
	apptest.AssertReconciled(t, f.app.DB)
}

func TestIdempotencyConflicts(t *testing.T) {
	f := newFixture(t, "100.00")
	apptest.Must(t, f.c.Submit(f.op("BET", "bet-1", "10.00")), http.StatusCreated, "bet")

	// mesma chave, conteúdo diferente
	changed := f.op("BET", "bet-1", "11.00")
	r := apptest.Must(t, f.c.Submit(changed), http.StatusConflict, "chave reutilizada")
	if r.ErrorCode() != "IDEMPOTENCY_KEY_REUSED" {
		t.Errorf("code = %s", r.ErrorCode())
	}

	// mesma chave apontando para outro id externo
	other := f.op("BET", "bet-2", "10.00")
	other.Key = "provider-a:bet-1"
	if r := f.c.Submit(other); r.Status != http.StatusConflict || r.ErrorCode() != "IDEMPOTENCY_KEY_REUSED" {
		t.Errorf("chave em outra operação = %d %s", r.Status, r.ErrorCode())
	}

	// mesma operação financeira com outra chave: não reaplica
	sameOp := f.op("BET", "bet-1", "10.00")
	sameOp.Key = "outra-chave"
	if r := f.c.Submit(sameOp); r.Status != http.StatusConflict || r.ErrorCode() != "DUPLICATE_TRANSACTION" {
		t.Errorf("outra chave = %d %s", r.Status, r.ErrorCode())
	}

	if f.balance() != "90.00" || f.ledgerCount() != 2 {
		t.Errorf("conflitos não podem movimentar: saldo %s, lançamentos %d", f.balance(), f.ledgerCount())
	}

	// o mesmo id externo em OUTRO provedor é outra operação
	pb := f.op("BET", "bet-1", "10.00")
	pb.Provider = "provider-b"
	apptest.Must(t, f.c.Submit(pb), http.StatusCreated, "outro provedor")
}

func TestInvalidInputIsNotPersisted(t *testing.T) {
	f := newFixture(t, "100.00")
	before := apptest.Count(t, f.app.DB, `SELECT count(*) FROM wager_transactions`)

	noKey := f.c.Do("POST", "/wagering/transactions", f.op("BET", "x", "1.00").Body())
	if noKey.Status != 400 {
		t.Errorf("sem Idempotency-Key = %d", noKey.Status)
	}
	cases := map[string]apptest.Op{
		"OPENING externo":        f.op("OPENING", "x1", "1.00"),
		"LOSS diferente de zero": f.op("LOSS", "x2", "1.00"),
		"BET zero":               f.op("BET", "x3", "0.00"),
		"escala errada":          f.op("BET", "x4", "1.0"),
		"REFUND sem referência":  f.op("REFUND", "x5", "1.00"),
		"tipo desconhecido":      f.op("DEPOSIT", "x6", "1.00"),
	}
	for name, op := range cases {
		if r := f.c.Submit(op); r.Status != 400 || r.ErrorCode() != "INVALID_REQUEST" {
			t.Errorf("%s = %d %v", name, r.Status, r.Body)
		}
	}
	if after := apptest.Count(t, f.app.DB, `SELECT count(*) FROM wager_transactions`); after != before {
		t.Errorf("entrada inválida foi gravada: %d -> %d", before, after)
	}
}

func TestLoss_NoLedgerNoVersionChange(t *testing.T) {
	f := newFixture(t, "100.00")
	r := apptest.Must(t, f.c.Submit(f.op("LOSS", "loss-1", "0.00")), http.StatusCreated, "loss")
	if r.Amount("balance") != "100.00" {
		t.Errorf("loss = %v", r.Body)
	}
	w := f.c.Do("GET", "/wallets/"+f.walletID, nil)
	if w.Body["version"].(float64) != 1 || f.ledgerCount() != 1 {
		t.Errorf("versão %v, lançamentos %d", w.Body["version"], f.ledgerCount())
	}
	events := apptest.Count(t, f.app.DB, `SELECT count(*) FROM outbox_events WHERE aggregate_id = $1`, r.Str("transactionId"))
	balanceEvents := apptest.Count(t, f.app.DB, `SELECT count(*) FROM outbox_events
		WHERE event_type = 'WalletBalanceChanged' AND causation_id = $1`, r.Str("transactionId"))
	if events != 1 || balanceEvents != 0 {
		t.Errorf("LOSS: %d eventos da transação, %d de saldo", events, balanceEvents)
	}
}

func TestRejections_ArePersistedAndReplayed(t *testing.T) {
	f := newFixture(t, "100.00")

	r := apptest.Must(t, f.c.Submit(f.op("BET", "bet-big", "500.00")), http.StatusUnprocessableEntity, "sem saldo")
	if r.Str("status") != "REJECTED" || r.Str("failureCode") != "INSUFFICIENT_FUNDS" || r.Body["failureCorrectable"] != false {
		t.Errorf("rejeição = %v", r.Body)
	}
	replay := apptest.Must(t, f.c.Submit(f.op("BET", "bet-big", "500.00")), http.StatusUnprocessableEntity, "replay")
	if replay.Body["idempotentReplay"] != true || replay.Str("failureCode") != "INSUFFICIENT_FUNDS" {
		t.Errorf("replay da rejeição = %v", replay.Body)
	}

	ghost := f.op("BET", "bet-ghost", "1.00")
	ghost.WalletID = uuid.NewString()
	r = apptest.Must(t, f.c.Submit(ghost), http.StatusUnprocessableEntity, "carteira inexistente")
	if r.Str("failureCode") != "WALLET_NOT_FOUND" || r.Body["failureCorrectable"] != true {
		t.Errorf("carteira inexistente = %v", r.Body)
	}

	rejectedEvents := apptest.Count(t, f.app.DB, `SELECT count(*) FROM outbox_events WHERE event_type = 'WagerTransactionRejected'`)
	if rejectedEvents != 2 || f.balance() != "100.00" {
		t.Errorf("eventos de rejeição %d, saldo %s", rejectedEvents, f.balance())
	}
}

func TestReversals(t *testing.T) {
	t.Run("REFUND devolve e não pode ser repetido", func(t *testing.T) {
		f := newFixture(t, "100.00")
		apptest.Must(t, f.c.Submit(f.op("BET", "bet-1", "30.00")), 201, "bet")
		refund := f.op("REFUND", "refund-1", "30.00")
		refund.RefID = "bet-1"
		apptest.Must(t, f.c.Submit(refund), 201, "refund")

		rollback := f.op("ROLLBACK", "rollback-1", "30.00")
		rollback.RefID = "bet-1"
		r := apptest.Must(t, f.c.Submit(rollback), 422, "segunda reversão da mesma aposta")
		if r.Str("failureCode") != "ALREADY_REVERSED" {
			t.Errorf("code = %s", r.Str("failureCode"))
		}
		if f.balance() != "100.00" {
			t.Errorf("saldo = %s (a aposta não pode ser devolvida duas vezes)", f.balance())
		}
		apptest.AssertReconciled(t, f.app.DB)
	})
	t.Run("ROLLBACK de WIN sem saldo tem código próprio", func(t *testing.T) {
		f := newFixture(t, "0.00")
		apptest.Must(t, f.c.Submit(f.op("WIN", "win-1", "50.00")), 201, "win")
		apptest.Must(t, f.c.Submit(f.op("BET", "bet-1", "45.00")), 201, "bet")
		rb := f.op("ROLLBACK", "rb-1", "50.00")
		rb.RefID = "win-1"
		r := apptest.Must(t, f.c.Submit(rb), 422, "rollback sem saldo")
		if r.Str("failureCode") != "REVERSAL_INSUFFICIENT_FUNDS" || f.balance() != "5.00" {
			t.Errorf("code %s, saldo %s", r.Str("failureCode"), f.balance())
		}
		apptest.AssertReconciled(t, f.app.DB)
	})
	t.Run("referência ainda não chegou fica pendente", func(t *testing.T) {
		f := newFixture(t, "100.00")
		refund := f.op("REFUND", "refund-early", "30.00")
		refund.RefID = "bet-future"
		r := apptest.Must(t, f.c.Submit(refund), http.StatusAccepted, "refund antes da aposta")
		if r.Str("status") != "PENDING_REFERENCE" || r.Str("nextAttemptAt") == "" {
			t.Errorf("pendente = %v", r.Body)
		}
		got := apptest.Must(t, f.c.Do("GET", "/providers/provider-a/wagering/transactions/refund-early", nil), 200, "consulta")
		if got.Str("status") != "PENDING_REFERENCE" || got.Str("referenceExternalTransactionId") != "bet-future" {
			t.Errorf("consulta = %v", got.Body)
		}
		pendingEvents := apptest.Count(t, f.app.DB, `SELECT count(*) FROM outbox_events WHERE event_type = 'WagerTransactionPendingReference'`)
		if pendingEvents != 1 || f.balance() != "100.00" {
			t.Errorf("eventos %d, saldo %s", pendingEvents, f.balance())
		}
	})
}

func TestTransactionQueries(t *testing.T) {
	f := newFixture(t, "100.00")
	r := apptest.Must(t, f.c.Submit(f.op("BET", "bet-1", "10.00")), 201, "bet")
	id := r.Str("transactionId")

	byID := apptest.Must(t, f.c.Do("GET", "/wagering/transactions/"+id, nil), 200, "por id")
	byExt := apptest.Must(t, f.c.Do("GET", "/providers/provider-a/wagering/transactions/bet-1", nil), 200, "por id externo")
	for _, got := range []apptest.Response{byID, byExt} {
		if got.Str("transactionId") != id || got.Str("status") != "PROCESSED" || got.Amount("balanceAfter") != "90.00" ||
			got.Str("kind") != "BET" || got.Str("providerId") != "provider-a" {
			t.Errorf("consulta = %v", got.Body)
		}
	}
	if r := f.c.Do("GET", "/wagering/transactions/"+uuid.NewString(), nil); r.Status != 404 {
		t.Errorf("inexistente = %d", r.Status)
	}
	if r := f.c.Do("GET", "/providers/provider-b/wagering/transactions/bet-1", nil); r.Status != 404 {
		t.Errorf("outro provedor = %d", r.Status)
	}
}
