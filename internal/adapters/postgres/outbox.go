package postgres

import (
	"cmp"
	"context"
	"slices"
	"time"

	"github.com/google/uuid"

	"github.com/Taino-Edu/Desafio-go-arqui/internal/app"
)

// Claim reivindica eventos para publicação.
//
// Toda parte da consulta tem custo limitado pelo tamanho do lote, nunca pelo
// acúmulo, mesmo com estatísticas desatualizadas. Isso importa: logo depois
// de um pico, o Postgres ainda "acha" que quase nada está pendente e escolhe
// planos que seriam baratos com 1 pendente e são quadráticos com 40 mil (o
// teste de carga mostrou a publicação parar por statement_timeout; ver
// docs/LOAD-TEST.md e TestOutbox_ClaimScalesWithBacklog).
//
//   - ready: percorre os pendentes em ordem de gravação e fica com as
//     "cabeças" (o pendente de menor seq do seu agregado), já prontas
//     (next_attempt_at vencido) e sem arrendamento válido (ou com
//     arrendamento vencido: trabalho abandonado). A cabeça é conferida por
//     uma subconsulta ORDER BY seq LIMIT 1: qualquer índice que o
//     planejador escolha para ela para cedo. Para ao juntar heads cabeças.
//     FOR UPDATE SKIP LOCKED: cada instância pega cabeças diferentes.
//   - win: os primeiros window pendentes em ordem de gravação (leitura
//     limitada), numerados por agregado. Os eventos de um agregado que caem
//     na janela são sempre os primeiros dele (rn = 1 é a cabeça), então a
//     sequência é contínua. Uma só ordenação; nenhuma junção por
//     desigualdade (com 500 cabeças, a junção anterior fazia 2,5 milhões de
//     comparações).
//   - candidates: quem fica com a cabeça é dono do agregado nesta rodada e
//     leva também os eventos seguintes dele que estão na janela, até
//     perAggregate no total, parando no primeiro que não estiver pronto. O
//     filtro "agregado é meu" é um IN, resolvido por hash.
//     Nenhuma outra instância disputa esses eventos: elas só reivindicam
//     cabeças, e a cabeça está com o dono; quando ela é publicada, o
//     seguinte já está arrendado por ele.
//   - UPDATE: grava o arrendamento e conta a tentativa.
func (r outboxRepo) Claim(ctx context.Context, owner string, now time.Time, lease time.Duration, heads, perAggregate int) ([]app.OutboxMessage, error) {
	window := min(max(heads*perAggregate, heads), maxClaimWindow)
	rows, err := r.q.Query(ctx, `
		WITH ready AS (
			SELECT e.event_id, e.aggregate_id, e.seq
			  FROM outbox_events e
			 WHERE e.published_at IS NULL
			   AND e.next_attempt_at <= $1
			   AND (e.locked_until IS NULL OR e.locked_until < $1)
			   AND e.seq = (
			       SELECT p.seq FROM outbox_events p
			        WHERE p.aggregate_id = e.aggregate_id AND p.published_at IS NULL
			        ORDER BY p.seq
			        LIMIT 1)
			 ORDER BY e.seq
			 LIMIT $4
			 FOR UPDATE OF e SKIP LOCKED
		), win AS MATERIALIZED (
			SELECT event_id, aggregate_id,
			       row_number() OVER x AS rn,
			       bool_and(ok) OVER x AS contiguous
			  FROM (SELECT event_id, aggregate_id, seq,
			               next_attempt_at <= $1 AND (locked_until IS NULL OR locked_until < $1) AS ok
			          FROM outbox_events
			         WHERE published_at IS NULL
			         ORDER BY seq
			         LIMIT $6) pending
			WINDOW x AS (PARTITION BY aggregate_id ORDER BY seq ROWS UNBOUNDED PRECEDING)
		), candidates AS (
			SELECT event_id FROM win
			 WHERE rn BETWEEN 2 AND $5
			   AND contiguous
			   AND aggregate_id IN (SELECT aggregate_id FROM ready)
		), claim AS (
			SELECT event_id FROM ready
			UNION ALL
			SELECT event_id FROM candidates
		)
		UPDATE outbox_events o
		   SET locked_until = $1 + ($2 * interval '1 millisecond'),
		       locked_by = $3,
		       attempts = o.attempts + 1
		  FROM claim
		 WHERE o.event_id = claim.event_id
		RETURNING o.event_id, o.aggregate_type, o.aggregate_id, o.event_type, o.event_version,
		          o.correlation_id, o.payload, o.occurred_at, o.attempts, o.seq, o.trace_parent`,
		now, lease.Milliseconds(), owner, heads, perAggregate, window)
	if err != nil {
		return nil, classify(err)
	}
	defer rows.Close()

	type withSeq struct {
		msg app.OutboxMessage
		seq int64
	}
	var claimed []withSeq
	for rows.Next() {
		var w withSeq
		var parent *string
		m := &w.msg
		if err := rows.Scan(&m.EventID, &m.AggregateType, &m.AggregateID, &m.EventType, &m.EventVersion,
			&m.CorrelationID, &m.Payload, &m.OccurredAt, &m.Attempts, &w.seq, &parent); err != nil {
			return nil, classify(err)
		}
		if parent != nil {
			m.TraceParent = *parent
		}
		claimed = append(claimed, w)
	}
	if err := rows.Err(); err != nil {
		return nil, classify(err)
	}
	// RETURNING não garante ordem: publica na ordem de gravação
	slices.SortFunc(claimed, func(a, b withSeq) int { return cmp.Compare(a.seq, b.seq) })
	out := make([]app.OutboxMessage, len(claimed))
	for i, c := range claimed {
		out[i] = c.msg
	}
	return out, nil
}

