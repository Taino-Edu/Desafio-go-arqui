package wagering_test

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Taino-Edu/Desafio-go-arqui/internal/domain/domainerr"
	"github.com/Taino-Edu/Desafio-go-arqui/internal/domain/events"
	"github.com/Taino-Edu/Desafio-go-arqui/internal/domain/money"
	"github.com/Taino-Edu/Desafio-go-arqui/internal/domain/wagering"
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

// fixture: uma carteira e helpers para criar operações sobre ela.
type fixture struct {
	t      *testing.T
	wallet *wallet.Wallet
	seq    int
}

func newFixture(t *testing.T, balance string) *fixture {
	t.Helper()
	w, err := wallet.Rehydrate(wallet.RehydrateParams{
		ID: uuid.New(), PlayerID: uuid.New(), Balance: brl(t, balance),
		Version: 1, CreatedAt: t0, UpdatedAt: t0,
	})
	if err != nil {
		t.Fatal(err)
	}
	return &fixture{t: t, wallet: w}
}

type opt func(*wagering.NewExternalParams)

func withRef(extID string) opt {
	return func(p *wagering.NewExternalParams) { p.External.ReferenceExternalTransactionID = extID }
}
func withRound(r string) opt {
	return func(p *wagering.NewExternalParams) { p.External.RoundID = r }
}
func withProvider(pr string) opt {
	return func(p *wagering.NewExternalParams) { p.External.ProviderID = pr }
}

func (f *fixture) params(kind wagering.Kind, extID, amount string, opts ...opt) wagering.NewExternalParams {
	p := wagering.NewExternalParams{
		ID: uuid.New(), Kind: kind, WalletID: f.wallet.ID(), PlayerID: f.wallet.PlayerID(),
		Amount: brl(f.t, amount), Now: t0,
		External: wagering.External{
			ProviderID: "provider-a", ExternalTransactionID: extID,
			IdempotencyKey: "provider-a:" + extID, PayloadHash: "hash-" + extID,
			RoundID: "round-1", GameID: "fortune-chimp",
		},
	}
	for _, o := range opts {
		o(&p)
	}
	return p
}

func (f *fixture) newTx(kind wagering.Kind, extID, amount string, opts ...opt) *wagering.WagerTransaction {
	f.t.Helper()
	tx, err := wagering.NewExternal(f.params(kind, extID, amount, opts...))
	if err != nil {
		f.t.Fatalf("NewExternal(%s %s): %v", kind, extID, err)
	}
	return tx
}

func (f *fixture) apply(tx, ref *wagering.WagerTransaction) wagering.Outcome {
	f.t.Helper()
	f.seq++
	out, err := wagering.Apply(wagering.ApplyInput{
		Transaction: tx, Wallet: f.wallet, Reference: ref,
		LedgerEntryID: uuid.New(), Now: t0.Add(time.Duration(f.seq) * time.Second),
	})
	if err != nil {
		f.t.Fatalf("Apply(%s): %v", tx.Kind(), err)
	}
	return out
}

// processed cria e aplica uma operação, exigindo sucesso.
func (f *fixture) processed(kind wagering.Kind, extID, amount string, ref *wagering.WagerTransaction, opts ...opt) *wagering.WagerTransaction {
	f.t.Helper()
	tx := f.newTx(kind, extID, amount, opts...)
	if out := f.apply(tx, ref); out.Result != wagering.ResultProcessed {
		f.t.Fatalf("%s %s: result = %s (%s)", kind, extID, out.Result, tx.FailureCode())
	}
	return tx
}

func (f *fixture) balance() string { return f.wallet.Balance().Amount() }

// ---------- tipos e criação ----------

func TestParseExternalKind(t *testing.T) {
	for _, k := range []string{"BET", "WIN", "LOSS", "REFUND", "ROLLBACK"} {
		if _, err := wagering.ParseExternalKind(k); err != nil {
			t.Errorf("%s: %v", k, err)
		}
	}
	for _, k := range []string{"OPENING", "", "bet", "DEPOSIT"} {
		if _, err := wagering.ParseExternalKind(k); !errors.Is(err, domainerr.ErrValidation) {
			t.Errorf("%q: err = %v, want ErrValidation", k, err)
		}
	}
}

