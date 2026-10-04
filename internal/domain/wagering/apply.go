package wagering

import (
	"errors"
	"time"

	"github.com/google/uuid"

	"github.com/Taino-Edu/Desafio-go-arqui/internal/domain/wallet"
)

// Result é o desfecho de Apply.
type Result string

const (
	ResultProcessed Result = "PROCESSED"
	ResultRejected  Result = "REJECTED"
	// ResultAwaitingReference: a referência ainda não existe ou ainda não foi
	// concluída. A transação NÃO muda de estado; quem chama decide entre
	// MarkPendingReference, RescheduleReference ou expirar (MarkRejected
	// com REFERENCE_NOT_FOUND), conforme a política de retentativas.
	ResultAwaitingReference Result = "AWAITING_REFERENCE"
)

// ApplyInput reúne tudo o que Apply precisa, já carregado do banco.
type ApplyInput struct {
	Transaction *WagerTransaction
	Wallet      *wallet.Wallet    // nil se a carteira não existe
	Reference   *WagerTransaction // nil se não informada ou ainda não recebida
	// LedgerEntryID é usado se houver movimentação.
	LedgerEntryID uuid.UUID
	Now           time.Time
}

// Outcome é o resultado de Apply. Entry é nil quando não há movimentação
// (LOSS, rejeição, espera).
type Outcome struct {
	Result Result
	Entry  *wallet.LedgerEntry
}

// Apply executa as regras de negócio de uma operação externa sobre a carteira:
// valida carteira, moeda e referência, movimenta o saldo e faz a transição de
// estado. É uma função pura sobre objetos em memória; a camada de aplicação
// persiste o resultado na mesma transação SQL.
//
// Um erro devolvido indica falha inesperada (entrada inválida ou estado
// incoerente), não rejeição de negócio: rejeições voltam como ResultRejected.
func Apply(in ApplyInput) (Outcome, error) {
	tx, w := in.Transaction, in.Wallet
	if tx == nil {
		return Outcome{}, errors.New("wagering: nil transaction")
	}
	if tx.origin != OriginExternal {
		return Outcome{}, errors.New("wagering: Apply only handles external operations")
	}
	if tx.status != StatusPending && tx.status != StatusPendingReference {
		return Outcome{}, &TransitionError{From: tx.status, To: StatusProcessed}
	}

	reject := func(code FailureCode) (Outcome, error) {
		if err := tx.MarkRejected(code, in.Now); err != nil {
			return Outcome{}, err
		}
		return Outcome{Result: ResultRejected}, nil
	}

	// 1. carteira
	if w == nil {
		return reject(FailureWalletNotFound)
	}
	if w.ID() != tx.walletID || w.PlayerID() != tx.playerID {
		return reject(FailureWalletPlayerMismatch)
	}
	if w.Currency() != tx.amount.Currency() {
		return reject(FailureCurrencyMismatch)
	}

	// 2. referência
	var refID *uuid.UUID
	var refKind Kind
	if tx.external.HasReference() {
		if in.Reference == nil {
			return Outcome{Result: ResultAwaitingReference}, nil
		}
		switch decision, code := evaluateReference(tx, in.Reference); decision {
		case refWait:
			return Outcome{Result: ResultAwaitingReference}, nil
		case refReject:
			return reject(code)
		}
		id := in.Reference.id
		refID, refKind = &id, in.Reference.kind
	}

	// 3. movimentação
	dir, moves := movement(tx.kind, refKind)
	if !moves { // LOSS
		if err := tx.MarkProcessed(w.Balance(), refID, in.Now); err != nil {
			return Outcome{}, err
		}
		return Outcome{Result: ResultProcessed}, nil
	}

	entry, err := w.Move(dir, in.LedgerEntryID, tx.id, tx.amount, in.Now)
	if errors.Is(err, wallet.ErrInsufficientFunds) {
		if tx.kind == KindRollback {
			// código diferente da aposta sem saldo, como exige o desafio
			return reject(FailureReversalInsufficientFunds)
		}
		return reject(FailureInsufficientFunds)
	}
	if err != nil {
		return Outcome{}, err
	}
	if err := tx.MarkProcessed(w.Balance(), refID, in.Now); err != nil {
		return Outcome{}, err
	}
	return Outcome{Result: ResultProcessed, Entry: &entry}, nil
}

// movement devolve a direção do lançamento de cada tipo.
// ROLLBACK faz o contrário da operação referenciada.
func movement(k Kind, refKind Kind) (wallet.Direction, bool) {
	switch k {
	case KindBet:
		return wallet.Debit, true
	case KindWin, KindRefund, KindOpening:
		return wallet.Credit, true
	case KindRollback:
		original, _ := movement(refKind, "")
		return original.Opposite(), true
	default: // LOSS
		return "", false
	}
}

type refDecision int

const (
	refApply refDecision = iota
	refWait
	refReject
)

// allowedReferences: que tipo pode apontar para qual.
var allowedReferences = map[Kind][]Kind{
	KindWin:      {KindBet},
	KindRefund:   {KindBet},
	KindRollback: {KindBet, KindWin, KindRefund},
}

// evaluateReference confere se a referência é compatível. Divergências
// estáticas (tipo, jogador, carteira, moeda, rodada, valor) são verificadas
// antes do estado da referência, porque não mudam com o tempo.
func evaluateReference(tx, ref *WagerTransaction) (refDecision, FailureCode) {
	if ref.origin != OriginExternal || ref.external == nil {
		return refReject, FailureReferenceKindInvalid
	}
	kindOK := false
	for _, k := range allowedReferences[tx.kind] {
		if ref.kind == k {
			kindOK = true
		}
	}
	if !kindOK {
		return refReject, FailureReferenceKindInvalid
	}

	if ref.external.ProviderID != tx.external.ProviderID ||
		ref.external.ExternalTransactionID != tx.external.ReferenceExternalTransactionID ||
		ref.playerID != tx.playerID ||
		ref.walletID != tx.walletID ||
		ref.amount.Currency() != tx.amount.Currency() ||
		ref.external.RoundID != tx.external.RoundID {
		return refReject, FailureReferenceMismatch
	}
	if tx.kind.requiresReference() && !ref.amount.Equal(tx.amount) {
		return refReject, FailureAmountMismatch // reversões parciais não são suportadas
	}

	switch ref.status {
	case StatusProcessed:
		return refApply, ""
	case StatusPending, StatusPendingReference:
		return refWait, ""
	default: // REJECTED, FAILED
		return refReject, FailureReferenceNotProcessed
	}
}
