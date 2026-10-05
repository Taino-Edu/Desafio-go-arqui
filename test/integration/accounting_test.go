//go:build integration

package integration

import (
	"net/http"
	"testing"

	"github.com/google/uuid"

	"github.com/Taino-Edu/Desafio-go-arqui/internal/testsupport/apptest"
	"github.com/Taino-Edu/Desafio-go-arqui/internal/testsupport/idptest"
)

func moneyAt(r apptest.Response, path ...string) string {
	var cur any = r.Body
	for _, p := range path {
		cur = cur.(map[string]any)[p]
	}
	return cur.(map[string]any)["amount"].(string)
}

// Pela aplicação real: cada operação vira um lançamento em partidas
// dobradas, e o balancete fecha com os números esperados.
//
//	carteira 1: abre 100, BET 30 (provider-a), WIN 50 (provider-a), LOSS 0
//	carteira 2: abre 100, BET 20 (provider-b), REFUND 20 (provider-b),
//	            BET 500 recusada (saldo insuficiente: sem lançamento)
//
//	cash:BRL (ativo)              D 200            saldo 200
//	provider:provider-a (receita) C 30  / D 50     saldo −20 (pagou mais do que recebeu)
//	provider:provider-b (receita) C 20  / D 20     saldo 0
//	carteiras (passivo)           C 270 / D 50     saldo 220 = 120 + 100
//	Σ débitos = 200 + 50 + 20 + 50 = 320 = 30 + 20 + 270 = Σ créditos
//	ativo 200 = passivo 220 + receita (−20)
func TestAccounting_TrialBalance(t *testing.T) {
	a := apptest.Start(t)
	c := a.Client
	p1, p2 := uuid.NewString(), uuid.NewString()
	w1, w2 := c.OpenWallet(p1, "100.00"), c.OpenWallet(p2, "100.00")
	op := func(provider, ext, player, wallet, kind, amount, ref string) apptest.Op {
		return apptest.Op{Provider: provider, ExternalID: ext, PlayerID: player, WalletID: wallet,
			Round: "r", Game: "g", Kind: kind, Amount: amount, RefID: ref}
	}
	apptest.Must(t, c.Submit(op("provider-a", "bet-1", p1, w1, "BET", "30.00", "")), http.StatusCreated, "bet")
	apptest.Must(t, c.Submit(op("provider-a", "win-1", p1, w1, "WIN", "50.00", "bet-1")), http.StatusCreated, "win")
	apptest.Must(t, c.Submit(op("provider-a", "loss-1", p1, w1, "LOSS", "0.00", "")), http.StatusCreated, "loss")
	apptest.Must(t, c.Submit(op("provider-b", "bet-2", p2, w2, "BET", "20.00", "")), http.StatusCreated, "bet 2")
	apptest.Must(t, c.Submit(op("provider-b", "refund-2", p2, w2, "REFUND", "20.00", "bet-2")), http.StatusCreated, "refund")
	if r := c.Submit(op("provider-b", "bet-3", p2, w2, "BET", "500.00", "")); r.Str("status") != "REJECTED" {
		t.Fatalf("aposta sem saldo: %v", r.Body)
	}

	tb := apptest.Must(t, c.Do("GET", "/accounting/trial-balance?currency=BRL", nil), http.StatusOK, "trial balance")
	if tb.Body["balanced"] != true || moneyAt(tb, "totalDebits") != moneyAt(tb, "totalCredits") {
		t.Errorf("não fecha: %v", tb.Body)
	}
	if moneyAt(tb, "totalDebits") != "320.00" { // 200 + 50 + 20 + 50 (tabela acima)
		t.Errorf("total de débitos = %s", moneyAt(tb, "totalDebits"))
	}
	if moneyAt(tb, "wallets", "journalBalance") != "220.00" || moneyAt(tb, "wallets", "storedBalance") != "220.00" ||
		tb.Body["wallets"].(map[string]any)["consistent"] != true || tb.Body["wallets"].(map[string]any)["count"] != float64(2) {
		t.Errorf("carteiras = %v", tb.Body["wallets"])
	}
	want := map[string]struct{ typ, debits, credits, balance string }{
		"cash:BRL":                {"ASSET", "200.00", "0.00", "200.00"},
		"provider:provider-a:BRL": {"REVENUE", "50.00", "30.00", "-20.00"},
		"provider:provider-b:BRL": {"REVENUE", "20.00", "20.00", "0.00"},
		"wallet:*":                {"LIABILITY", "50.00", "270.00", "220.00"},
	}
	accounts := tb.Body["accounts"].([]any)
	if len(accounts) != len(want) {
		t.Fatalf("contas = %v", accounts)
	}
	for _, raw := range accounts {
		acc := raw.(map[string]any)
		w, ok := want[acc["account"].(string)]
		got := struct{ typ, debits, credits, balance string }{acc["type"].(string),
			acc["debits"].(map[string]any)["amount"].(string), acc["credits"].(map[string]any)["amount"].(string),
			acc["balance"].(map[string]any)["amount"].(string)}
		if !ok || got != w {
			t.Errorf("%s = %+v, want %+v", acc["account"], got, w)
		}
	}

	// reconciliação: saldo, ledger e razão dizem o mesmo
	rec := apptest.Must(t, c.Do("POST", "/wallets/"+w1+"/reconciliation", nil), http.StatusOK, "reconcile")
	if rec.Body["consistent"] != true || moneyAt(rec, "journalBalance") != "120.00" {
		t.Errorf("reconciliação = %v", rec.Body)
	}
	apptest.AssertReconciled(t, a.DB)

	// moeda sem movimento: balancete vazio, fechado
	usd := apptest.Must(t, c.Do("GET", "/accounting/trial-balance?currency=USD", nil), http.StatusOK, "usd")
	if usd.Body["balanced"] != true || len(usd.Body["accounts"].([]any)) != 0 {
		t.Errorf("USD = %v", usd.Body)
	}
	// moeda inválida e permissões
	if r := c.Do("GET", "/accounting/trial-balance?currency=reais", nil); r.Status != http.StatusBadRequest {
		t.Errorf("moeda inválida: %d", r.Status)
	}
	if r := c.As(idptest.Token(t, "provider-a")).Do("GET", "/accounting/trial-balance?currency=BRL", nil); r.Status != http.StatusForbidden {
		t.Errorf("provedor lendo o balancete: %d", r.Status)
	}
}