// Política de valor zero e de referência de cada tipo.
func TestNewExternal_AmountAndReferenceRules(t *testing.T) {
	f := newFixture(t, "100.00")
	tests := []struct {
		name   string
		kind   wagering.Kind
		amount string
		opts   []opt
		ok     bool
	}{
		{"BET positivo", wagering.KindBet, "10.00", nil, true},
		{"BET zero", wagering.KindBet, "0.00", nil, false},
		{"BET com referência", wagering.KindBet, "10.00", []opt{withRef("x")}, false},
		{"WIN positivo", wagering.KindWin, "10.00", nil, true},
		{"WIN com referência", wagering.KindWin, "10.00", []opt{withRef("bet-1")}, true},
		{"WIN zero", wagering.KindWin, "0.00", nil, false},
		{"LOSS zero", wagering.KindLoss, "0.00", nil, true},
		{"LOSS positivo", wagering.KindLoss, "0.01", nil, false},
		{"LOSS com referência", wagering.KindLoss, "0.00", []opt{withRef("x")}, false},
		{"REFUND com referência", wagering.KindRefund, "10.00", []opt{withRef("bet-1")}, true},
		{"REFUND sem referência", wagering.KindRefund, "10.00", nil, false},
		{"REFUND zero", wagering.KindRefund, "0.00", []opt{withRef("bet-1")}, false},
		{"ROLLBACK com referência", wagering.KindRollback, "10.00", []opt{withRef("bet-1")}, true},
		{"ROLLBACK sem referência", wagering.KindRollback, "10.00", nil, false},
		{"ROLLBACK zero", wagering.KindRollback, "0.00", []opt{withRef("bet-1")}, false},
		{"referência a si mesma", wagering.KindRefund, "10.00", []opt{withRef("self")}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			extID := "tx-1"
			if tt.name == "referência a si mesma" {
				extID = "self"
			}
			tx, err := wagering.NewExternal(f.params(tt.kind, extID, tt.amount, tt.opts...))
			if tt.ok {
				if err != nil {
					t.Fatalf("err = %v", err)
				}
				if tx.Status() != wagering.StatusPending || tx.Origin() != wagering.OriginExternal {
					t.Errorf("status = %s origin = %s", tx.Status(), tx.Origin())
				}
				if len(tx.PullEvents()) != 0 {
					t.Error("criação em PENDING não emite evento")
				}
				return
			}
			if !errors.Is(err, domainerr.ErrValidation) {
				t.Errorf("err = %v, want ErrValidation", err)
			}
		})
	}
}

func TestNewExternal_RequiredFields(t *testing.T) {
	f := newFixture(t, "100.00")
	mutations := map[string]func(*wagering.NewExternalParams){
		"OPENING":        func(p *wagering.NewExternalParams) { p.Kind = wagering.KindOpening },
		"tipo vazio":     func(p *wagering.NewExternalParams) { p.Kind = "" },
		"sem id":         func(p *wagering.NewExternalParams) { p.ID = uuid.Nil },
		"sem carteira":   func(p *wagering.NewExternalParams) { p.WalletID = uuid.Nil },
		"sem jogador":    func(p *wagering.NewExternalParams) { p.PlayerID = uuid.Nil },
		"sem valor":      func(p *wagering.NewExternalParams) { p.Amount = money.Money{} },
		"sem provedor":   func(p *wagering.NewExternalParams) { p.External.ProviderID = "" },
		"sem id externo": func(p *wagering.NewExternalParams) { p.External.ExternalTransactionID = "" },
		"sem chave":      func(p *wagering.NewExternalParams) { p.External.IdempotencyKey = "" },
		"sem hash":       func(p *wagering.NewExternalParams) { p.External.PayloadHash = "" },
		"sem rodada":     func(p *wagering.NewExternalParams) { p.External.RoundID = "" },
		"sem jogo":       func(p *wagering.NewExternalParams) { p.External.GameID = "" },
		"sem instante":   func(p *wagering.NewExternalParams) { p.Now = time.Time{} },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			p := f.params(wagering.KindBet, "tx-1", "10.00")
			mutate(&p)
			if _, err := wagering.NewExternal(p); !errors.Is(err, domainerr.ErrValidation) {
				t.Errorf("err = %v, want ErrValidation", err)
			}
		})
	}
}

