package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"time"

	"github.com/Taino-Edu/Desafio-go-arqui/internal/domain/domainerr"
)

// QueueMessage é uma operação recebida pela fila, já decodificada.
type QueueMessage struct {
	Consumer  string // nome do consumidor (escopo da inbox)
	MessageID string // messageId do envelope: identidade durável da mensagem
	Input     SubmitInput
}

// QueueOutcome é o desfecho do tratamento de uma mensagem.
type QueueOutcome struct {
	// Duplicate: a inbox já tinha concluído esta mensagem; nada foi refeito.
	Duplicate bool
	// Result: desfecho da operação (nil em Duplicate).
	Result *SubmitResult
}

// HandleQueueMessage trata uma mensagem com as MESMAS garantias do HTTP.
//
// Tudo numa única transação SQL:
//  1. registra (consumidor, messageId, hash) na inbox; se outro processo
//     estiver com a mesma mensagem, espera;
//  2. se a inbox já concluiu esta mensagem: é reentrega, não faz nada
//     (Duplicate) — e se o hash mudou, ErrInboxConflict;
//  3. processa a operação pelo núcleo do HTTP (submitInTx), com a
//     idempotência por (provedor, chave) de data.idempotencyKey;
//  4. marca a mensagem como concluída na inbox.
//
// Só depois do COMMIT o adaptador apaga a mensagem da fila. Se o processo
// morrer entre o COMMIT e a remoção, a reentrega cai no passo 2.
//
// Rejeições de negócio são desfechos confirmados (a mensagem pode ser
// removida). Erros de validação, de conflito de idempotência e de inbox são
// permanentes (DLQ). ErrTransient pode ser tentado de novo.
func (s *WagerService) HandleQueueMessage(ctx context.Context, m QueueMessage) (QueueOutcome, error) {
	if m.Consumer == "" || m.MessageID == "" {
		return QueueOutcome{}, domainerr.Field("messageId", "required")
	}
	start := time.Now()
	kind, bizHash, err := prepare(m.Input)
	if err != nil {
		return QueueOutcome{}, err
	}
	// o hash da mensagem cobre o conteúdo de negócio E a chave de idempotência
	sum := sha256.Sum256([]byte(bizHash + "|" + m.Input.IdempotencyKey))
	msgHash := hex.EncodeToString(sum[:])

	var out QueueOutcome
	err = s.retryTransient(ctx, SourceSQS, func() error {
		out = QueueOutcome{} // nada de uma tentativa desfeita sobrevive
		return s.store.WithinTx(ctx, func(ctx context.Context, r Repositories) error {
			now := s.clock.Now()
			existing, inserted, err := r.Inbox().Register(ctx, m.Consumer, m.MessageID, msgHash, now)
			if err != nil {
				return err
			}
			if !inserted {
				if existing.PayloadHash != msgHash {
					return ErrInboxConflict
				}
				if existing.Completed {
					out = QueueOutcome{Duplicate: true}
					return nil
				}
			}
			res, err := s.submitInTx(ctx, r, m.Input, kind, bizHash)
			if err != nil {
				return err
			}
			if err := r.Inbox().Complete(ctx, m.Consumer, m.MessageID, s.clock.Now()); err != nil {
				return err
			}
			out = QueueOutcome{Result: &res}
			return nil
		})
	})
	if err != nil {
		s.observe(SourceSQS, SubmitResult{}, err, start)
		return QueueOutcome{}, err
	}
	// reentregas (Duplicate) são contadas pelo consumidor, que as reconhece
	if out.Result != nil {
		s.observe(SourceSQS, *out.Result, nil, start)
	}
	return out, nil
}

// IsPermanentQueueError informa se o erro nunca vai se resolver tentando de
// novo (a mensagem deve ir para a DLQ).
func IsPermanentQueueError(err error) bool {
	return errors.Is(err, domainerr.ErrValidation) ||
		errors.Is(err, ErrIdempotencyKeyReused) ||
		errors.Is(err, ErrDuplicateTransaction) ||
		errors.Is(err, ErrInboxConflict) ||
		errors.Is(err, ErrForbidden)
}
