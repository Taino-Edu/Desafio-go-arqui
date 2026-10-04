package postgres

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/Taino-Edu/Desafio-go-arqui/internal/app"
)

// Claim reivindica eventos para publicação.
//
//   - heads: o evento pendente mais antigo (menor seq) de cada agregado. Só
//     ele pode ser publicado; o seguinte espera, o que mantém a ordem por
//     agregado mesmo com várias instâncias publicando.
//   - ready: entre as cabeças, as que já podem ser tentadas (next_attempt_at
//     vencido) e não estão arrendadas por ninguém (ou o arrendamento venceu:
//     trabalho abandonado). FOR UPDATE SKIP LOCKED: cada instância pega
//     linhas diferentes. published_at IS NULL é conferido de novo aqui
//     porque, em READ COMMITTED, a linha é reavaliada na versão mais recente.
//   - UPDATE: grava o arrendamento e conta a tentativa.
func (r outboxRepo) Claim(ctx context.Context, owner string, now time.Time, lease time.Duration, limit int) ([]app.OutboxMessage, error) {
	rows, err := r.q.Query(ctx, `
		WITH heads AS (
			SELECT DISTINCT ON (aggregate_id) event_id
			  FROM outbox_events
			 WHERE published_at IS NULL
			 ORDER BY aggregate_id, seq
		), ready AS (
			SELECT e.event_id
			  FROM outbox_events e
			  JOIN heads h ON h.event_id = e.event_id
			 WHERE e.published_at IS NULL
			   AND e.next_attempt_at <= $1
			   AND (e.locked_until IS NULL OR e.locked_until < $1)
			 ORDER BY e.seq
			 LIMIT $4
			 FOR UPDATE OF e SKIP LOCKED
		)
		UPDATE outbox_events o
		   SET locked_until = $1 + ($2 * interval '1 millisecond'),
		       locked_by = $3,
		       attempts = o.attempts + 1
		  FROM ready
		 WHERE o.event_id = ready.event_id
		RETURNING o.event_id, o.aggregate_type, o.aggregate_id, o.event_type, o.event_version,
		          o.correlation_id, o.payload, o.occurred_at, o.attempts, o.seq`,
		now, lease.Milliseconds(), owner, limit)
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
		m := &w.msg
		if err := rows.Scan(&m.EventID, &m.AggregateType, &m.AggregateID, &m.EventType, &m.EventVersion,
			&m.CorrelationID, &m.Payload, &m.OccurredAt, &m.Attempts, &w.seq); err != nil {
			return nil, classify(err)
		}
		claimed = append(claimed, w)
	}
	if err := rows.Err(); err != nil {
		return nil, classify(err)
	}
	// RETURNING não garante ordem: publica na ordem de gravação
	for i := 1; i < len(claimed); i++ {
		for j := i; j > 0 && claimed[j].seq < claimed[j-1].seq; j-- {
			claimed[j], claimed[j-1] = claimed[j-1], claimed[j]
		}
	}
	out := make([]app.OutboxMessage, len(claimed))
	for i, c := range claimed {
		out[i] = c.msg
	}
	return out, nil
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