// ---------- abertura interna ----------

func TestNewOpening(t *testing.T) {
	walletID, playerID := uuid.New(), uuid.New()
	tx, err := wagering.NewOpening(uuid.New(), walletID, playerID, brl(t, "1000.00"), t0)
	if err != nil {
		t.Fatal(err)
	}
	if tx.Kind() != wagering.KindOpening || tx.Origin() != wagering.OriginInternal || tx.Status() != wagering.StatusProcessed {
		t.Errorf("opening = %s %s %s", tx.Kind(), tx.Origin(), tx.Status())
	}
	if tx.External() != nil {
		t.Error("OPENING não tem metadados externos")
	}
	if bal, ok := tx.BalanceAfter(); !ok || bal.Amount() != "1000.00" {
		t.Errorf("balanceAfter = %v, %v", bal, ok)
	}
	evs := tx.PullEvents()
	if len(evs) != 1 {
		t.Fatalf("events = %d", len(evs))
	}
	ev, ok := evs[0].(events.WagerTransactionProcessed)
	if !ok || ev.Kind != "OPENING" || ev.External != nil || ev.WalletID != walletID {
		t.Errorf("event = %+v", evs[0])
	}

	zero, _ := money.Zero(money.BRL)
	if _, err := wagering.NewOpening(uuid.New(), walletID, playerID, zero, t0); !errors.Is(err, domainerr.ErrValidation) {
		t.Errorf("abertura com zero deveria falhar: %v", err)
	}
}

// ---------- máquina de estados ----------

func TestTerminalStatesDoNotTransition(t *testing.T) {
	f := newFixture(t, "100.00")
	next := t0.Add(time.Minute)

	processed := f.processed(wagering.KindBet, "bet-1", "10.00", nil)
	rejected := f.newTx(wagering.KindBet, "bet-2", "999.00")
	if f.apply(rejected, nil).Result != wagering.ResultRejected {
		t.Fatal("esperava rejeição")
	}
	failed := f.newTx(wagering.KindBet, "bet-3", "1.00")
	if err := failed.MarkFailed(t0); err != nil {
		t.Fatal(err)
	}

	for _, tx := range []*wagering.WagerTransaction{processed, rejected, failed} {
		t.Run(string(tx.Status()), func(t *testing.T) {
			errs := []error{
				tx.MarkProcessed(brl(t, "1.00"), nil, next),
				tx.MarkRejected(wagering.FailureInsufficientFunds, next),
				tx.MarkFailed(next),
				tx.RescheduleReference(next, next),
			}
			for i, err := range errs {
				if !errors.Is(err, wagering.ErrInvalidTransition) {
					t.Errorf("transição %d: err = %v, want ErrInvalidTransition", i, err)
				}
			}
			var te *wagering.TransitionError
			if !errors.As(errs[0], &te) || te.From != tx.Status() {
				t.Errorf("TransitionError = %+v", te)
			}
			if _, err := wagering.Apply(wagering.ApplyInput{Transaction: tx, Wallet: f.wallet, LedgerEntryID: uuid.New(), Now: next}); !errors.Is(err, wagering.ErrInvalidTransition) {
				t.Errorf("Apply em estado terminal: err = %v", err)
			}
		})
	}
}

