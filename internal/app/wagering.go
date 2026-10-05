package app

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/Taino-Edu/Desafio-go-arqui/internal/domain/accounting"
	"github.com/Taino-Edu/Desafio-go-arqui/internal/domain/domainerr"
	"github.com/Taino-Edu/Desafio-go-arqui/internal/domain/money"
	"github.com/Taino-Edu/Desafio-go-arqui/internal/domain/wagering"
	"github.com/Taino-Edu/Desafio-go-arqui/internal/domain/wallet"
)

// WagerService processa operações dos provedores. É o MESMO caso de uso para
// HTTP e SQS: as garantias de idempotência e concorrência não dependem da
// porta de entrada.
type WagerService struct {
	store     Store
	clock     Clock
	ids       IDGenerator
	refPolicy wagering.ReferenceRetryPolicy
	metrics   Metrics
}

func NewWagerService(store Store, clock Clock, ids IDGenerator, refPolicy wagering.ReferenceRetryPolicy, opts ...Option) *WagerService {
	o := applyOptions(opts)
	return &WagerService{store: store, clock: clock, ids: ids, refPolicy: refPolicy, metrics: o.metrics}
}

// SubmitInput é uma operação recebida de um provedor.
type SubmitInput struct {
	ProviderID                     string
	ExternalTransactionID          string
	IdempotencyKey                 string
	PlayerID                       uuid.UUID
	WalletID                       uuid.UUID
	RoundID                        string
	GameID                         string
	Kind                           string
	Money                          money.Money
	ReferenceExternalTransactionID string
}

// SubmitResult é o desfecho. Em replays, Transaction é o registro original
// (com o saldo observado no processamento original).
type SubmitResult struct {
	Transaction *wagering.WagerTransaction
	Replay      bool
}

// maxTransientAttempts: tentativas automáticas diante de falha transitória.
// Repetir é seguro porque o caso de uso é idempotente: uma tentativa que
// chegou a confirmar vira replay na seguinte.
const maxTransientAttempts = 3

// Submit valida e processa uma operação com idempotência persistente.
func (s *WagerService) Submit(ctx context.Context, in SubmitInput) (SubmitResult, error) {
	start := time.Now()
	kind, hash, err := prepare(in)
	if err != nil {
		return SubmitResult{}, err
	}
	var res SubmitResult
	err = s.retryTransient(ctx, SourceHTTP, func() error {
		return s.store.WithinTx(ctx, func(ctx context.Context, r Repositories) error {
			var err error
			res, err = s.submitInTx(ctx, r, in, kind, hash)
			return err
		})
	})
	s.observe(SourceHTTP, res, err, start)
	if err != nil {
		return SubmitResult{}, err // nada de uma tentativa desfeita sobrevive
	}
	return res, nil
}

// observe registra o desfecho de um envio nas métricas.
func (s *WagerService) observe(source string, res SubmitResult, err error, start time.Time) {
	switch {
	case errors.Is(err, ErrIdempotencyKeyReused):
		s.metrics.IdempotencyConflict(source, ConflictKeyReused)
	case errors.Is(err, ErrDuplicateTransaction):
		s.metrics.IdempotencyConflict(source, ConflictDuplicateTransaction)
	case err != nil:
		// falhas de infraestrutura aparecem em retries, conflitos e no HTTP
	case res.Replay:
		s.metrics.IdempotentReplay(source)
		s.metrics.ProcessingDuration(source, time.Since(start))
	default:
		tx := res.Transaction
		s.metrics.WagerOutcome(source, string(tx.Kind()), string(tx.Status()))
		s.metrics.ProcessingDuration(source, time.Since(start))
	}
}

// prepare valida o tipo e calcula o hash do conteúdo de negócio.
func prepare(in SubmitInput) (wagering.Kind, string, error) {
	kind, err := wagering.ParseExternalKind(in.Kind)
	if err != nil {
		return "", "", err
	}
	hash, err := wagering.BusinessPayload{
		ProviderID: in.ProviderID, ExternalTransactionID: in.ExternalTransactionID,
		PlayerID: in.PlayerID, WalletID: in.WalletID, RoundID: in.RoundID, GameID: in.GameID,
		Kind: kind, Money: in.Money, ReferenceExternalTransactionID: in.ReferenceExternalTransactionID,
	}.Hash()
	if err != nil {
		return "", "", domainerr.Field("money", "required")
	}
	return kind, hash, nil
}

