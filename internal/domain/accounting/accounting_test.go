package accounting_test

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Taino-Edu/Desafio-go-arqui/internal/domain/accounting"
	"github.com/Taino-Edu/Desafio-go-arqui/internal/domain/domainerr"
	"github.com/Taino-Edu/Desafio-go-arqui/internal/domain/money"
	"github.com/Taino-Edu/Desafio-go-arqui/internal/domain/wagering"
	"github.com/Taino-Edu/Desafio-go-arqui/internal/domain/wallet"
)

var t0 = time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)

func brl(t *testing.T, s string) money.Money {
	t.Helper()
	m, err := money.Parse(s, money.BRL)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// must desembrulha (valor, erro) dos construtores; erro aqui é bug do teste.
func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}

func TestAccounts(t *testing.T) {
	id := uuid.MustParse("0192f291-27dd-7d3f-8071-5f8685deef37")
	cases := []struct {
		acc    accounting.Account
		code   string
		typ    accounting.AccountType
		normal wallet.Direction
	}{
		{must(accounting.WalletAccount(id, money.BRL)), "wallet:0192f291-27dd-7d3f-8071-5f8685deef37", accounting.Liability, wallet.Credit},
		{must(accounting.CashAccount(money.BRL)), "cash:BRL", accounting.Asset, wallet.Debit},
		{must(accounting.ProviderAccount("provider-a", money.BRL)), "provider:provider-a:BRL", accounting.Revenue, wallet.Credit},
	}
	for _, c := range cases {
		if c.acc.Code() != c.code || c.acc.Type() != c.typ || c.acc.Type().NormalSide() != c.normal {
			t.Errorf("%s: type %s normal %s", c.acc.Code(), c.acc.Type(), c.acc.Type().NormalSide())
		}
	}
	for _, err := range []error{
		func() error { _, err := accounting.WalletAccount(uuid.Nil, money.BRL); return err }(),
		func() error { _, err := accounting.ProviderAccount(" ", money.BRL); return err }(),
		func() error { _, err := accounting.CashAccount(money.Currency{}); return err }(),
	} {
		if !errors.Is(err, domainerr.ErrValidation) {
			t.Errorf("err = %v", err)
		}
	}
}

func TestNewJournalEntry_Rules(t *testing.T) {
	tx := uuid.New()
	cash := must(accounting.CashAccount(money.BRL))
	prov := must(accounting.ProviderAccount("provider-a", money.BRL))
	w := must(accounting.WalletAccount(uuid.New(), money.BRL))
	usd := must(accounting.CashAccount(must(money.NewCurrency("USD"))))
	tenUSD := must(money.Parse("10.00", must(money.NewCurrency("USD"))))
	d := func(a accounting.Account, m money.Money) accounting.Posting {
		return accounting.Posting{Account: a, Direction: wallet.Debit, Amount: m}
	}
	c := func(a accounting.Account, m money.Money) accounting.Posting {
		return accounting.Posting{Account: a, Direction: wallet.Credit, Amount: m}
	}

	// válido com três partidas: 10 = 6 + 4
	if _, err := accounting.NewJournalEntry(tx, t0, d(w, brl(t, "10.00")), c(prov, brl(t, "6.00")), c(cash, brl(t, "4.00"))); err != nil {
		t.Fatalf("três partidas balanceadas: %v", err)
	}

	bad := map[string][]accounting.Posting{
		"desbalanceado":        {d(w, brl(t, "10.00")), c(prov, brl(t, "9.99"))},
		"uma partida":          {d(w, brl(t, "10.00"))},
		"conta repetida":       {d(w, brl(t, "10.00")), c(w, brl(t, "10.00"))},
		"valor zero":           {d(w, brl(t, "0.00")), c(prov, brl(t, "0.00"))},
		"direção inválida":     {{Account: w, Direction: "SIDEWAYS", Amount: brl(t, "1.00")}, c(prov, brl(t, "1.00"))},
		"conta sem código":     {d(accounting.Account{}, brl(t, "1.00")), c(prov, brl(t, "1.00"))},
		"moedas misturadas":    {d(usd, tenUSD), c(prov, brl(t, "10.00"))},
		"conta de outra moeda": {d(usd, brl(t, "10.00")), c(prov, brl(t, "10.00"))},
	}
	for name, ps := range bad {
		if _, err := accounting.NewJournalEntry(tx, t0, ps...); err == nil {
			t.Errorf("%s: aceito", name)
		}
	}
	if _, err := accounting.NewJournalEntry(uuid.Nil, t0, d(w, brl(t, "1.00")), c(prov, brl(t, "1.00"))); err == nil {
		t.Error("sem transação: aceito")
	}
}