func TestPendingReferenceTransitions(t *testing.T) {
	f := newFixture(t, "100.00")
	tx := f.newTx(wagering.KindRefund, "refund-1", "10.00", withRef("bet-1"))

	if err := tx.RescheduleReference(t0, t0); !errors.Is(err, wagering.ErrInvalidTransition) {
		t.Errorf("reagendar fora de PENDING_REFERENCE: err = %v", err)
	}
	if err := tx.MarkPendingReference(t0.Add(time.Second), t0); err != nil {
		t.Fatal(err)
	}
	if tx.Status() != wagering.StatusPendingReference {
		t.Fatalf("status = %s", tx.Status())
	}
	evs := tx.PullEvents()
	if len(evs) != 1 || evs[0].EventType() != events.TypeWagerTransactionPendingReference {
		t.Errorf("events = %+v", evs)
	}
	if err := tx.MarkPendingReference(t0.Add(time.Second), t0); !errors.Is(err, wagering.ErrInvalidTransition) {
		t.Errorf("PENDING_REFERENCE -> PENDING_REFERENCE: err = %v", err)
	}
	if err := tx.RescheduleReference(t0.Add(3*time.Second), t0.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if tx.Attempts() != 1 {
		t.Errorf("attempts = %d", tx.Attempts())
	}
	if next, _ := tx.NextAttemptAt(); !next.Equal(t0.Add(3 * time.Second)) {
		t.Errorf("nextAttemptAt = %v", next)
	}

	// esgotou: rejeita com REFERENCE_NOT_FOUND
	if err := tx.MarkRejected(wagering.FailureReferenceNotFound, t0.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if tx.FailureCode() != wagering.FailureReferenceNotFound || tx.FailureCode().IsCorrectable() {
		t.Errorf("failureCode = %s", tx.FailureCode())
	}
	evs = tx.PullEvents()
	if len(evs) != 1 || evs[0].EventType() != events.TypeWagerTransactionRejected {
		t.Errorf("events = %+v", evs)
	}

	bet := f.newTx(wagering.KindBet, "bet-x", "1.00")
	if err := bet.MarkPendingReference(t0, t0); !errors.Is(err, domainerr.ErrValidation) {
		t.Errorf("BET sem referência não pode esperar referência: %v", err)
	}
}

func TestMarkRejected_InvalidCode(t *testing.T) {
	f := newFixture(t, "100.00")
	tx := f.newTx(wagering.KindBet, "bet-1", "1.00")
	for _, code := range []wagering.FailureCode{"", "WHATEVER", wagering.FailurePermanentError} {
		if err := tx.MarkRejected(code, t0); !errors.Is(err, domainerr.ErrValidation) {
			t.Errorf("code %q: err = %v", code, err)
		}
	}
	if tx.Status() != wagering.StatusPending {
		t.Error("status mudou")
	}
}

// ---------- regras dos tipos ----------

func TestBet(t *testing.T) {
	f := newFixture(t, "100.00")
	tx := f.newTx(wagering.KindBet, "bet-1", "25.00")
	out := f.apply(tx, nil)

	if out.Result != wagering.ResultProcessed || out.Entry == nil || out.Entry.Direction() != wallet.Debit {
		t.Fatalf("out = %+v", out)
	}
	if f.balance() != "75.00" || f.wallet.Version() != 2 {
		t.Errorf("wallet = %s v%d", f.balance(), f.wallet.Version())
	}
	if bal, _ := tx.BalanceAfter(); bal.Amount() != "75.00" {
		t.Errorf("balanceAfter = %v", bal)
	}
	if evs := tx.PullEvents(); len(evs) != 1 || evs[0].EventType() != events.TypeWagerTransactionProcessed {
		t.Errorf("tx events = %+v", evs)
	}
	if evs := f.wallet.PullEvents(); len(evs) != 1 || evs[0].EventType() != events.TypeWalletBalanceChanged {
		t.Errorf("wallet events = %+v", evs)
	}
}

// O cenário do teste obrigatório, em memória: as duas apostas de 80 em
// sequência sobre 100. (A disputa concorrente real é testada no banco.)
func TestTwoBetsOf80On100(t *testing.T) {
	f := newFixture(t, "100.00")
	first := f.newTx(wagering.KindBet, "bet-1", "80.00")
	second := f.newTx(wagering.KindBet, "bet-2", "80.00")

	r1, r2 := f.apply(first, nil), f.apply(second, nil)

	if r1.Result != wagering.ResultProcessed || r2.Result != wagering.ResultRejected {
		t.Fatalf("results = %s, %s", r1.Result, r2.Result)
	}
	if second.FailureCode() != wagering.FailureInsufficientFunds {
		t.Errorf("failureCode = %s", second.FailureCode())
	}
	if f.balance() != "20.00" || r2.Entry != nil {
		t.Errorf("saldo = %s", f.balance())
	}
	if evs := second.PullEvents(); len(evs) != 1 || evs[0].EventType() != events.TypeWagerTransactionRejected {
		t.Errorf("events = %+v", evs)
	}
}

func TestWin(t *testing.T) {
	f := newFixture(t, "100.00")
	bet := f.processed(wagering.KindBet, "bet-1", "10.00", nil)

	f.processed(wagering.KindWin, "win-1", "60.00", nil)
	if f.balance() != "150.00" {
		t.Errorf("saldo após WIN sem referência = %s", f.balance())
	}

	win := f.processed(wagering.KindWin, "win-2", "5.00", bet, withRef("bet-1"))
	if id, ok := win.ReferenceTransactionID(); !ok || id != bet.ID() {
		t.Errorf("referência resolvida = %v", id)
	}
	if f.balance() != "155.00" {
		t.Errorf("saldo = %s", f.balance())
	}
}

func TestLoss(t *testing.T) {
	f := newFixture(t, "100.00")
	tx := f.newTx(wagering.KindLoss, "loss-1", "0.00")
	out := f.apply(tx, nil)

	if out.Result != wagering.ResultProcessed || out.Entry != nil {
		t.Fatalf("LOSS não cria lançamento: %+v", out)
	}
	if f.balance() != "100.00" || f.wallet.Version() != 1 {
		t.Errorf("LOSS não altera saldo nem versão: %s v%d", f.balance(), f.wallet.Version())
	}
	if len(f.wallet.PullEvents()) != 0 {
		t.Error("LOSS não emite WalletBalanceChanged")
	}
	evs := tx.PullEvents()
	if len(evs) != 1 || evs[0].EventType() != events.TypeWagerTransactionProcessed {
		t.Errorf("LOSS emite WagerTransactionProcessed: %+v", evs)
	}
}

func TestRefund(t *testing.T) {
	f := newFixture(t, "100.00")
	bet := f.processed(wagering.KindBet, "bet-1", "25.00", nil)
	refund := f.processed(wagering.KindRefund, "refund-1", "25.00", bet, withRef("bet-1"))

	if f.balance() != "100.00" {
		t.Errorf("saldo = %s", f.balance())
	}
	if id, _ := refund.ReferenceTransactionID(); id != bet.ID() {
		t.Error("referência não resolvida")
	}
}

func TestRollback(t *testing.T) {
	t.Run("de BET devolve", func(t *testing.T) {
		f := newFixture(t, "100.00")
		bet := f.processed(wagering.KindBet, "bet-1", "25.00", nil)
		rb := f.newTx(wagering.KindRollback, "rb-1", "25.00", withRef("bet-1"))
		out := f.apply(rb, bet)
		if out.Result != wagering.ResultProcessed || out.Entry.Direction() != wallet.Credit || f.balance() != "100.00" {
			t.Errorf("out = %+v saldo = %s", out, f.balance())
		}
	})
	t.Run("de WIN retira", func(t *testing.T) {
		f := newFixture(t, "100.00")
		win := f.processed(wagering.KindWin, "win-1", "50.00", nil)
		rb := f.newTx(wagering.KindRollback, "rb-1", "50.00", withRef("win-1"))
		out := f.apply(rb, win)
		if out.Result != wagering.ResultProcessed || out.Entry.Direction() != wallet.Debit || f.balance() != "100.00" {
			t.Errorf("out = %+v saldo = %s", out, f.balance())
		}
	})
	t.Run("de REFUND retira", func(t *testing.T) {
		f := newFixture(t, "100.00")
		bet := f.processed(wagering.KindBet, "bet-1", "25.00", nil)
		refund := f.processed(wagering.KindRefund, "refund-1", "25.00", bet, withRef("bet-1"))
		rb := f.newTx(wagering.KindRollback, "rb-1", "25.00", withRef("refund-1"))
		out := f.apply(rb, refund)
		if out.Result != wagering.ResultProcessed || out.Entry.Direction() != wallet.Debit || f.balance() != "75.00" {
			t.Errorf("out = %+v saldo = %s", out, f.balance())
		}
	})
	t.Run("de WIN sem saldo tem código próprio", func(t *testing.T) {
		f := newFixture(t, "0.00")
		win := f.processed(wagering.KindWin, "win-1", "50.00", nil)
		f.processed(wagering.KindBet, "bet-1", "40.00", nil) // saldo cai para 10
		rb := f.newTx(wagering.KindRollback, "rb-1", "50.00", withRef("win-1"))
		out := f.apply(rb, win)
		if out.Result != wagering.ResultRejected || rb.FailureCode() != wagering.FailureReversalInsufficientFunds {
			t.Errorf("result = %s code = %s", out.Result, rb.FailureCode())
		}
		if rb.FailureCode() == wagering.FailureInsufficientFunds {
			t.Error("deve diferir do código de aposta sem saldo")
		}
		if f.balance() != "10.00" {
			t.Errorf("saldo = %s", f.balance())
		}
	})
}

// ---------- referências ----------

func TestReferenceValidation(t *testing.T) {
	type setup struct {
		ref  func(f *fixture) *wagering.WagerTransaction
		tx   func(f *fixture) *wagering.WagerTransaction
		want wagering.Result
		code wagering.FailureCode
	}
	bet := func(f *fixture) *wagering.WagerTransaction {
		return f.processed(wagering.KindBet, "bet-1", "25.00", nil)
	}
	tests := map[string]setup{
		"referência ainda não chegou": {
			ref: func(f *fixture) *wagering.WagerTransaction { return nil },
			tx: func(f *fixture) *wagering.WagerTransaction {
				return f.newTx(wagering.KindRefund, "r", "25.00", withRef("bet-1"))
			},
			want: wagering.ResultAwaitingReference,
		},
		"referência ainda pendente": {
			ref: func(f *fixture) *wagering.WagerTransaction { return f.newTx(wagering.KindBet, "bet-1", "25.00") },
			tx: func(f *fixture) *wagering.WagerTransaction {
				return f.newTx(wagering.KindRefund, "r", "25.00", withRef("bet-1"))
			},
			want: wagering.ResultAwaitingReference,
		},
		"referência rejeitada": {
			ref: func(f *fixture) *wagering.WagerTransaction {
				b := f.newTx(wagering.KindBet, "bet-1", "999.00")
				f.apply(b, nil)
				return b
			},
			tx: func(f *fixture) *wagering.WagerTransaction {
				return f.newTx(wagering.KindRefund, "r", "999.00", withRef("bet-1"))
			},
			want: wagering.ResultRejected, code: wagering.FailureReferenceNotProcessed,
		},
		"valor diferente": {
			ref: bet,
			tx: func(f *fixture) *wagering.WagerTransaction {
				return f.newTx(wagering.KindRefund, "r", "10.00", withRef("bet-1"))
			},
			want: wagering.ResultRejected, code: wagering.FailureAmountMismatch,
		},
		"rodada diferente": {
			ref: bet,
			tx: func(f *fixture) *wagering.WagerTransaction {
				return f.newTx(wagering.KindRefund, "r", "25.00", withRef("bet-1"), withRound("round-2"))
			},
			want: wagering.ResultRejected, code: wagering.FailureReferenceMismatch,
		},
		"provedor diferente": {
			ref: bet,
			tx: func(f *fixture) *wagering.WagerTransaction {
				return f.newTx(wagering.KindRefund, "r", "25.00", withRef("bet-1"), withProvider("provider-b"))
			},
			want: wagering.ResultRejected, code: wagering.FailureReferenceMismatch,
		},
		"REFUND de WIN não é permitido": {
			ref: func(f *fixture) *wagering.WagerTransaction {
				return f.processed(wagering.KindWin, "bet-1", "25.00", nil)
			},
			tx: func(f *fixture) *wagering.WagerTransaction {
				return f.newTx(wagering.KindRefund, "r", "25.00", withRef("bet-1"))
			},
			want: wagering.ResultRejected, code: wagering.FailureReferenceKindInvalid,
		},
		"ROLLBACK de ROLLBACK não é permitido": {
			ref: func(f *fixture) *wagering.WagerTransaction {
				b := f.processed(wagering.KindBet, "bet-0", "25.00", nil)
				return f.processed(wagering.KindRollback, "bet-1", "25.00", b, withRef("bet-0"))
			},
			tx: func(f *fixture) *wagering.WagerTransaction {
				return f.newTx(wagering.KindRollback, "r", "25.00", withRef("bet-1"))
			},
			want: wagering.ResultRejected, code: wagering.FailureReferenceKindInvalid,
		},
		"WIN referenciando WIN": {
			ref: func(f *fixture) *wagering.WagerTransaction {
				return f.processed(wagering.KindWin, "bet-1", "25.00", nil)
			},
			tx: func(f *fixture) *wagering.WagerTransaction {
				return f.newTx(wagering.KindWin, "r", "5.00", withRef("bet-1"))
			},
			want: wagering.ResultRejected, code: wagering.FailureReferenceKindInvalid,
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t, "100.00")
			ref := tt.ref(f)
			tx := tt.tx(f)
			before := f.balance()
			out := f.apply(tx, ref)
			if out.Result != tt.want || tx.FailureCode() != tt.code {
				t.Fatalf("result = %s code = %q; want %s %q", out.Result, tx.FailureCode(), tt.want, tt.code)
			}
			if f.balance() != before {
				t.Errorf("saldo mudou: %s -> %s", before, f.balance())
			}
			if tt.want == wagering.ResultAwaitingReference && tx.Status() != wagering.StatusPending {
				t.Errorf("aguardando referência não muda o estado: %s", tx.Status())
			}
		})
	}
}

