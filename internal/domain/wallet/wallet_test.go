package wallet_test

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Taino-Edu/Desafio-go-arqui/internal/domain/domainerr"
	"github.com/Taino-Edu/Desafio-go-arqui/internal/domain/events"
	"github.com/Taino-Edu/Desafio-go-arqui/internal/domain/money"
	"github.com/Taino-Edu/Desafio-go-arqui/internal/domain/wallet"
)

var t0 = time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)

func brl(t *testing.T, s string) money.Money {
	t.Helper()
	m, err := money.Parse(s, money.BRL)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func open(t *testing.T, initial string) *wallet.Wallet {
	t.Helper()
	w, _, err := wallet.Open(wallet.OpenParams{
		ID: uuid.New(), PlayerID: uuid.New(), InitialBalance: brl(t, initial),
		OpeningTransactionID: uuid.New(), LedgerEntryID: uuid.New(), Now: t0,
	})
	if err != nil {
		t.Fatal(err)
	}
	w.PullEvents() // descarta os eventos da abertura
	return w
}

func TestOpen_PositiveBalance(t *testing.T) {
	txID := uuid.New()
	w, entry, err := wallet.Open(wallet.OpenParams{
		ID: uuid.New(), PlayerID: uuid.New(), InitialBalance: brl(t, "1000.00"),
		OpeningTransactionID: txID, LedgerEntryID: uuid.New(), Now: t0,
	})
	if err != nil {
		t.Fatal(err)
	}
	if w.Version() != 1 {
		t.Errorf("version = %d, want 1", w.Version())
	}
	if w.Balance().Amount() != "1000.00" || w.Currency() != money.BRL {
		t.Errorf("balance = %v", w.Balance())
	}
	if entry == nil {
		t.Fatal("abertura com saldo positivo deve gerar lançamento")
	}
	if entry.Direction() != wallet.Credit || entry.TransactionID() != txID || entry.WalletVersion() != 1 ||
		entry.BalanceBefore().Amount() != "0.00" || entry.BalanceAfter().Amount() != "1000.00" {
		t.Errorf("entry = %+v", entry)
	}

	evs := w.PullEvents()
	if len(evs) != 1 {
		t.Fatalf("events = %d, want 1", len(evs))
	}
	ev, ok := evs[0].(events.WalletBalanceChanged)
	if !ok || ev.WalletVersion != 1 || ev.Direction != "CREDIT" || ev.BalanceAfter.Amount() != "1000.00" {
		t.Errorf("event = %+v", evs[0])
	}
	if len(w.PullEvents()) != 0 {
		t.Error("PullEvents deve limpar a lista")
	}
}

func TestOpen_ZeroBalance(t *testing.T) {
	zero, _ := money.Zero(money.BRL)
	w, entry, err := wallet.Open(wallet.OpenParams{
		ID: uuid.New(), PlayerID: uuid.New(), InitialBalance: zero, Now: t0,
	})
	if err != nil {
		t.Fatal(err)
	}
	if entry != nil || len(w.PullEvents()) != 0 {
		t.Error("saldo inicial zero não cria lançamento nem evento")
	}
	if w.Version() != 1 || !w.Balance().IsZero() {
		t.Errorf("wallet = v%d %v", w.Version(), w.Balance())
	}
}

func TestOpen_Invalid(t *testing.T) {
	neg, _ := money.FromMinorUnits(-1, money.BRL)
	valid := wallet.OpenParams{
		ID: uuid.New(), PlayerID: uuid.New(), InitialBalance: brl(t, "1.00"),
		OpeningTransactionID: uuid.New(), LedgerEntryID: uuid.New(), Now: t0,
	}
	tests := map[string]func(p *wallet.OpenParams){
		"sem id":                 func(p *wallet.OpenParams) { p.ID = uuid.Nil },
		"sem jogador":            func(p *wallet.OpenParams) { p.PlayerID = uuid.Nil },
		"saldo não inicializado": func(p *wallet.OpenParams) { p.InitialBalance = money.Money{} },
		"saldo negativo":         func(p *wallet.OpenParams) { p.InitialBalance = neg },
		"sem instante":           func(p *wallet.OpenParams) { p.Now = time.Time{} },
		"sem id de transação":    func(p *wallet.OpenParams) { p.OpeningTransactionID = uuid.Nil },
		"sem id de lançamento":   func(p *wallet.OpenParams) { p.LedgerEntryID = uuid.Nil },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			p := valid
			mutate(&p)
			if _, _, err := wallet.Open(p); !errors.Is(err, domainerr.ErrValidation) {
				t.Errorf("err = %v, want ErrValidation", err)
			}
		})
	}
}

