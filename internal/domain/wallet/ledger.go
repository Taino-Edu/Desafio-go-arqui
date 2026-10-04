package wallet

import (
	"time"

	"github.com/google/uuid"

	"github.com/Taino-Edu/Desafio-go-arqui/internal/domain/domainerr"
	"github.com/Taino-Edu/Desafio-go-arqui/internal/domain/money"
)

// Direction é o sentido de um lançamento.
type Direction string

const (
	Debit  Direction = "DEBIT"
	Credit Direction = "CREDIT"
)

// Opposite devolve a direção contrária (usada pelo ROLLBACK).
func (d Direction) Opposite() Direction {
	if d == Debit {
		return Credit
	}
	return Debit
}

func (d Direction) valid() bool { return d == Debit || d == Credit }

// LedgerEntry é um lançamento imutável do livro-razão. Não há setters: depois
// de construído, nenhum campo muda. Correções exigem um novo lançamento.
type LedgerEntry struct {
	id            uuid.UUID
	walletID      uuid.UUID
	transactionID uuid.UUID
	direction     Direction
	amount        money.Money
	balanceBefore money.Money
	balanceAfter  money.Money
	createdAt     time.Time
}

// LedgerEntryParams reúne os campos de um lançamento.
type LedgerEntryParams struct {
	ID            uuid.UUID
	WalletID      uuid.UUID
	TransactionID uuid.UUID
	Direction     Direction
	Amount        money.Money
	BalanceBefore money.Money
	BalanceAfter  money.Money
	CreatedAt     time.Time
}

// NewLedgerEntry valida e cria um lançamento. Como um lançamento não tem
// transições nem emite eventos, o mesmo construtor serve para criação e para
// reidratação a partir do banco.
//
// Invariante: balanceAfter = balanceBefore + amount (CREDIT)
//
//	ou balanceAfter = balanceBefore - amount (DEBIT).
func NewLedgerEntry(p LedgerEntryParams) (LedgerEntry, error) {
	switch {
	case p.ID == uuid.Nil:
		return LedgerEntry{}, domainerr.Field("id", "required")
	case p.WalletID == uuid.Nil:
		return LedgerEntry{}, domainerr.Field("walletId", "required")
	case p.TransactionID == uuid.Nil:
		return LedgerEntry{}, domainerr.Field("transactionId", "required")
	case !p.Direction.valid():
		return LedgerEntry{}, domainerr.Field("direction", "must be DEBIT or CREDIT")
	case !p.Amount.IsPositive():
		return LedgerEntry{}, domainerr.Field("amount", "must be greater than zero")
	case !p.BalanceBefore.IsValid() || p.BalanceBefore.IsNegative():
		return LedgerEntry{}, domainerr.Field("balanceBefore", "must be zero or positive")
	case !p.BalanceAfter.IsValid() || p.BalanceAfter.IsNegative():
		return LedgerEntry{}, domainerr.Field("balanceAfter", "must be zero or positive")
	case p.CreatedAt.IsZero():
		return LedgerEntry{}, domainerr.Field("createdAt", "required")
	}

	var expected money.Money
	var err error
	if p.Direction == Credit {
		expected, err = p.BalanceBefore.Add(p.Amount)
	} else {
		expected, err = p.BalanceBefore.Sub(p.Amount)
	}
	if err != nil { // moedas diferentes ou overflow
		return LedgerEntry{}, err
	}
	if !expected.Equal(p.BalanceAfter) {
		return LedgerEntry{}, domainerr.Field("balanceAfter", "must equal balanceBefore ± amount")
	}

	return LedgerEntry{
		id: p.ID, walletID: p.WalletID, transactionID: p.TransactionID,
		direction: p.Direction, amount: p.Amount,
		balanceBefore: p.BalanceBefore, balanceAfter: p.BalanceAfter,
		createdAt: p.CreatedAt.UTC(),
	}, nil
}

func (e LedgerEntry) ID() uuid.UUID              { return e.id }
func (e LedgerEntry) WalletID() uuid.UUID        { return e.walletID }
func (e LedgerEntry) TransactionID() uuid.UUID   { return e.transactionID }
func (e LedgerEntry) Direction() Direction       { return e.direction }
func (e LedgerEntry) Amount() money.Money        { return e.amount }
func (e LedgerEntry) BalanceBefore() money.Money { return e.balanceBefore }
func (e LedgerEntry) BalanceAfter() money.Money  { return e.balanceAfter }
func (e LedgerEntry) CreatedAt() time.Time       { return e.createdAt }
