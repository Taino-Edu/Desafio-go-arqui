package wagering

import (
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/Taino-Edu/Desafio-go-arqui/internal/domain/domainerr"
	"github.com/Taino-Edu/Desafio-go-arqui/internal/domain/events"
	"github.com/Taino-Edu/Desafio-go-arqui/internal/domain/money"
)

// ErrInvalidTransition indica uma mudança de estado proibida pela máquina de
// estados (por exemplo, sair de um estado terminal).
var ErrInvalidTransition = errors.New("wagering: invalid state transition")

// TransitionError detalha a transição recusada. Casa com ErrInvalidTransition.
type TransitionError struct{ From, To Status }

func (e *TransitionError) Error() string {
	return fmt.Sprintf("wagering: invalid transition %s -> %s", e.From, e.To)
}
func (e *TransitionError) Is(target error) bool { return target == ErrInvalidTransition }

// External são os metadados de uma operação enviada por um provedor.
type External struct {
	ProviderID            string
	ExternalTransactionID string
	IdempotencyKey        string
	PayloadHash           string // hash do JSON canônico dos campos de negócio
	RoundID               string
	GameID                string
	// ReferenceExternalTransactionID é obrigatório em REFUND/ROLLBACK,
	// opcional em WIN e proibido nos demais.
	ReferenceExternalTransactionID string
}

func (e External) validate() error {
	required := []struct{ name, value string }{
		{"providerId", e.ProviderID},
		{"externalTransactionId", e.ExternalTransactionID},
		{"idempotencyKey", e.IdempotencyKey},
		{"payloadHash", e.PayloadHash},
		{"roundId", e.RoundID},
		{"gameId", e.GameID},
	}
	for _, f := range required {
		if f.value == "" {
			return domainerr.Field(f.name, "required")
		}
	}
	return nil
}

// HasReference informa se a operação aponta para outra.
func (e External) HasReference() bool { return e.ReferenceExternalTransactionID != "" }

func (e *External) eventView() *events.External {
	if e == nil {
		return nil
	}
	return &events.External{
		ProviderID: e.ProviderID, ExternalTransactionID: e.ExternalTransactionID,
		RoundID: e.RoundID, GameID: e.GameID,
		ReferenceExternalTransactionID: e.ReferenceExternalTransactionID,
	}
}

// WagerTransaction é uma operação financeira e seu ciclo de vida.
type WagerTransaction struct {
	id       uuid.UUID
	origin   Origin
	kind     Kind
	status   Status
	walletID uuid.UUID
	playerID uuid.UUID
	amount   money.Money
	external *External // nil quando origin = INTERNAL

	// preenchidos conforme o processamento
	referenceTransactionID *uuid.UUID   // referência interna resolvida
	failureCode            FailureCode  // REJECTED ou FAILED
	balanceAfter           *money.Money // saldo observado ao concluir; devolvido em replays
	attempts               int          // tentativas de resolução de referência
	nextAttemptAt          *time.Time

	createdAt time.Time
	updatedAt time.Time

	events []events.Event
}

// NewOpening cria a transação interna de abertura de carteira, já PROCESSED.
// Só existe quando o saldo inicial é positivo.
func NewOpening(id, walletID, playerID uuid.UUID, amount money.Money, now time.Time) (*WagerTransaction, error) {
	switch {
	case id == uuid.Nil:
		return nil, domainerr.Field("id", "required")
	case walletID == uuid.Nil:
		return nil, domainerr.Field("walletId", "required")
	case playerID == uuid.Nil:
		return nil, domainerr.Field("playerId", "required")
	case !amount.IsPositive():
		return nil, domainerr.Field("amount", "opening requires a positive amount")
	case now.IsZero():
		return nil, domainerr.Field("now", "required")
	}
	now = now.UTC()
	t := &WagerTransaction{
		id: id, origin: OriginInternal, kind: KindOpening, status: StatusPending,
		walletID: walletID, playerID: playerID, amount: amount,
		createdAt: now, updatedAt: now,
	}
	if err := t.MarkProcessed(amount, nil, now); err != nil {
		return nil, err
	}
	return t, nil
}

