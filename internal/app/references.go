package app

import (
	"context"
	"errors"
	"math/rand/v2"
	"time"

	"github.com/Taino-Edu/Desafio-go-arqui/internal/domain/wagering"
)

// ReferenceOutcome descreve o que aconteceu com uma pendência processada.
type ReferenceOutcome string

const (
	ReferenceResolved    ReferenceOutcome = "RESOLVED"    // processada ou rejeitada pelas regras
	ReferenceRescheduled ReferenceOutcome = "RESCHEDULED" // referência ainda indisponível
	ReferenceExpired     ReferenceOutcome = "EXPIRED"     // tentativas esgotadas: REFERENCE_NOT_FOUND
)

// ResolvePendingResult é o resultado de uma rodada do worker.
type ResolvePendingResult struct {
	Found       bool // havia uma pendência vencida
	Outcome     ReferenceOutcome
	Transaction *wagering.WagerTransaction
}

// ResolveNextPending pega UMA operação em PENDING_REFERENCE cuja próxima
// tentativa venceu e tenta concluí-la. Tudo acontece numa transação SQL:
//
//  1. trava a pendência com FOR UPDATE SKIP LOCKED: várias instâncias rodando
//     o worker pegam pendências diferentes, nunca a mesma;
//  2. trava a carteira e busca a referência;
//  3. aplica as MESMAS regras do envio (evaluate) e grava com o MESMO
//     persist: saldo, ledger, estado e eventos no mesmo commit;
//  4. se a referência ainda não está disponível, reagenda com backoff
//     exponencial + jitter, ou rejeita com REFERENCE_NOT_FOUND quando as
//     tentativas acabam.
//
// Se o processo morrer no meio, a transação é desfeita e a pendência continua
// lá, vencida: qualquer instância a pega de novo.
func (s *WagerService) ResolveNextPending(ctx context.Context) (ResolvePendingResult, error) {
	entryID, err := s.ids.NewID()
	if err != nil {
		return ResolvePendingResult{}, err
	}
	now := s.clock.Now()

	var res ResolvePendingResult
	err = s.store.WithinTx(ctx, func(ctx context.Context, r Repositories) error {
		tx, err := r.Transactions().ClaimDuePendingReference(ctx, now)
		if err != nil || tx == nil {
			return err
		}
		res = ResolvePendingResult{Found: true, Transaction: tx}

		w, err := r.Wallets().GetForUpdate(ctx, tx.WalletID())
		if err != nil && !errors.Is(err, ErrWalletNotFound) {
			return err
		}
		ext := tx.External()
		ref, err := r.Transactions().GetByExternalID(ctx, ext.ProviderID, ext.ReferenceExternalTransactionID)
		if err != nil && !errors.Is(err, ErrTransactionNotFound) {
			return err
		}

		entry, awaiting, err := s.evaluate(ctx, r, tx, w, ref, entryID, now)
		if err != nil {
			return err
		}
		res.Outcome = ReferenceResolved
		if awaiting {
			attempts := tx.Attempts() + 1
			if s.refPolicy.Exhausted(attempts) {
				// registra a última tentativa e encerra
				if err := tx.RescheduleReference(now, now); err != nil {
					return err
				}
				if err := tx.MarkRejected(wagering.FailureReferenceNotFound, now); err != nil {
					return err
				}
				res.Outcome = ReferenceExpired
			} else {
				if err := tx.RescheduleReference(now.Add(withJitter(s.refPolicy.Delay(attempts))), now); err != nil {
					return err
				}
				res.Outcome = ReferenceRescheduled
			}
		}
		return s.persist(ctx, r, tx, w, entry)
	})
	if err != nil {
		return ResolvePendingResult{}, err
	}
	return res, nil
}

// withJitter soma até 20% de aleatoriedade ao atraso, para que instâncias e
// pendências não acordem todas no mesmo instante.
func withJitter(d time.Duration) time.Duration {
	if d <= 0 {
		return d
	}
	return d + time.Duration(rand.Int64N(int64(d)/5+1))
}
