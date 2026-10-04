package observability

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Taino-Edu/Desafio-go-arqui/internal/adapters/httpapi"
	"github.com/Taino-Edu/Desafio-go-arqui/internal/adapters/sqsconsumer"
	"github.com/Taino-Edu/Desafio-go-arqui/internal/app"
)

// o mesmo objeto atende às três portas
var (
	_ app.Metrics             = (*Prometheus)(nil)
	_ sqsconsumer.Metrics     = (*Prometheus)(nil)
	_ httpapi.RequestObserver = (*Prometheus)(nil)
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

// scrape faz uma coleta como o Prometheus faria.
func scrape(t *testing.T, p *Prometheus) string {
	t.Helper()
	rec := httptest.NewRecorder()
	p.Handler(quiet).ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	return rec.Body.String()
}

func requireLines(t *testing.T, text string, lines ...string) {
	t.Helper()
	for _, l := range lines {
		if !strings.Contains(text, "\n"+l+"\n") {
			t.Errorf("linha ausente: %s", l)
		}
	}
}

func TestPrometheus_BusinessMetrics(t *testing.T) {
	p := NewPrometheus()
	p.WagerOutcome(app.SourceHTTP, "BET", "PROCESSED")
	p.WagerOutcome(app.SourceHTTP, "BET", "PROCESSED")
	p.WagerOutcome(app.SourceSQS, "WIN", "PENDING_REFERENCE")
	p.IdempotentReplay(app.SourceHTTP)
	p.IdempotencyConflict(app.SourceSQS, app.ConflictKeyReused)
	p.TransientRetry(app.SourceHTTP)
	p.ConcurrencyConflict(app.SourceHTTP)
	p.ProcessingDuration(app.SourceHTTP, 30*time.Millisecond)
	p.ReferenceResolution(string(app.ReferenceExpired))
	p.OutboxPublished(3)
	p.OutboxFailed(1)
	p.QueueMessage(sqsconsumer.OutcomeDLQ)
	p.Reconciliation(true)
	p.Reconciliation(false)

	requireLines(t, scrape(t, p),
		`wager_transactions_total{kind="BET",source="http",status="PROCESSED"} 2`,
		`wager_transactions_total{kind="WIN",source="sqs",status="PENDING_REFERENCE"} 1`,
		`idempotent_replays_total{source="http"} 1`,
		`idempotency_conflicts_total{reason="key_reused",source="sqs"} 1`,
		`transient_retries_total{source="http"} 1`,
		`wallet_lock_conflicts_total{source="http"} 1`,
		`wager_processing_duration_seconds_bucket{source="http",le="0.025"} 0`,
		`wager_processing_duration_seconds_bucket{source="http",le="0.05"} 1`,
		`wager_processing_duration_seconds_count{source="http"} 1`,
		`reference_resolutions_total{outcome="EXPIRED"} 1`,
		`outbox_events_total{result="published"} 3`,
		`outbox_events_total{result="failed"} 1`,
		`sqs_messages_total{outcome="dlq"} 1`,
		`reconciliations_total{result="consistent"} 1`,
		`reconciliations_total{result="divergent"} 1`,
		`reconciliation_mismatch_total 1`,
	)
}

// Séries de rótulos fixos existem desde a partida, valendo zero.
func TestPrometheus_FixedSeriesStartAtZero(t *testing.T) {
	text := scrape(t, NewPrometheus())
	requireLines(t, text,
		`reconciliation_mismatch_total 0`,
		`reconciliations_total{result="divergent"} 0`,
		`outbox_events_total{result="published"} 0`,
		`reference_resolutions_total{outcome="RESCHEDULED"} 0`,
	)
	for _, runtime := range []string{"go_goroutines ", "process_open_fds "} {
		if !strings.Contains(text, "\n"+runtime) {
			t.Errorf("métrica de runtime ausente: %s", runtime)
		}
	}
}

func TestPrometheus_HTTPMethodIsNormalized(t *testing.T) {
	p := NewPrometheus()
	p.ObserveRequest("GET", "/wallets/{walletId}", 200, 5*time.Millisecond)
	p.ObserveRequest("BREW", httpapi.RouteUnmatched, 405, time.Millisecond)
	requireLines(t, scrape(t, p),
		`http_requests_total{method="GET",route="/wallets/{walletId}",status="200"} 1`,
		`http_requests_total{method="OTHER",route="unmatched",status="405"} 1`,
	)
}

func TestPrometheus_BacklogIsReadAtScrapeTime(t *testing.T) {
	p := NewPrometheus()
	oldest := time.Now().Add(-90 * time.Second)
	backlog := app.Backlog{OutboxPending: 3, OutboxOldestPending: &oldest, PendingReferences: 2}
	if err := p.RegisterBacklog(func(context.Context) (app.Backlog, error) { return backlog, nil }, time.Second); err != nil {
		t.Fatal(err)
	}
	text := scrape(t, p)
	requireLines(t, text, `outbox_pending_events 3`, `wager_pending_references 2`)
	if !strings.Contains(text, "\noutbox_lag_seconds 9") { // 90.x segundos
		t.Errorf("outbox_lag_seconds deveria ser ~90:\n%s", grep(text, "outbox_lag"))
	}

	// sem pendências, o atraso é zero
	backlog = app.Backlog{}
	requireLines(t, scrape(t, p), `outbox_pending_events 0`, `outbox_lag_seconds 0`)
}

// Banco fora durante a coleta: o /metrics continua respondendo com as outras
// métricas, e só as do banco somem.
func TestPrometheus_CollectorFailureDoesNotBreakTheScrape(t *testing.T) {
	p := NewPrometheus()
	_ = p.RegisterBacklog(func(context.Context) (app.Backlog, error) {
		return app.Backlog{}, errors.New("database down")
	}, time.Second)
	_ = p.RegisterQueueDepth(func(context.Context) (map[string]int64, error) {
		return map[string]int64{"input": 4, "dlq": 1}, nil
	}, time.Second)
	p.Reconciliation(false)

	text := scrape(t, p)
	requireLines(t, text, `reconciliation_mismatch_total 1`,
		`sqs_queue_messages{queue="dlq"} 1`, `sqs_queue_messages{queue="input"} 4`)
	if strings.Contains(text, "outbox_pending_events ") {
		t.Error("métrica do banco não deveria aparecer com o banco fora")
	}
}

func grep(text, substr string) string {
	var out []string
	for _, l := range strings.Split(text, "\n") {
		if strings.Contains(l, substr) {
			out = append(out, l)
		}
	}
	return strings.Join(out, "\n")
}