// NewExternalParams são os dados de uma operação de provedor.
type NewExternalParams struct {
	ID       uuid.UUID
	Kind     Kind
	WalletID uuid.UUID
	PlayerID uuid.UUID
	Amount   money.Money
	External External
	Now      time.Time
}

// NewExternal valida e cria uma operação externa em PENDING.
//
// Regras de valor: LOSS exige exatamente 0.00; BET, WIN, REFUND e ROLLBACK
// exigem valor maior que zero. Referência: obrigatória em REFUND/ROLLBACK,
// opcional em WIN, proibida em BET/LOSS.
func NewExternal(p NewExternalParams) (*WagerTransaction, error) {
	switch {
	case p.ID == uuid.Nil:
		return nil, domainerr.Field("id", "required")
	case p.Kind == KindOpening:
		return nil, domainerr.Field("kind", "OPENING is internal and cannot be submitted")
	case !p.Kind.valid():
		return nil, domainerr.Field("kind", "invalid")
	case p.WalletID == uuid.Nil:
		return nil, domainerr.Field("walletId", "required")
	case p.PlayerID == uuid.Nil:
		return nil, domainerr.Field("playerId", "required")
	case !p.Amount.IsValid():
		return nil, domainerr.Field("money", "required")
	case p.Now.IsZero():
		return nil, domainerr.Field("now", "required")
	}
	if err := p.External.validate(); err != nil {
		return nil, err
	}

	if p.Kind == KindLoss {
		if !p.Amount.IsZero() {
			return nil, domainerr.Field("money.amount", `LOSS requires "0.00"`)
		}
	} else if !p.Amount.IsPositive() {
		return nil, domainerr.Field("money.amount", "must be greater than zero")
	}

	hasRef := p.External.HasReference()
	if p.Kind.requiresReference() && !hasRef {
		return nil, domainerr.Field("referenceExternalTransactionId", "required for "+string(p.Kind))
	}
	if hasRef && !p.Kind.acceptsReference() {
		return nil, domainerr.Field("referenceExternalTransactionId", "not allowed for "+string(p.Kind))
	}
	if hasRef && p.External.ReferenceExternalTransactionID == p.External.ExternalTransactionID {
		return nil, domainerr.Field("referenceExternalTransactionId", "cannot reference itself")
	}

	now := p.Now.UTC()
	ext := p.External
	return &WagerTransaction{
		id: p.ID, origin: OriginExternal, kind: p.Kind, status: StatusPending,
		walletID: p.WalletID, playerID: p.PlayerID, amount: p.Amount, external: &ext,
		createdAt: now, updatedAt: now,
	}, nil
}

// RehydrateParams são os dados lidos do banco.
type RehydrateParams struct {
	ID                     uuid.UUID
	Origin                 Origin
	Kind                   Kind
	Status                 Status
	WalletID               uuid.UUID
	PlayerID               uuid.UUID
	Amount                 money.Money
	External               *External
	ReferenceTransactionID *uuid.UUID
	FailureCode            FailureCode
	BalanceAfter           *money.Money
	Attempts               int
	NextAttemptAt          *time.Time
	CreatedAt              time.Time
	UpdatedAt              time.Time
}