// retryTransient repete fn diante de ErrTransient, com espera crescente
// (20ms, 80ms), e conta as novas tentativas e as disputas de escrita.
func (s *WagerService) retryTransient(ctx context.Context, source string, fn func() error) error {
	for attempt := 1; ; attempt++ {
		err := fn()
		if err == nil || !errors.Is(err, ErrTransient) {
			return err
		}
		if errors.Is(err, ErrConcurrencyConflict) {
			s.metrics.ConcurrencyConflict(source)
		}
		if attempt == maxTransientAttempts {
			return err
		}
		select {
		case <-ctx.Done():
			return err
		case <-time.After(time.Duration(attempt*attempt) * 20 * time.Millisecond):
		}
		s.metrics.TransientRetry(source) // só conta a tentativa que vai acontecer
	}
}

// submitInTx processa a operação dentro de uma transação SQL já aberta. É o
// núcleo compartilhado por HTTP e SQS: o consumidor SQS chama esta função na
// MESMA transação em que registra e conclui a mensagem na inbox.
func (s *WagerService) submitInTx(ctx context.Context, r Repositories, in SubmitInput, kind wagering.Kind, hash string) (SubmitResult, error) {
	txID, err := s.ids.NewID()
	if err != nil {
		return SubmitResult{}, err
	}
	entryID, err := s.ids.NewID()
	if err != nil {
		return SubmitResult{}, err
	}
	now := s.clock.Now()

	tx, err := wagering.NewExternal(wagering.NewExternalParams{
		ID: txID, Kind: kind, WalletID: in.WalletID, PlayerID: in.PlayerID, Amount: in.Money, Now: now,
		External: wagering.External{
			ProviderID: in.ProviderID, ExternalTransactionID: in.ExternalTransactionID,
			IdempotencyKey: in.IdempotencyKey, PayloadHash: hash,
			RoundID: in.RoundID, GameID: in.GameID,
			ReferenceExternalTransactionID: in.ReferenceExternalTransactionID,
		},
	})
	if err != nil {
		return SubmitResult{}, err
	}

	var result SubmitResult
	err = func() error {
		// 1. Idempotência: registra a operação. Se outra requisição com a
		//    mesma chave estiver em andamento, o banco faz esta esperar.
		inserted, err := r.Transactions().InsertIfAbsent(ctx, tx)
		if err != nil {
			return err
		}
		if !inserted {
			existing, err := r.Transactions().FindExisting(ctx, in.ProviderID, in.IdempotencyKey, in.ExternalTransactionID)
			if err != nil {
				return err
			}
			if existing == nil {
				// a ocupante foi desfeita entre o INSERT e a busca: tenta de novo
				return fmt.Errorf("%w: %w: idempotency slot released concurrently", ErrTransient, ErrConcurrencyConflict)
			}
			if err := matchExisting(existing, in.IdempotencyKey, in.ExternalTransactionID, hash); err != nil {
				return err
			}
			result = SubmitResult{Transaction: existing, Replay: true}
			return nil
		}

		// 2. Trava SÓ esta carteira até o COMMIT. Operações de outras
		//    carteiras seguem em paralelo.
		w, err := r.Wallets().GetForUpdate(ctx, in.WalletID)
		if err != nil && !errors.Is(err, ErrWalletNotFound) {
			return err
		}

		// 3. Referência (REFUND, ROLLBACK, WIN com referência).
		var ref *wagering.WagerTransaction
		if in.ReferenceExternalTransactionID != "" {
			ref, err = r.Transactions().GetByExternalID(ctx, in.ProviderID, in.ReferenceExternalTransactionID)
			if err != nil && !errors.Is(err, ErrTransactionNotFound) {
				return err
			}
		}

		// 4. Regras de negócio.
		entry, awaiting, err := s.evaluate(ctx, r, tx, w, ref, entryID, now)
		if err != nil {
			return err
		}
		if awaiting {
			// a referência ainda não chegou: fica durável como
			// PENDING_REFERENCE e o worker de referências assume daqui
			if err := tx.MarkPendingReference(now.Add(s.refPolicy.Delay(0)), now); err != nil {
				return err
			}
		}

		// 5. Persiste tudo no mesmo commit: estado, saldo, ledger, eventos.
		if err := s.persist(ctx, r, tx, w, entry); err != nil {
			return err
		}

		// 6. Se esta operação pode ser referência de outra (BET, WIN, REFUND)
		//    e foi concluída, antecipa as pendências que esperam por ela.
		if tx.Status() == wagering.StatusProcessed && tx.Kind() != wagering.KindLoss && tx.Kind() != wagering.KindRollback {
			if err := r.Transactions().NudgePendingReferences(ctx, in.ProviderID, in.ExternalTransactionID, now); err != nil {
				return err
			}
		}
		result = SubmitResult{Transaction: tx}
		return nil
	}()
	return result, err
}

