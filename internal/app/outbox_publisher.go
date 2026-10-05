package app

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/google/uuid"
)

// OutboxConfig parametriza o publicador.
type OutboxConfig struct {
	Owner        string        // identifica a instância no arrendamento
	BatchSize    int           // agregados (cabeças) por rodada
	PerAggregate int           // eventos seguidos de um agregado por rodada (0 = padrão)
	Parallelism  int           // agregados publicados ao mesmo tempo (0 = padrão)
	Lease        time.Duration // por quanto tempo um evento reivindicado fica reservado
	RetryBase    time.Duration // backoff de falha: min(base × 2^(tentativas-1), max)
	RetryMax     time.Duration
}

// Padrões do publicador (ver o teste de carga em docs/LOAD-TEST.md).
const (
	DefaultOutboxPerAggregate = 20
	DefaultOutboxParallelism  = 8
)

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
	metrics   Metrics
}

func NewOutboxService(store Store, publisher EventPublisher, clock Clock, cfg OutboxConfig, hooks OutboxHooks, opts ...Option) *OutboxService {
	o := applyOptions(opts)
	if cfg.PerAggregate < 1 {
		cfg.PerAggregate = DefaultOutboxPerAggregate
	}
	if cfg.Parallelism < 1 {
		cfg.Parallelism = DefaultOutboxParallelism
	}
	return &OutboxService{store: store, publisher: publisher, clock: clock, cfg: cfg, hooks: hooks, metrics: o.metrics}
}

// PublishResult resume uma rodada.
type PublishResult struct {
	Claimed   int
	Published int
	Failed    int
}

// PublishBatch faz uma rodada:
//
//  1. reivindica (em uma transação curta, confirmada) até BatchSize agregados
//     prontos e, de cada um, até PerAggregate eventos seguidos, com
//     arrendamento: a reserva é durável, então outra instância não os pega
//     enquanto o arrendamento vale, e os pega se esta instância cair;
//  2. publica FORA de transação (rede lenta não segura locks), em ondas: a
//     onda n leva o n-ésimo evento de cada agregado, em lotes de até 10 por
//     chamada ao SQS e até Parallelism chamadas ao mesmo tempo. Agregados
//     diferentes não têm ordem entre si; dentro de um agregado, o evento n+1
//     só sai depois do n;
//  3. marca os publicados de cada onda num único UPDATE. Se um falha, ele
//     recebe backoff e os seguintes do MESMO agregado são liberados na hora:
//     esperam atrás dele, preservando a ordem.
//
// A outbox só contém eventos de transações CONFIRMADAS (foram gravados no
// mesmo commit da operação), então nada é publicado antes do commit.
//
// Entrega at-least-once: se o processo cair entre publicar e marcar, o
// arrendamento vence e outra instância republica o MESMO evento, com o
// MESMO eventId. Os consumidores deduplicam por eventId.
func (s *OutboxService) PublishBatch(ctx context.Context) (res PublishResult, err error) {
	defer func() {
		s.metrics.OutboxPublished(res.Published)
		s.metrics.OutboxFailed(res.Failed)
	}()
	return s.publishBatch(ctx)
}