// maxClaimWindow limita quantos pendentes uma rodada lê para achar os
// eventos seguintes de cada agregado.
const maxClaimWindow = 5000

// Release devolve eventos arrendados por owner, sem nova tentativa agendada.
// Eles não chegaram a ser tentados: a contagem do Claim é desfeita, para não
// inflar o backoff de uma falha futura.
func (r outboxRepo) Release(ctx context.Context, owner string, eventIDs []uuid.UUID) error {
	_, err := r.q.Exec(ctx, `
		UPDATE outbox_events
		   SET locked_until = NULL, locked_by = NULL, attempts = GREATEST(attempts - 1, 0)
		 WHERE event_id = ANY($1) AND locked_by = $2 AND published_at IS NULL`, eventIDs, owner)
	return classify(err)
}

func (r outboxRepo) MarkPublishedMany(ctx context.Context, eventIDs []uuid.UUID, now time.Time) error {
	_, err := r.q.Exec(ctx, `
		UPDATE outbox_events
		   SET published_at = $2, locked_until = NULL, locked_by = NULL, last_error = NULL
		 WHERE event_id = ANY($1) AND published_at IS NULL`, eventIDs, now)
	return classify(err)
}

func (r outboxRepo) MarkPublished(ctx context.Context, eventID uuid.UUID, now time.Time) error {
	_, err := r.q.Exec(ctx, `
		UPDATE outbox_events
		   SET published_at = $2, locked_until = NULL, locked_by = NULL, last_error = NULL
		 WHERE event_id = $1 AND published_at IS NULL`, eventID, now)
	return classify(err)
}

// Backlog usa o índice parcial dos não publicados (published_at IS NULL).
func (r outboxRepo) Backlog(ctx context.Context) (int64, *time.Time, error) {
	var (
		n      int64
		oldest *time.Time
	)
	err := r.q.QueryRow(ctx, `
		SELECT COUNT(*), MIN(occurred_at) FROM outbox_events WHERE published_at IS NULL`).Scan(&n, &oldest)
	if err != nil {
		return 0, nil, classify(err)
	}
	return n, oldest, nil
}

func (r outboxRepo) MarkFailed(ctx context.Context, eventID uuid.UUID, owner string, next time.Time, cause string) error {
	if len(cause) > 1000 {
		cause = cause[:1000]
	}
	_, err := r.q.Exec(ctx, `
		UPDATE outbox_events
		   SET locked_until = NULL, locked_by = NULL, next_attempt_at = $3, last_error = $4
		 WHERE event_id = $1 AND published_at IS NULL AND locked_by = $2`, eventID, owner, next, cause)
	return classify(err)
}