// A partida da carteira repete o lançamento do ledger; a contrapartida fica
// do lado oposto, no mesmo valor.
func TestForWalletMovement(t *testing.T) {
	w := must(wallet.Rehydrate(wallet.RehydrateParams{
		ID: uuid.New(), PlayerID: uuid.New(), Balance: brl(t, "100.00"), Version: 1, CreatedAt: t0, UpdatedAt: t0,
	}))
	prov := must(accounting.ProviderAccount("provider-a", money.BRL))

	bet := must(w.Debit(uuid.New(), uuid.New(), brl(t, "25.00"), t0))
	j := must(accounting.ForWalletMovement(bet, prov))
	ps := j.Postings()
	if j.TransactionID() != bet.TransactionID() || len(ps) != 2 {
		t.Fatalf("lançamento = %+v", j)
	}
	if ps[0].Account.Code() != "wallet:"+w.ID().String() || ps[0].Direction != wallet.Debit || !ps[0].Amount.Equal(bet.Amount()) {
		t.Errorf("partida da carteira = %+v", ps[0])
	}
	if ps[1].Account.Code() != prov.Code() || ps[1].Direction != wallet.Credit || !ps[1].Amount.Equal(bet.Amount()) {
		t.Errorf("contrapartida = %+v", ps[1])
	}

	win := must(w.Credit(uuid.New(), uuid.New(), brl(t, "40.00"), t0))
	jw := must(accounting.ForWalletMovement(win, prov))
	ps = jw.Postings()
	if ps[0].Direction != wallet.Credit || ps[1].Direction != wallet.Debit {
		t.Errorf("prêmio: %s / %s", ps[0].Direction, ps[1].Direction)
	}

	// Postings devolve cópia: o lançamento é imutável
	ps[0].Amount = brl(t, "1.00")
	if !jw.Postings()[0].Amount.Equal(win.Amount()) {
		t.Error("cópia alterou o lançamento")
	}
}

// A contrapartida segue a natureza da operação: abertura no caixa, operação
// de provedor na conta DAQUELE provedor.
func TestForTransaction_Counterpart(t *testing.T) {
	walletID, playerID := uuid.New(), uuid.New()
	w := must(wallet.Rehydrate(wallet.RehydrateParams{
		ID: walletID, PlayerID: playerID, Balance: brl(t, "100.00"), Version: 1, CreatedAt: t0, UpdatedAt: t0,
	}))

	opening := must(wagering.NewOpening(uuid.New(), walletID, playerID, brl(t, "100.00"), t0))
	openEntry := must(wallet.NewLedgerEntry(wallet.LedgerEntryParams{
		ID: uuid.New(), WalletID: walletID, TransactionID: opening.ID(), WalletVersion: 1, Direction: wallet.Credit,
		Amount: brl(t, "100.00"), BalanceBefore: brl(t, "0.00"), BalanceAfter: brl(t, "100.00"), CreatedAt: t0,
	}))
	ps := must(accounting.ForTransaction(opening, openEntry)).Postings()
	if ps[1].Account.Code() != "cash:BRL" || ps[1].Direction != wallet.Debit {
		t.Errorf("abertura: %s %s", ps[1].Account.Code(), ps[1].Direction)
	}

	bet := must(wagering.NewExternal(wagering.NewExternalParams{
		ID: uuid.New(), Kind: wagering.KindBet, WalletID: walletID, PlayerID: playerID, Amount: brl(t, "25.00"),
		External: wagering.External{ProviderID: "provider-b", ExternalTransactionID: "bet-1", IdempotencyKey: "k",
			PayloadHash: "h", RoundID: "r", GameID: "g"},
		Now: t0,
	}))
	betEntry := must(w.Debit(uuid.New(), bet.ID(), brl(t, "25.00"), t0))
	ps = must(accounting.ForTransaction(bet, betEntry)).Postings()
	if ps[1].Account.Code() != "provider:provider-b:BRL" || ps[1].Direction != wallet.Credit {
		t.Errorf("aposta: %s %s", ps[1].Account.Code(), ps[1].Direction)
	}

	// lançamento de outra transação: recusado
	if _, err := accounting.ForTransaction(opening, betEntry); err == nil {
		t.Error("lançamento de outra transação aceito")
	}
}
