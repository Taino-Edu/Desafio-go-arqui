// Package wallet contém a carteira (raiz do agregado financeiro) e o
// lançamento de ledger.
package wallet

import (
	"errors"
	"time"

	"github.com/google/uuid"

	"github.com/Taino-Edu/Desafio-go-arqui/internal/domain/domainerr"
	"github.com/Taino-Edu/Desafio-go-arqui/internal/domain/events"
	"github.com/Taino-Edu/Desafio-go-arqui/internal/domain/money"
)

// ErrInsufficientFunds indica que um débito deixaria o saldo negativo.
var ErrInsufficientFunds = errors.New("wallet: insufficient funds")

// InitialVersion é a versão de toda carteira recém-aberta.
const InitialVersion int64 = 1

// Wallet é a raiz do agregado financeiro. O saldo só muda por Debit/Credit,
// e cada mudança produz o LedgerEntry correspondente e um evento
// WalletBalanceChanged. A versão sobe apenas quando o saldo muda.
type Wallet struct {
	id        uuid.UUID
	playerID  uuid.UUID
	currency  money.Currency
	balance   money.Money
	version   int64
	createdAt time.Time
	updatedAt time.Time

	events []events.Event // eventos ainda não gravados na outbox
}

// OpenParams são os dados de abertura de uma carteira.
type OpenParams struct {
	ID             uuid.UUID
	PlayerID       uuid.UUID
	InitialBalance money.Money // zero é permitido
	// OpeningTransactionID e LedgerEntryID só são usados quando o saldo
	// inicial é positivo (é quando existe crédito de abertura).
	OpeningTransactionID uuid.UUID
	LedgerEntryID        uuid.UUID
	Now                  time.Time
}

// Open cria uma carteira nova com versão 1.
//
// Com saldo inicial positivo, devolve também o lançamento de crédito de
// abertura e registra WalletBalanceChanged. Com saldo zero, não há lançamento
// nem evento (o LedgerEntry devolvido é nil).
func Open(p OpenParams) (*Wallet, *LedgerEntry, error) {
	switch {
	case p.ID == uuid.Nil:
		return nil, nil, domainerr.Field("id", "required")
	case p.PlayerID == uuid.Nil:
		return nil, nil, domainerr.Field("playerId", "required")
	case !p.InitialBalance.IsValid():
		return nil, nil, domainerr.Field("initialBalance", "required")
	case p.InitialBalance.IsNegative():
		return nil, nil, domainerr.Field("initialBalance", "must not be negative")
	case p.Now.IsZero():
		return nil, nil, domainerr.Field("now", "required")
	}

	now := p.Now.UTC()
	w := &Wallet{
		id: p.ID, playerID: p.PlayerID,
		currency: p.InitialBalance.Currency(), balance: p.InitialBalance,
		version: InitialVersion, createdAt: now, updatedAt: now,
	}
	if p.InitialBalance.IsZero() {
		return w, nil, nil
	}

	zero, _ := money.Zero(w.currency)
	entry, err := NewLedgerEntry(LedgerEntryParams{
		ID: p.LedgerEntryID, WalletID: w.id, TransactionID: p.OpeningTransactionID,
		WalletVersion: InitialVersion, Direction: Credit, Amount: p.InitialBalance,
		BalanceBefore: zero, BalanceAfter: p.InitialBalance, CreatedAt: now,
	})
	if err != nil {
		return nil, nil, err
	}
	w.record(entry)
	return w, &entry, nil
}

// RehydrateParams são os dados lidos do banco.
type RehydrateParams struct {
	ID        uuid.UUID
	PlayerID  uuid.UUID
	Balance   money.Money
	Version   int64
	CreatedAt time.Time
	UpdatedAt time.Time
}