func (s *OutboxService) publishBatch(ctx context.Context) (PublishResult, error) {
	var claimed []OutboxMessage
	err := s.store.WithinTx(ctx, func(ctx context.Context, r Repositories) error {
		var err error
		claimed, err = r.Outbox().Claim(ctx, s.cfg.Owner, s.clock.Now(), s.cfg.Lease, s.cfg.BatchSize, s.cfg.PerAggregate)
		return err
	})
	if err != nil || len(claimed) == 0 {
		return PublishResult{}, err
	}
	if s.hooks.AfterClaim != nil && errors.Is(s.hooks.AfterClaim(), ErrSimulatedCrash) {
		return PublishResult{Claimed: len(claimed)}, nil
	}

	// agrupa por agregado, preservando a ordem de gravação dentro de cada um
	var active []uuid.UUID
	groups := map[uuid.UUID][]OutboxMessage{}
	for _, m := range claimed {
		if _, ok := groups[m.AggregateID]; !ok {
			active = append(active, m.AggregateID)
		}
		groups[m.AggregateID] = append(groups[m.AggregateID], m)
	}

	// Ondas: a onda n leva o n-ésimo evento de cada agregado ainda ativo.
	// Um mesmo lote nunca tem dois eventos do mesmo agregado, então uma falha
	// parcial do lote não fura a ordem; e a onda n+1 só sai depois da n.
	res := PublishResult{Claimed: len(claimed)}
	repo := s.store.Reader().Outbox()
	var errs error
	for wave := 0; len(active) > 0; wave++ {
		if ctx.Err() != nil {
			// desligando: devolve o que não começou para outra instância já
			for _, agg := range active {
				errs = errors.Join(errs, s.release(groups[agg][wave:]))
			}
			return res, errs
		}
		msgs := make([]OutboxMessage, len(active))
		for i, agg := range active {
			msgs[i] = groups[agg][wave]
		}
		sendErrs := s.send(ctx, msgs)

		var ok []uuid.UUID
		var next []uuid.UUID
		for i, m := range msgs {
			rest := groups[m.AggregateID][wave+1:]
			if sendErrs[i] != nil {
				// falhou: backoff neste evento; os seguintes do agregado esperam atrás dele
				res.Failed++
				retryAt := s.clock.Now().Add(s.backoff(m.Attempts))
				errs = errors.Join(errs,
					repo.MarkFailed(ctx, m.EventID, s.cfg.Owner, retryAt, sendErrs[i].Error()),
					s.release(rest))
				continue
			}
			if s.hooks.AfterPublish != nil && errors.Is(s.hooks.AfterPublish(m), ErrSimulatedCrash) {
				return res, nil // o "processo" morreu: nada desta onda é marcado
			}
			ok = append(ok, m.EventID)
			if len(rest) > 0 {
				next = append(next, m.AggregateID)
			}
		}
		if len(ok) > 0 {
			if err := repo.MarkPublishedMany(ctx, ok, s.clock.Now()); err != nil {
				// publicados mas não marcados: serão republicados com o mesmo eventId
				return res, errors.Join(errs, err)
			}
			res.Published += len(ok)
		}
		active = next
	}
	return res, errs
}

// send entrega uma onda: em lotes (BatchPublisher, até 10 por chamada) ou um
// a um, com até Parallelism chamadas ao mesmo tempo. Devolve um erro por
// mensagem, na mesma ordem.
func (s *OutboxService) send(ctx context.Context, msgs []OutboxMessage) []error {
	errs := make([]error, len(msgs))
	bp, batched := s.publisher.(BatchPublisher)
	size := 1
	if batched {
		size = 10
	}
	var wg sync.WaitGroup
	slots := make(chan struct{}, s.cfg.Parallelism)
	for start := 0; start < len(msgs); start += size {
		chunk := msgs[start:min(start+size, len(msgs))]
		wg.Add(1)
		slots <- struct{}{}
		go func() {
			defer func() { <-slots; wg.Done() }()
			if batched {
				copy(errs[start:], bp.PublishBatch(ctx, chunk))
				return
			}
			errs[start] = s.publisher.Publish(ctx, chunk[0])
		}()
	}
	wg.Wait()
	return errs
}

// release devolve eventos não publicados (com contexto próprio: também roda
// no desligamento, quando o da rodada já foi cancelado).
func (s *OutboxService) release(msgs []OutboxMessage) error {
	if len(msgs) == 0 {
		return nil
	}
	ids := make([]uuid.UUID, len(msgs))
	for i, m := range msgs {
		ids[i] = m.EventID
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return s.store.Reader().Outbox().Release(ctx, s.cfg.Owner, ids)
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