func TestDebitCredit(t *testing.T) {
	w := open(t, "100.00")

	e, err := w.Debit(uuid.New(), uuid.New(), brl(t, "80.00"), t0.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if w.Balance().Amount() != "20.00" || w.Version() != 2 {
		t.Errorf("após débito: %v v%d", w.Balance(), w.Version())
	}
	if e.BalanceBefore().Amount() != "100.00" || e.BalanceAfter().Amount() != "20.00" || e.Direction() != wallet.Debit || e.WalletVersion() != 2 {
		t.Errorf("entry = %+v", e)
	}

	if _, err := w.Credit(uuid.New(), uuid.New(), brl(t, "5.50"), t0.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	if w.Balance().Amount() != "25.50" || w.Version() != 3 {
		t.Errorf("após crédito: %v v%d", w.Balance(), w.Version())
	}
	if !w.UpdatedAt().Equal(t0.Add(2 * time.Second)) {
		t.Errorf("updatedAt = %v", w.UpdatedAt())
	}

	evs := w.PullEvents()
	if len(evs) != 2 {
		t.Fatalf("events = %d, want 2", len(evs))
	}
	last := evs[1].(events.WalletBalanceChanged)
	if last.WalletVersion != 3 || last.BalanceBefore.Amount() != "20.00" || last.BalanceAfter.Amount() != "25.50" {
		t.Errorf("último evento = %+v", last)
	}
}

func TestDebit_InsufficientFundsLeavesWalletUnchanged(t *testing.T) {
	w := open(t, "20.00")
	_, err := w.Debit(uuid.New(), uuid.New(), brl(t, "80.00"), t0)
	if !errors.Is(err, wallet.ErrInsufficientFunds) {
		t.Fatalf("err = %v, want ErrInsufficientFunds", err)
	}
	if w.Balance().Amount() != "20.00" || w.Version() != 1 || len(w.PullEvents()) != 0 {
		t.Errorf("carteira mudou após rejeição: %v v%d", w.Balance(), w.Version())
	}
}

func TestDebit_ExactBalanceGoesToZero(t *testing.T) {
	w := open(t, "80.00")
	if _, err := w.Debit(uuid.New(), uuid.New(), brl(t, "80.00"), t0); err != nil {
		t.Fatal(err)
	}
	if !w.Balance().IsZero() {
		t.Errorf("balance = %v", w.Balance())
	}
}

func TestMove_Invalid(t *testing.T) {
	w := open(t, "100.00")
	usd, _ := money.Parse("1.00", money.USD)
	zero, _ := money.Zero(money.BRL)

	if _, err := w.Credit(uuid.New(), uuid.New(), usd, t0); !errors.Is(err, money.ErrCurrencyMismatch) {
		t.Errorf("moeda diferente: err = %v", err)
	}
	if _, err := w.Debit(uuid.New(), uuid.New(), zero, t0); !errors.Is(err, domainerr.ErrValidation) {
		t.Errorf("valor zero: err = %v", err)
	}
	if _, err := w.Credit(uuid.Nil, uuid.New(), brl(t, "1.00"), t0); !errors.Is(err, domainerr.ErrValidation) {
		t.Errorf("sem id de lançamento: err = %v", err)
	}
	if w.Version() != 1 || w.Balance().Amount() != "100.00" {
		t.Error("carteira mudou após erro")
	}

	var empty wallet.Wallet
	if _, err := empty.Credit(uuid.New(), uuid.New(), brl(t, "1.00"), t0); !errors.Is(err, domainerr.ErrUninitialized) {
		t.Errorf("carteira não inicializada: err = %v", err)
	}
}

func TestRehydrate(t *testing.T) {
	p := wallet.RehydrateParams{
		ID: uuid.New(), PlayerID: uuid.New(), Balance: brl(t, "975.00"),
		Version: 7, CreatedAt: t0, UpdatedAt: t0.Add(time.Hour),
	}
	w, err := wallet.Rehydrate(p)
	if err != nil {
		t.Fatal(err)
	}
	if w.Version() != 7 || w.Balance().Amount() != "975.00" {
		t.Errorf("wallet = v%d %v", w.Version(), w.Balance())
	}
	if len(w.PullEvents()) != 0 {
		t.Error("reidratação não pode emitir eventos")
	}

	neg, _ := money.FromMinorUnits(-1, money.BRL)
	bad := map[string]func(*wallet.RehydrateParams){
		"versão zero":            func(p *wallet.RehydrateParams) { p.Version = 0 },
		"saldo negativo":         func(p *wallet.RehydrateParams) { p.Balance = neg },
		"updatedAt antes":        func(p *wallet.RehydrateParams) { p.UpdatedAt = t0.Add(-time.Second) },
		"sem jogador":            func(p *wallet.RehydrateParams) { p.PlayerID = uuid.Nil },
		"saldo não inicializado": func(p *wallet.RehydrateParams) { p.Balance = money.Money{} },
	}
	for name, mutate := range bad {
		t.Run(name, func(t *testing.T) {
			q := p
			mutate(&q)
			if _, err := wallet.Rehydrate(q); !errors.Is(err, domainerr.ErrValidation) {
				t.Errorf("err = %v, want ErrValidation", err)
			}
		})
	}
}

func TestNewLedgerEntry(t *testing.T) {
	base := wallet.LedgerEntryParams{
		ID: uuid.New(), WalletID: uuid.New(), TransactionID: uuid.New(), WalletVersion: 2,
		Direction: wallet.Debit, Amount: brl(t, "25.00"),
		BalanceBefore: brl(t, "100.00"), BalanceAfter: brl(t, "75.00"), CreatedAt: t0,
	}
	if _, err := wallet.NewLedgerEntry(base); err != nil {
		t.Fatalf("lançamento válido: %v", err)
	}
	credit := base
	credit.Direction, credit.BalanceAfter = wallet.Credit, brl(t, "125.00")
	if _, err := wallet.NewLedgerEntry(credit); err != nil {
		t.Fatalf("crédito válido: %v", err)
	}

	usd, _ := money.Parse("25.00", money.USD)
	bad := map[string]func(*wallet.LedgerEntryParams){
		"conta não fecha":  func(p *wallet.LedgerEntryParams) { p.BalanceAfter = brl(t, "76.00") },
		"direção trocada":  func(p *wallet.LedgerEntryParams) { p.Direction = wallet.Credit },
		"direção inválida": func(p *wallet.LedgerEntryParams) { p.Direction = "SIDEWAYS" },
		"valor zero":       func(p *wallet.LedgerEntryParams) { p.Amount, _ = money.Zero(money.BRL) },
		"saldo final negativo": func(p *wallet.LedgerEntryParams) {
			p.Amount = brl(t, "200.00")
			p.BalanceAfter, _ = money.FromMinorUnits(-10000, money.BRL)
		},
		"sem transação":   func(p *wallet.LedgerEntryParams) { p.TransactionID = uuid.Nil },
		"sem instante":    func(p *wallet.LedgerEntryParams) { p.CreatedAt = time.Time{} },
		"versão zero":     func(p *wallet.LedgerEntryParams) { p.WalletVersion = 0 },
		"moeda diferente": func(p *wallet.LedgerEntryParams) { p.Amount = usd },
	}
	for name, mutate := range bad {
		t.Run(name, func(t *testing.T) {
			p := base
			mutate(&p)
			if _, err := wallet.NewLedgerEntry(p); err == nil {
				t.Error("esperava erro")
			}
		})
	}
}

func TestDirectionOpposite(t *testing.T) {
	if wallet.Debit.Opposite() != wallet.Credit || wallet.Credit.Opposite() != wallet.Debit {
		t.Error("Opposite incorreto")
	}
}