// Rehydrate reconstrói uma carteira existente. Valida os invariantes, mas não
// reaplica movimentações, não altera a versão e não emite eventos.
func Rehydrate(p RehydrateParams) (*Wallet, error) {
	switch {
	case p.ID == uuid.Nil:
		return nil, domainerr.Field("id", "required")
	case p.PlayerID == uuid.Nil:
		return nil, domainerr.Field("playerId", "required")
	case !p.Balance.IsValid():
		return nil, domainerr.Field("balance", "required")
	case p.Balance.IsNegative():
		return nil, domainerr.Field("balance", "must not be negative")
	case p.Version < InitialVersion:
		return nil, domainerr.Field("version", "must be >= 1")
	case p.CreatedAt.IsZero() || p.UpdatedAt.IsZero():
		return nil, domainerr.Field("timestamps", "required")
	case p.UpdatedAt.Before(p.CreatedAt):
		return nil, domainerr.Field("updatedAt", "must not be before createdAt")
	}
	return &Wallet{
		id: p.ID, playerID: p.PlayerID,
		currency: p.Balance.Currency(), balance: p.Balance, version: p.Version,
		createdAt: p.CreatedAt.UTC(), updatedAt: p.UpdatedAt.UTC(),
	}, nil
}

// Debit retira amount do saldo. Devolve ErrInsufficientFunds se o saldo
// ficaria negativo; nesse caso a carteira não muda.
func (w *Wallet) Debit(entryID, transactionID uuid.UUID, amount money.Money, now time.Time) (LedgerEntry, error) {
	return w.move(Debit, entryID, transactionID, amount, now)
}

// Credit adiciona amount ao saldo.
func (w *Wallet) Credit(entryID, transactionID uuid.UUID, amount money.Money, now time.Time) (LedgerEntry, error) {
	return w.move(Credit, entryID, transactionID, amount, now)
}

// Move aplica um lançamento na direção informada.
func (w *Wallet) Move(dir Direction, entryID, transactionID uuid.UUID, amount money.Money, now time.Time) (LedgerEntry, error) {
	return w.move(dir, entryID, transactionID, amount, now)
}

func (w *Wallet) move(dir Direction, entryID, txID uuid.UUID, amount money.Money, now time.Time) (LedgerEntry, error) {
	if w == nil || w.id == uuid.Nil {
		return LedgerEntry{}, domainerr.ErrUninitialized
	}
	if !amount.IsPositive() {
		return LedgerEntry{}, domainerr.Field("amount", "must be greater than zero")
	}
	if amount.Currency() != w.currency {
		return LedgerEntry{}, money.ErrCurrencyMismatch
	}
	if now.IsZero() {
		return LedgerEntry{}, domainerr.Field("now", "required")
	}

	var after money.Money
	var err error
	if dir == Debit {
		after, err = w.balance.Sub(amount)
		if err == nil && after.IsNegative() {
			return LedgerEntry{}, ErrInsufficientFunds
		}
	} else {
		after, err = w.balance.Add(amount)
	}
	if err != nil {
		return LedgerEntry{}, err
	}

	now = now.UTC()
	entry, err := NewLedgerEntry(LedgerEntryParams{
		ID: entryID, WalletID: w.id, TransactionID: txID, WalletVersion: w.version + 1, Direction: dir,
		Amount: amount, BalanceBefore: w.balance, BalanceAfter: after, CreatedAt: now,
	})
	if err != nil {
		return LedgerEntry{}, err
	}

	// só altera o estado depois de tudo validado
	w.balance = after
	w.version++
	if now.After(w.updatedAt) {
		w.updatedAt = now
	}
	w.record(entry)
	return entry, nil
}

func (w *Wallet) record(e LedgerEntry) {
	w.events = append(w.events, events.NewWalletBalanceChanged(
		w.id, e.TransactionID(), string(e.Direction()),
		e.Amount(), e.BalanceBefore(), e.BalanceAfter(), w.version, e.CreatedAt(),
	))
}

// PullEvents devolve os eventos pendentes e limpa a lista. A camada de
// aplicação grava esses eventos na outbox na mesma transação SQL.
func (w *Wallet) PullEvents() []events.Event {
	evs := w.events
	w.events = nil
	return evs
}

func (w *Wallet) ID() uuid.UUID            { return w.id }
func (w *Wallet) PlayerID() uuid.UUID      { return w.playerID }
func (w *Wallet) Currency() money.Currency { return w.currency }
func (w *Wallet) Balance() money.Money     { return w.balance }
func (w *Wallet) Version() int64           { return w.version }
func (w *Wallet) CreatedAt() time.Time     { return w.createdAt }
func (w *Wallet) UpdatedAt() time.Time     { return w.updatedAt }
