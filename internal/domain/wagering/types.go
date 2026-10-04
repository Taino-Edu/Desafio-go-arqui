// Package wagering contém a transação de aposta (WagerTransaction), sua
// máquina de estados e as regras de aplicação dos cinco tipos externos.
package wagering

import (
	"fmt"

	"github.com/Taino-Edu/Desafio-go-arqui/internal/domain/domainerr"
)

// Kind é o tipo de operação.
type Kind string

const (
	KindOpening  Kind = "OPENING" // interno: crédito de abertura de carteira
	KindBet      Kind = "BET"
	KindWin      Kind = "WIN"
	KindLoss     Kind = "LOSS"
	KindRefund   Kind = "REFUND"
	KindRollback Kind = "ROLLBACK"
)

// ParseExternalKind valida um tipo recebido por HTTP ou SQS. OPENING é
// reservado à abertura interna e é rejeitado aqui.
func ParseExternalKind(s string) (Kind, error) {
	switch k := Kind(s); k {
	case KindBet, KindWin, KindLoss, KindRefund, KindRollback:
		return k, nil
	case KindOpening:
		return "", domainerr.Field("kind", "OPENING is internal and cannot be submitted")
	default:
		return "", domainerr.Field("kind", fmt.Sprintf("unknown kind %q", s))
	}
}

func (k Kind) valid() bool {
	switch k {
	case KindOpening, KindBet, KindWin, KindLoss, KindRefund, KindRollback:
		return true
	}
	return false
}

// requiresReference informa se a referência é obrigatória.
func (k Kind) requiresReference() bool { return k == KindRefund || k == KindRollback }

// acceptsReference informa se a referência é permitida.
func (k Kind) acceptsReference() bool { return k.requiresReference() || k == KindWin }

// Origin distingue operações internas (abertura) de externas (provedores).
type Origin string

const (
	OriginInternal Origin = "INTERNAL"
	OriginExternal Origin = "EXTERNAL"
)

// Status é o estado da transação. Veja a máquina de estados em transitions.
type Status string

const (
	StatusPending          Status = "PENDING"
	StatusPendingReference Status = "PENDING_REFERENCE"
	StatusProcessed        Status = "PROCESSED"
	StatusRejected         Status = "REJECTED"
	StatusFailed           Status = "FAILED"
)

// IsTerminal informa se o estado é final.
func (s Status) IsTerminal() bool {
	return s == StatusProcessed || s == StatusRejected || s == StatusFailed
}

// transitions é a máquina de estados completa. Qualquer par ausente é proibido.
//
//	PENDING ──────────► PROCESSED | REJECTED | FAILED | PENDING_REFERENCE
//	PENDING_REFERENCE ─► PROCESSED | REJECTED | FAILED
//	PROCESSED, REJECTED, FAILED: terminais, sem saída
var transitions = map[Status][]Status{
	StatusPending:          {StatusProcessed, StatusRejected, StatusFailed, StatusPendingReference},
	StatusPendingReference: {StatusProcessed, StatusRejected, StatusFailed},
}

func canTransition(from, to Status) bool {
	for _, s := range transitions[from] {
		if s == to {
			return true
		}
	}
	return false
}

func (s Status) valid() bool {
	switch s {
	case StatusPending, StatusPendingReference, StatusProcessed, StatusRejected, StatusFailed:
		return true
	}
	return false
}

// FailureCode é um código estável de rejeição ou falha, exposto ao provedor.
type FailureCode string

const (
	// Resultados definitivos: reenviar a mesma operação não muda nada.
	FailureInsufficientFunds         FailureCode = "INSUFFICIENT_FUNDS"
	FailureReversalInsufficientFunds FailureCode = "REVERSAL_INSUFFICIENT_FUNDS"
	FailureReferenceNotFound         FailureCode = "REFERENCE_NOT_FOUND"
	FailureReferenceNotProcessed     FailureCode = "REFERENCE_NOT_PROCESSED"
	FailureAlreadyReversed           FailureCode = "ALREADY_REVERSED"

	// Entradas corrigíveis: o provedor enviou dados que não batem; uma nova
	// operação (com outro externalTransactionId) e dados corretos pode passar.
	FailureWalletNotFound       FailureCode = "WALLET_NOT_FOUND"
	FailureWalletPlayerMismatch FailureCode = "WALLET_PLAYER_MISMATCH"
	FailureCurrencyMismatch     FailureCode = "CURRENCY_MISMATCH"
	FailureReferenceMismatch    FailureCode = "REFERENCE_MISMATCH"
	FailureReferenceKindInvalid FailureCode = "REFERENCE_KIND_INVALID"
	FailureAmountMismatch       FailureCode = "AMOUNT_MISMATCH"

	// Falha permanente de infraestrutura (estado FAILED), para auditoria.
	FailurePermanentError FailureCode = "PERMANENT_PROCESSING_ERROR"
)

var correctable = map[FailureCode]bool{
	FailureWalletNotFound:       true,
	FailureWalletPlayerMismatch: true,
	FailureCurrencyMismatch:     true,
	FailureReferenceMismatch:    true,
	FailureReferenceKindInvalid: true,
	FailureAmountMismatch:       true,
}

var knownFailures = map[FailureCode]bool{
	FailureInsufficientFunds: true, FailureReversalInsufficientFunds: true,
	FailureReferenceNotFound: true, FailureReferenceNotProcessed: true,
	FailureAlreadyReversed: true, FailurePermanentError: true,
}

func init() {
	for c := range correctable {
		knownFailures[c] = true
	}
}

// IsCorrectable informa se a rejeição decorre de entrada corrigível pelo
// provedor (true) ou se é um resultado definitivo (false).
func (c FailureCode) IsCorrectable() bool { return correctable[c] }

func (c FailureCode) valid() bool { return knownFailures[c] }