func TestReferenceInAnotherWalletOrPlayer(t *testing.T) {
	f := newFixture(t, "100.00")
	other := newFixture(t, "100.00")
	foreignBet := other.processed(wagering.KindBet, "bet-1", "25.00", nil)

	tx := f.newTx(wagering.KindRefund, "r", "25.00", withRef("bet-1"))
	if out := f.apply(tx, foreignBet); out.Result != wagering.ResultRejected || tx.FailureCode() != wagering.FailureReferenceMismatch {
		t.Errorf("result = %s code = %s", out.Result, tx.FailureCode())
	}
}

// ---------- carteira ----------

func TestApply_WalletChecks(t *testing.T) {
	f := newFixture(t, "100.00")

	t.Run("carteira inexistente", func(t *testing.T) {
		tx := f.newTx(wagering.KindBet, "b1", "1.00")
		out, err := wagering.Apply(wagering.ApplyInput{Transaction: tx, Wallet: nil, LedgerEntryID: uuid.New(), Now: t0})
		if err != nil || out.Result != wagering.ResultRejected || tx.FailureCode() != wagering.FailureWalletNotFound {
			t.Errorf("out = %+v err = %v code = %s", out, err, tx.FailureCode())
		}
		if !tx.FailureCode().IsCorrectable() {
			t.Error("WALLET_NOT_FOUND é corrigível")
		}
	})
	t.Run("jogador de outra carteira", func(t *testing.T) {
		p := f.params(wagering.KindBet, "b2", "1.00")
		p.PlayerID = uuid.New()
		tx, _ := wagering.NewExternal(p)
		if out := f.apply(tx, nil); tx.FailureCode() != wagering.FailureWalletPlayerMismatch {
			t.Errorf("out = %+v code = %s", out, tx.FailureCode())
		}
	})
	t.Run("moeda diferente da carteira", func(t *testing.T) {
		p := f.params(wagering.KindBet, "b3", "1.00")
		p.Amount, _ = money.Parse("1.00", money.USD)
		tx, _ := wagering.NewExternal(p)
		if out := f.apply(tx, nil); tx.FailureCode() != wagering.FailureCurrencyMismatch {
			t.Errorf("out = %+v code = %s", out, tx.FailureCode())
		}
	})
	if f.balance() != "100.00" || f.wallet.Version() != 1 {
		t.Error("rejeições não podem alterar a carteira")
	}
}

