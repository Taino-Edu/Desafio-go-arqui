package events_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Taino-Edu/Desafio-go-arqui/internal/domain/events"
	"github.com/Taino-Edu/Desafio-go-arqui/internal/domain/money"
)

func TestEnvelope_WalletBalanceChanged(t *testing.T) {
	walletID := uuid.MustParse("0192f291-27dd-7d3f-8071-5f8685deef37")
	txID := uuid.MustParse("0192f298-345e-7e38-af88-e43f851a819d")
	eventID := uuid.MustParse("0192f2a0-0000-7000-8000-000000000001")
	m := func(s string) money.Money { v, _ := money.Parse(s, money.BRL); return v }
	at := time.Date(2026, 9, 8, 9, 0, 0, 0, time.FixedZone("BRT", -3*3600))

	ev := events.NewWalletBalanceChanged(walletID, txID, "DEBIT", m("25.00"), m("1000.00"), m("975.00"), 2, at)
	env := events.NewEnvelope(eventID, "corr-1", &txID, ev)

	if env.EventType != "WalletBalanceChanged" || env.Version != 1 || env.AggregateID != walletID {
		t.Errorf("envelope = %+v", env)
	}
	if env.OccurredAt.Location() != time.UTC || !env.OccurredAt.Equal(at) {
		t.Errorf("occurredAt deve ser UTC: %v", env.OccurredAt)
	}

	b, err := json.Marshal(env)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"eventId":"0192f2a0-0000-7000-8000-000000000001","eventType":"WalletBalanceChanged",` +
		`"aggregateId":"0192f291-27dd-7d3f-8071-5f8685deef37","correlationId":"corr-1",` +
		`"causationId":"0192f298-345e-7e38-af88-e43f851a819d","occurredAt":"2026-09-08T12:00:00Z","version":1,` +
		`"data":{"walletId":"0192f291-27dd-7d3f-8071-5f8685deef37","transactionId":"0192f298-345e-7e38-af88-e43f851a819d",` +
		`"direction":"DEBIT","money":{"amount":"25.00","currency":"BRL"},` +
		`"balanceBefore":{"amount":"1000.00","currency":"BRL"},"balanceAfter":{"amount":"975.00","currency":"BRL"},` +
		`"walletVersion":2}}`
	if string(b) != want {
		t.Errorf("JSON:\n got %s\nwant %s", b, want)
	}
}

func TestEventSnapshotIsIndependent(t *testing.T) {
	ext := &events.External{ProviderID: "provider-a", ExternalTransactionID: "tx-1"}
	m, _ := money.Parse("1.00", money.BRL)
	ev := events.NewWagerTransactionRejected(uuid.New(), uuid.New(), uuid.New(), "BET", m, "INSUFFICIENT_FUNDS", ext, time.Now())

	ext.ProviderID = "alterado"
	if ev.External.ProviderID != "provider-a" {
		t.Error("o evento deve guardar uma cópia, não o ponteiro original")
	}
	if ev.EventType() != events.TypeWagerTransactionRejected || ev.EventVersion() != 1 {
		t.Errorf("tipo/versão = %s/%d", ev.EventType(), ev.EventVersion())
	}
}