// Rehydrate reconstrói uma transação existente sem executar transições nem
// emitir eventos. Valida a coerência do registro.
func Rehydrate(p RehydrateParams) (*WagerTransaction, error) {
	switch {
	case p.ID == uuid.Nil || p.WalletID == uuid.Nil || p.PlayerID == uuid.Nil:
		return nil, domainerr.Field("ids", "required")
	case !p.Kind.valid():
		return nil, domainerr.Field("kind", "invalid")
	case !p.Status.valid():
		return nil, domainerr.Field("status", "invalid")
	case !p.Amount.IsValid() || p.Amount.IsNegative():
		return nil, domainerr.Field("amount", "invalid")
	case p.CreatedAt.IsZero() || p.UpdatedAt.IsZero():
		return nil, domainerr.Field("timestamps", "required")
	case p.Attempts < 0:
		return nil, domainerr.Field("attempts", "must not be negative")
	}

	switch p.Origin {
	case OriginInternal:
		if p.Kind != KindOpening || p.External != nil {
			return nil, domainerr.Field("origin", "INTERNAL must be OPENING without external metadata")
		}
	case OriginExternal:
		if p.Kind == KindOpening || p.External == nil {
			return nil, domainerr.Field("origin", "EXTERNAL requires external metadata and a non-OPENING kind")
		}
		if err := p.External.validate(); err != nil {
			return nil, err
		}
	default:
		return nil, domainerr.Field("origin", "invalid")
	}

	switch p.Status {
	case StatusProcessed:
		if p.BalanceAfter == nil || p.FailureCode != "" {
			return nil, domainerr.Field("status", "PROCESSED requires balanceAfter and no failureCode")
		}
	case StatusRejected, StatusFailed:
		if !p.FailureCode.valid() {
			return nil, domainerr.Field("failureCode", "required for "+string(p.Status))
		}
	case StatusPendingReference:
		if p.NextAttemptAt == nil {
			return nil, domainerr.Field("nextAttemptAt", "required for PENDING_REFERENCE")
		}
	}

	t := &WagerTransaction{
		id: p.ID, origin: p.Origin, kind: p.Kind, status: p.Status,
		walletID: p.WalletID, playerID: p.PlayerID, amount: p.Amount,
		referenceTransactionID: p.ReferenceTransactionID, failureCode: p.FailureCode,
		balanceAfter: p.BalanceAfter, attempts: p.Attempts, nextAttemptAt: p.NextAttemptAt,
		createdAt: p.CreatedAt.UTC(), updatedAt: p.UpdatedAt.UTC(),
	}
	if p.External != nil {
		ext := *p.External
		t.external = &ext
	}
	return t, nil
}

func (t *WagerTransaction) transition(to Status, now time.Time) error {
	if t == nil || t.id == uuid.Nil {
		return domainerr.ErrUninitialized
	}
	if !canTransition(t.status, to) {
		return &TransitionError{From: t.status, To: to}
	}
	if now.IsZero() {
		return domainerr.Field("now", "required")
	}
	t.status = to
	if now = now.UTC(); now.After(t.updatedAt) {
		t.updatedAt = now
	}
	return nil
}

// MarkProcessed conclui com sucesso, guardando o saldo observado para replays.
func (t *WagerTransaction) MarkProcessed(balanceAfter money.Money, referenceID *uuid.UUID, now time.Time) error {
	if !balanceAfter.IsValid() || balanceAfter.IsNegative() {
		return domainerr.Field("balanceAfter", "must be zero or positive")
	}
	if err := t.transition(StatusProcessed, now); err != nil {
		return err
	}
	t.balanceAfter = &balanceAfter
	if referenceID != nil {
		ref := *referenceID
		t.referenceTransactionID = &ref
	}
	t.nextAttemptAt = nil
	t.events = append(t.events, events.NewWagerTransactionProcessed(
		t.id, t.walletID, t.playerID, string(t.kind), t.amount, balanceAfter,
		t.external.eventView(), t.updatedAt,
	))
	return nil
}

// MarkRejected recusa a operação por regra de negócio (terminal).
func (t *WagerTransaction) MarkRejected(code FailureCode, now time.Time) error {
	if !code.valid() || code == FailurePermanentError {
		return domainerr.Field("failureCode", "invalid rejection code")
	}
	if err := t.transition(StatusRejected, now); err != nil {
		return err
	}
	t.failureCode = code
	t.nextAttemptAt = nil
	t.events = append(t.events, events.NewWagerTransactionRejected(
		t.id, t.walletID, t.playerID, string(t.kind), t.amount, string(code),
		t.external.eventView(), t.updatedAt,
	))
	return nil
}