// ---------- reidratação ----------

func TestRehydrate(t *testing.T) {
	bal := brl(t, "975.00")
	ext := &wagering.External{
		ProviderID: "provider-a", ExternalTransactionID: "tx-1", IdempotencyKey: "provider-a:tx-1",
		PayloadHash: "h", RoundID: "r", GameID: "g",
	}
	p := wagering.RehydrateParams{
		ID: uuid.New(), Origin: wagering.OriginExternal, Kind: wagering.KindBet,
		Status: wagering.StatusProcessed, WalletID: uuid.New(), PlayerID: uuid.New(),
		Amount: brl(t, "25.00"), External: ext, BalanceAfter: &bal,
		CreatedAt: t0, UpdatedAt: t0,
	}
	tx, err := wagering.Rehydrate(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(tx.PullEvents()) != 0 {
		t.Error("reidratação não emite eventos")
	}
	if got, _ := tx.BalanceAfter(); got.Amount() != "975.00" {
		t.Errorf("balanceAfter = %v", got)
	}
	ext.ProviderID = "alterado"
	if tx.External().ProviderID != "provider-a" {
		t.Error("Rehydrate deve copiar os metadados externos")
	}

	bad := map[string]func(*wagering.RehydrateParams){
		"PROCESSED sem saldo":          func(p *wagering.RehydrateParams) { p.BalanceAfter = nil },
		"REJECTED sem código":          func(p *wagering.RehydrateParams) { p.Status = wagering.StatusRejected },
		"PENDING_REFERENCE sem agenda": func(p *wagering.RehydrateParams) { p.Status = wagering.StatusPendingReference },
		"INTERNAL com externo":         func(p *wagering.RehydrateParams) { p.Origin = wagering.OriginInternal },
		"EXTERNAL sem externo":         func(p *wagering.RehydrateParams) { p.External = nil },
		"EXTERNAL do tipo OPENING":     func(p *wagering.RehydrateParams) { p.Kind = wagering.KindOpening },
		"status desconhecido":          func(p *wagering.RehydrateParams) { p.Status = "DONE" },
		"tentativas negativas":         func(p *wagering.RehydrateParams) { p.Attempts = -1 },
	}
	for name, mutate := range bad {
		t.Run(name, func(t *testing.T) {
			q := p
			q.External = &wagering.External{
				ProviderID: "provider-a", ExternalTransactionID: "tx-1", IdempotencyKey: "k",
				PayloadHash: "h", RoundID: "r", GameID: "g",
			}
			mutate(&q)
			if _, err := wagering.Rehydrate(q); !errors.Is(err, domainerr.ErrValidation) {
				t.Errorf("err = %v, want ErrValidation", err)
			}
		})
	}
}

// ---------- política de retentativas ----------

func TestReferenceRetryPolicy(t *testing.T) {
	p := wagering.ReferenceRetryPolicy{BaseDelay: time.Second, MaxDelay: 10 * time.Second, MaxAttempts: 5}
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
	want := []time.Duration{1, 2, 4, 8, 10, 10}
	for i, w := range want {
		if got := p.Delay(i); got != w*time.Second {
			t.Errorf("Delay(%d) = %v, want %v", i, got, w*time.Second)
		}
	}
	if p.Delay(10_000) != 10*time.Second {
		t.Error("atraso deve ficar no teto mesmo com muitas tentativas")
	}
	if p.Exhausted(4) || !p.Exhausted(5) {
		t.Error("Exhausted incorreto")
	}
	if err := wagering.DefaultReferenceRetryPolicy.Validate(); err != nil {
		t.Errorf("política padrão inválida: %v", err)
	}
	if err := (wagering.ReferenceRetryPolicy{}).Validate(); err == nil {
		t.Error("política vazia deveria ser inválida")
	}
}
