package app

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"

	"github.com/Taino-Edu/Desafio-go-arqui/internal/domain/events"
)

// toOutboxRecords embrulha eventos de domínio em envelopes e os serializa.
// O eventId é gerado aqui, uma única vez, e gravado na outbox: toda
// republicação do mesmo registro usa o mesmo eventId.
func toOutboxRecords(ctx context.Context, ids IDGenerator, causation *uuid.UUID, evs ...events.Event) ([]OutboxRecord, error) {
	correlation := CorrelationID(ctx)
	records := make([]OutboxRecord, 0, len(evs))
	for _, ev := range evs {
		eventID, err := ids.NewID()
		if err != nil {
			return nil, fmt.Errorf("event id: %w", err)
		}
		env := events.NewEnvelope(eventID, correlation, causation, ev)
		payload, err := json.Marshal(env)
		if err != nil {
			return nil, fmt.Errorf("marshal %s: %w", ev.EventType(), err)
		}
		records = append(records, OutboxRecord{
			EventID:       eventID,
			AggregateType: aggregateType(ev),
			AggregateID:   ev.AggregateID(),
			EventType:     ev.EventType(),
			EventVersion:  ev.EventVersion(),
			CorrelationID: correlation,
			CausationID:   causation,
			Payload:       payload,
			OccurredAt:    ev.OccurredAt(),
		})
	}
	return records, nil
}

func aggregateType(ev events.Event) string {
	if ev.EventType() == events.TypeWalletBalanceChanged {
		return "wallet"
	}
	return "wager_transaction"
}