// MarkFailed registra uma falha permanente de infraestrutura (terminal).
// Não emite evento de negócio: serve para auditoria.
func (t *WagerTransaction) MarkFailed(now time.Time) error {
	if err := t.transition(StatusFailed, now); err != nil {
		return err
	}
	t.failureCode = FailurePermanentError
	t.nextAttemptAt = nil
	return nil
}

// MarkPendingReference coloca a operação em espera pela referência.
func (t *WagerTransaction) MarkPendingReference(nextAttemptAt, now time.Time) error {
	if t != nil && (t.external == nil || !t.external.HasReference()) {
		return domainerr.Field("referenceExternalTransactionId", "operation has no reference to wait for")
	}
	if nextAttemptAt.IsZero() {
		return domainerr.Field("nextAttemptAt", "required")
	}
	if err := t.transition(StatusPendingReference, now); err != nil {
		return err
	}
	next := nextAttemptAt.UTC()
	t.nextAttemptAt = &next
	t.events = append(t.events, events.NewWagerTransactionPendingReference(
		t.id, t.walletID, string(t.kind), t.amount, t.external.eventView(), next, t.updatedAt,
	))
	return nil
}

// RescheduleReference registra mais uma tentativa sem sucesso e agenda a
// próxima. Só vale em PENDING_REFERENCE; não muda o estado nem emite evento.
func (t *WagerTransaction) RescheduleReference(nextAttemptAt, now time.Time) error {
	if t == nil || t.id == uuid.Nil {
		return domainerr.ErrUninitialized
	}
	if t.status != StatusPendingReference {
		return &TransitionError{From: t.status, To: StatusPendingReference}
	}
	if nextAttemptAt.IsZero() || now.IsZero() {
		return domainerr.Field("nextAttemptAt", "required")
	}
	t.attempts++
	next := nextAttemptAt.UTC()
	t.nextAttemptAt = &next
	if now = now.UTC(); now.After(t.updatedAt) {
		t.updatedAt = now
	}
	return nil
}

// PullEvents devolve os eventos pendentes e limpa a lista.
func (t *WagerTransaction) PullEvents() []events.Event {
	evs := t.events
	t.events = nil
	return evs
}

func (t *WagerTransaction) ID() uuid.UUID            { return t.id }
func (t *WagerTransaction) Origin() Origin           { return t.origin }
func (t *WagerTransaction) Kind() Kind               { return t.kind }
func (t *WagerTransaction) Status() Status           { return t.status }
func (t *WagerTransaction) WalletID() uuid.UUID      { return t.walletID }
func (t *WagerTransaction) PlayerID() uuid.UUID      { return t.playerID }
func (t *WagerTransaction) Amount() money.Money      { return t.amount }
func (t *WagerTransaction) FailureCode() FailureCode { return t.failureCode }
func (t *WagerTransaction) Attempts() int            { return t.attempts }
func (t *WagerTransaction) CreatedAt() time.Time     { return t.createdAt }
func (t *WagerTransaction) UpdatedAt() time.Time     { return t.updatedAt }

// External devolve uma cópia dos metadados externos (nil se interna).
func (t *WagerTransaction) External() *External {
	if t.external == nil {
		return nil
	}
	c := *t.external
	return &c
}

// ReferenceTransactionID devolve a referência interna resolvida, se houver.
func (t *WagerTransaction) ReferenceTransactionID() (uuid.UUID, bool) {
	if t.referenceTransactionID == nil {
		return uuid.Nil, false
	}
	return *t.referenceTransactionID, true
}

// BalanceAfter devolve o saldo observado na conclusão, se houver.
func (t *WagerTransaction) BalanceAfter() (money.Money, bool) {
	if t.balanceAfter == nil {
		return money.Money{}, false
	}
	return *t.balanceAfter, true
}

// NextAttemptAt devolve o próximo instante de tentativa, se houver.
func (t *WagerTransaction) NextAttemptAt() (time.Time, bool) {
	if t.nextAttemptAt == nil {
		return time.Time{}, false
	}
	return *t.nextAttemptAt, true
}
