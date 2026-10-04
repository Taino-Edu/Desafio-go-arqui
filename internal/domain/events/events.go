// Package events define os eventos de integração do domínio.
//
// Cada evento é um tipo concreto cujo construtor fixa o tipo e a versão: quem
// cria o evento não escolhe esses valores. Os campos são snapshots imutáveis
// tirados no momento da mudança. Valores monetários serializam como strings
// decimais (via money.Money) e instantes são UTC.
//
// O pacote não importa os pacotes de entidade, para evitar ciclos; por isso
// tipo de operação, direção e código de falha aparecem como strings.
package events

import (
	"time"

	"github.com/google/uuid"

	"github.com/Taino-Edu/Desafio-go-arqui/internal/domain/money"
)

// Tipos de evento.
const (
	TypeWagerTransactionProcessed        = "WagerTransactionProcessed"
	TypeWagerTransactionRejected         = "WagerTransactionRejected"
	TypeWalletBalanceChanged             = "WalletBalanceChanged"
	TypeWagerTransactionPendingReference = "WagerTransactionPendingReference"
)

// Event é implementado por todo evento de domínio.
type Event interface {
	EventType() string
	EventVersion() int
	AggregateID() uuid.UUID
	OccurredAt() time.Time
}

// base guarda os metadados que não fazem parte do payload JSON.
type base struct {
	eventType  string
	version    int
	aggregate  uuid.UUID
	occurredAt time.Time
}

func (b base) EventType() string      { return b.eventType }
func (b base) EventVersion() int      { return b.version }
func (b base) AggregateID() uuid.UUID { return b.aggregate }
func (b base) OccurredAt() time.Time  { return b.occurredAt }
func newBase(t string, agg uuid.UUID, at time.Time) base {
	return base{eventType: t, version: 1, aggregate: agg, occurredAt: at.UTC()}
}

// External carrega os metadados de uma operação vinda de um provedor.
// É nil em operações internas (OPENING).
type External struct {
	ProviderID                     string `json:"providerId"`
	ExternalTransactionID          string `json:"externalTransactionId"`
	RoundID                        string `json:"roundId"`
	GameID                         string `json:"gameId"`
	ReferenceExternalTransactionID string `json:"referenceExternalTransactionId,omitempty"`
}

// clone copia o ponteiro para que o evento seja um snapshot independente.
func (e *External) clone() *External {
	if e == nil {
		return nil
	}
	c := *e
	return &c
}

// WagerTransactionProcessed: operação concluída com sucesso, incluindo LOSS.
type WagerTransactionProcessed struct {
	base          `json:"-"`
	TransactionID uuid.UUID   `json:"transactionId"`
	WalletID      uuid.UUID   `json:"walletId"`
	PlayerID      uuid.UUID   `json:"playerId"`
	Kind          string      `json:"kind"`
	Money         money.Money `json:"money"`
	BalanceAfter  money.Money `json:"balanceAfter"`
	External      *External   `json:"external,omitempty"`
}

func NewWagerTransactionProcessed(txID, walletID, playerID uuid.UUID, kind string, amount, balanceAfter money.Money, ext *External, at time.Time) WagerTransactionProcessed {
	return WagerTransactionProcessed{
		base:          newBase(TypeWagerTransactionProcessed, txID, at),
		TransactionID: txID, WalletID: walletID, PlayerID: playerID,
		Kind: kind, Money: amount, BalanceAfter: balanceAfter, External: ext.clone(),
	}
}

// WagerTransactionRejected: rejeição definitiva por regra de negócio.
type WagerTransactionRejected struct {
	base          `json:"-"`
	TransactionID uuid.UUID   `json:"transactionId"`
	WalletID      uuid.UUID   `json:"walletId"`
	PlayerID      uuid.UUID   `json:"playerId"`
	Kind          string      `json:"kind"`
	Money         money.Money `json:"money"`
	FailureCode   string      `json:"failureCode"`
	External      *External   `json:"external,omitempty"`
}

func NewWagerTransactionRejected(txID, walletID, playerID uuid.UUID, kind string, amount money.Money, failureCode string, ext *External, at time.Time) WagerTransactionRejected {
	return WagerTransactionRejected{
		base:          newBase(TypeWagerTransactionRejected, txID, at),
		TransactionID: txID, WalletID: walletID, PlayerID: playerID,
		Kind: kind, Money: amount, FailureCode: failureCode, External: ext.clone(),
	}
}

// WagerTransactionPendingReference: operação aguardando sua referência.
type WagerTransactionPendingReference struct {
	base          `json:"-"`
	TransactionID uuid.UUID   `json:"transactionId"`
	WalletID      uuid.UUID   `json:"walletId"`
	Kind          string      `json:"kind"`
	Money         money.Money `json:"money"`
	External      *External   `json:"external"`
	NextAttemptAt time.Time   `json:"nextAttemptAt"`
}

func NewWagerTransactionPendingReference(txID, walletID uuid.UUID, kind string, amount money.Money, ext *External, nextAttemptAt, at time.Time) WagerTransactionPendingReference {
	return WagerTransactionPendingReference{
		base:          newBase(TypeWagerTransactionPendingReference, txID, at),
		TransactionID: txID, WalletID: walletID, Kind: kind, Money: amount,
		External: ext.clone(), NextAttemptAt: nextAttemptAt.UTC(),
	}
}

// WalletBalanceChanged: alteração efetiva do saldo. O agregado é a carteira.
type WalletBalanceChanged struct {
	base          `json:"-"`
	WalletID      uuid.UUID   `json:"walletId"`
	TransactionID uuid.UUID   `json:"transactionId"`
	Direction     string      `json:"direction"`
	Money         money.Money `json:"money"`
	BalanceBefore money.Money `json:"balanceBefore"`
	BalanceAfter  money.Money `json:"balanceAfter"`
	WalletVersion int64       `json:"walletVersion"`
}

func NewWalletBalanceChanged(walletID, txID uuid.UUID, direction string, amount, before, after money.Money, walletVersion int64, at time.Time) WalletBalanceChanged {
	return WalletBalanceChanged{
		base:     newBase(TypeWalletBalanceChanged, walletID, at),
		WalletID: walletID, TransactionID: txID, Direction: direction,
		Money: amount, BalanceBefore: before, BalanceAfter: after, WalletVersion: walletVersion,
	}
}

// Envelope é o formato publicado. Tipo, versão, agregado e instante vêm do
// próprio evento; quem publica fornece apenas identidade e rastreio.
type Envelope struct {
	EventID       uuid.UUID  `json:"eventId"`
	EventType     string     `json:"eventType"`
	AggregateID   uuid.UUID  `json:"aggregateId"`
	CorrelationID string     `json:"correlationId"`
	CausationID   *uuid.UUID `json:"causationId,omitempty"`
	OccurredAt    time.Time  `json:"occurredAt"`
	Version       int        `json:"version"`
	Data          Event      `json:"data"`
}

// NewEnvelope embrulha um evento. eventID deve ser gerado uma vez e gravado
// na outbox, para que republicações preservem a mesma identidade.
func NewEnvelope(eventID uuid.UUID, correlationID string, causationID *uuid.UUID, ev Event) Envelope {
	return Envelope{
		EventID:       eventID,
		EventType:     ev.EventType(),
		AggregateID:   ev.AggregateID(),
		CorrelationID: correlationID,
		CausationID:   causationID,
		OccurredAt:    ev.OccurredAt(),
		Version:       ev.EventVersion(),
		Data:          ev,
	}
}
