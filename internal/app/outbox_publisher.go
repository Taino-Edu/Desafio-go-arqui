package app

import (
	"context"
	"errors"
	"time"
)

// OutboxConfig parametriza o publicador.
type OutboxConfig struct {
	Owner     string        // identifica a instância no arrendamento
	BatchSize int           // eventos por rodada
	Lease     time.Duration // por quanto tempo um evento reivindicado fica reservado
	RetryBase time.Duration // backoff de falha: min(base × 2^(tentativas-1), max)
	RetryMax  time.Duration
}

// OutboxHooks permitem aos testes simular quedas. Em produção são nil.
type OutboxHooks struct {
	// AfterClaim: depois de reivindicar e antes de publicar.
	AfterClaim func() error
	// AfterPublish: depois de publicar e antes de marcar como publicado.
	AfterPublish func(OutboxMessage) error
}

// ErrSimulatedCrash simula a queda do processo num ponto exato.
var ErrSimulatedCrash = errors.New("simulated crash")

// OutboxService publica os eventos gravados na outbox.
type OutboxService struct {
	store     Store
	publisher EventPublisher
	clock     Clock
	cfg       OutboxConfig
	hooks     OutboxHooks
}

func NewOutboxService(store Store, publisher EventPublisher, clock Clock, cfg OutboxConfig, hooks OutboxHooks) *OutboxService {
	return &OutboxService{store: store, publisher: publisher, clock: clock, cfg: cfg, hooks: hooks}
}

// PublishResult resume uma rodada.
type PublishResult struct {
	Claimed   int
	Published int
	Failed    int
}

// PublishBatch faz uma rodada:
//
//  1. reivindica (em uma transação curta, confirmada) até BatchSize eventos
//     com arrendamento: a reserva é durável, então outra instância não os
//     pega enquanto o arrendamento vale, e os pega se esta instância cair;
//  2. publica cada evento FORA de transação (rede lenta não segura locks);
//  3. marca como publicado, ou agenda nova tentativa com backoff.
//
// A outbox só contém eventos de transações CONFIRMADAS (foram gravados no
// mesmo commit da operação), então nada é publicado antes do commit.
//
// Entrega at-least-once: se o processo cair entre publicar e marcar, o
// arrendamento vence e outra instância republica o MESMO evento, com o
// MESMO eventId. Os consumidores deduplicam por eventId.
func (s *OutboxService) PublishBatch(ctx context.Context) (PublishResult, error) {
	var claimed []OutboxMessage
	err := s.store.WithinTx(ctx, func(ctx context.Context, r Repositories) error {
		var err error
		claimed, err = r.Outbox().Claim(ctx, s.cfg.Owner, s.clock.Now(), s.cfg.Lease, s.cfg.BatchSize)
		return err
	})
	if err != nil || len(claimed) == 0 {
		return PublishResult{}, err
	}
	res := PublishResult{Claimed: len(claimed)}
	if s.hooks.AfterClaim != nil && errors.Is(s.hooks.AfterClaim(), ErrSimulatedCrash) {
		return res, nil
	}

	repo := s.store.Reader().Outbox()
	for _, m := range claimed {
		if ctx.Err() != nil {
			// desligando: os não publicados voltam quando o arrendamento vencer
			return res, nil
		}
		if err := s.publisher.Publish(ctx, m); err != nil {
			res.Failed++
			next := s.clock.Now().Add(s.backoff(m.Attempts))
			if markErr := repo.MarkFailed(ctx, m.EventID, s.cfg.Owner, next, err.Error()); markErr != nil {
				return res, markErr
			}
			continue
		}
		if s.hooks.AfterPublish != nil && errors.Is(s.hooks.AfterPublish(m), ErrSimulatedCrash) {
			return res, nil
		}
		if err := repo.MarkPublished(ctx, m.EventID, s.clock.Now()); err != nil {
			// publicado mas não marcado: será republicado com o mesmo eventId
			return res, err
		}
		res.Published++
	}
	return res, nil
}

func (s *OutboxService) backoff(attempts int) time.Duration {
	d := s.cfg.RetryBase
	for i := 1; i < attempts && d < s.cfg.RetryMax; i++ {
		d *= 2
	}
	if d > s.cfg.RetryMax {
		d = s.cfg.RetryMax
	}
	return d
}