// evaluate aplica as regras de negócio à transação (PENDING ou
// PENDING_REFERENCE) com a carteira já travada. Devolve o lançamento a
// gravar (nil se não houver movimentação) e awaiting=true quando a referência
// ainda não está disponível; nesse caso a transação NÃO muda de estado e quem
// chama decide entre registrar a pendência, reagendar ou expirar.
func (s *WagerService) evaluate(ctx context.Context, r Repositories, tx *wagering.WagerTransaction,
	w *wallet.Wallet, ref *wagering.WagerTransaction, entryID uuid.UUID, now time.Time) (entry *wallet.LedgerEntry, awaiting bool, err error) {

	// Uma referência recebe no máximo uma reversão bem-sucedida. A consulta é
	// segura porque a carteira (da operação e da referência) está travada; o
	// índice único parcial no banco é a última barreira.
	if ref != nil && (tx.Kind() == wagering.KindRefund || tx.Kind() == wagering.KindRollback) &&
		ref.WalletID() == tx.WalletID() {
		done, err := r.Transactions().HasProcessedReversal(ctx, ref.ID())
		if err != nil {
			return nil, false, err
		}
		if done {
			return nil, false, tx.MarkRejected(wagering.FailureAlreadyReversed, now)
		}
	}

	out, err := wagering.Apply(wagering.ApplyInput{
		Transaction: tx, Wallet: w, Reference: ref, LedgerEntryID: entryID, Now: now,
	})
	if err != nil {
		return nil, false, err
	}
	return out.Entry, out.Result == wagering.ResultAwaitingReference, nil
}

// persist grava, na transação SQL corrente, o novo estado da operação, o
// saldo, o lançamento e as partidas dobradas (se houve movimentação) e os
// eventos na outbox.
func (s *WagerService) persist(ctx context.Context, r Repositories, tx *wagering.WagerTransaction,
	w *wallet.Wallet, entry *wallet.LedgerEntry) error {

	if err := r.Transactions().Update(ctx, tx); err != nil {
		return err
	}
	evs := tx.PullEvents()
	if w != nil {
		if moved := w.PullEvents(); len(moved) > 0 {
			if err := r.Wallets().Update(ctx, w); err != nil {
				return err
			}
			evs = append(evs, moved...)
		}
	}
	if entry != nil {
		journal, err := accounting.ForTransaction(tx, *entry)
		if err != nil {
			return err
		}
		if err := r.Ledger().Insert(ctx, *entry); err != nil {
			return err
		}
		if err := r.Journal().Post(ctx, journal); err != nil {
			return err
		}
	}
	id := tx.ID()
	records, err := toOutboxRecords(ctx, s.ids, &id, evs...)
	if err != nil {
		return err
	}
	return r.Outbox().Append(ctx, records...)
}

// matchExisting decide o que fazer quando a chave ou o id externo já existem:
//
//   - mesma chave, mesmo id externo e mesmo conteúdo: replay (nil);
//   - mesma chave com outro conteúdo: ErrIdempotencyKeyReused;
//   - mesmo id externo com outra chave: ErrDuplicateTransaction (a operação
//     financeira não é reaplicada com outra chave).
func matchExisting(existing *wagering.WagerTransaction, key, externalID, hash string) error {
	ext := existing.External()
	if ext == nil {
		return fmt.Errorf("existing transaction %s has no external metadata", existing.ID())
	}
	if ext.IdempotencyKey == key {
		if ext.ExternalTransactionID == externalID && ext.PayloadHash == hash {
			return nil
		}
		return ErrIdempotencyKeyReused
	}
	return ErrDuplicateTransaction
}

// GetTransaction devolve uma transação pelo id interno.
func (s *WagerService) GetTransaction(ctx context.Context, id uuid.UUID) (*wagering.WagerTransaction, error) {
	return s.store.Reader().Transactions().GetByID(ctx, id)
}

// GetByExternalID devolve uma transação pelo (provedor, id externo).
func (s *WagerService) GetByExternalID(ctx context.Context, providerID, externalID string) (*wagering.WagerTransaction, error) {
	return s.store.Reader().Transactions().GetByExternalID(ctx, providerID, externalID)
}
