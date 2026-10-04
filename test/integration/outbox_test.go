//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Taino-Edu/Desafio-go-arqui/internal/adapters/postgres"
	"github.com/Taino-Edu/Desafio-go-arqui/internal/adapters/sqsconsumer"
	"github.com/Taino-Edu/Desafio-go-arqui/internal/app"
	"github.com/Taino-Edu/Desafio-go-arqui/internal/platform/config"
	"github.com/Taino-Edu/Desafio-go-arqui/internal/testsupport/apptest"
	"github.com/Taino-Edu/Desafio-go-arqui/internal/testsupport/pgtest"
	"github.com/Taino-Edu/Desafio-go-arqui/internal/testsupport/sqstest"
)

// withOutbox liga o publicador da aplicação na fila do teste.
func withOutbox(q *sqstest.Queues) func(*config.Config) {
	return func(c *config.Config) {
		cc := sqstest.ClientConfig()
		c.SQS.Region, c.SQS.Endpoint, c.SQS.AccessKeyID, c.SQS.SecretAccessKey = cc.Region, cc.Endpoint, cc.AccessKeyID, cc.SecretAccessKey
		c.Outbox = config.Outbox{
			Enabled: true, Queue: q.URL, BatchSize: 50, PollInterval: 50 * time.Millisecond,
			Lease: 5 * time.Second, RetryBaseDelay: 100 * time.Millisecond, RetryMaxDelay: time.Second,
		}
	}
}

// recorder envolve um publicador e registra cada envio (eventId), podendo
// falhar as primeiras N tentativas.
type recorder struct {
	inner    app.EventPublisher
	mu       sync.Mutex
	sent     []uuid.UUID
	failNext atomic.Int32
}

func (r *recorder) Publish(ctx context.Context, m app.OutboxMessage) error {
	if r.failNext.Load() > 0 {
		r.failNext.Add(-1)
		return errors.New("sqs indisponível (simulado)")
	}
	if err := r.inner.Publish(ctx, m); err != nil {
		return err
	}
	r.mu.Lock()
	r.sent = append(r.sent, m.EventID)
	r.mu.Unlock()
	return nil
}

func (r *recorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.sent)
}

func newRecorder(t *testing.T, q *sqstest.Queues) *recorder {
	client, err := sqsconsumer.NewClient(context.Background(), sqstest.ClientConfig())
	if err != nil {
		t.Fatal(err)
	}
	return &recorder{inner: sqsconsumer.Publisher{API: client, QueueURL: func() string { return q.URL }}}
}

// directOutbox monta um publicador independente (como outra instância).
func directOutbox(t *testing.T, db *pgtest.DB, pub app.EventPublisher, owner string, lease time.Duration, hooks app.OutboxHooks) *app.OutboxService {
	pool, err := postgres.NewPool(postgres.PoolConfig{URL: db.AppURL, MaxConns: 4})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return app.NewOutboxService(postgres.NewStore(pool), pub, app.SystemClock{}, app.OutboxConfig{
		Owner: owner, BatchSize: 50, Lease: lease, RetryBase: 300 * time.Millisecond, RetryMax: time.Second,
	}, hooks)
}

// insertEvent grava um evento na outbox, como uma transação de negócio faria.
func insertEvent(t *testing.T, db *pgtest.DB, aggregate uuid.UUID, eventType string) uuid.UUID {
	id := uuid.New()
	payload := fmt.Sprintf(`{"eventId":%q,"eventType":%q,"aggregateId":%q}`, id, eventType, aggregate)
	if _, err := db.App.Exec(context.Background(), `
		INSERT INTO outbox_events (event_id, aggregate_type, aggregate_id, event_type, event_version,
			correlation_id, payload, occurred_at, next_attempt_at)
		VALUES ($1, 'wallet', $2, $3, 1, 'test', $4, now(), now())`, id, aggregate, eventType, payload); err != nil {
		t.Fatal(err)
	}
	return id
}

func unpublished(t *testing.T, db *pgtest.DB) int {
	return apptest.Count(t, db, `SELECT count(*) FROM outbox_events WHERE published_at IS NULL`)
}

// ------------------------------------------------------------------

// Fluxo completo: as operações gravam eventos no commit e o publicador da
// aplicação os entrega com o contrato de roteamento.
func TestOutbox_PublishesCommittedEvents(t *testing.T) {
	db := pgtest.New(t)
	q := sqstest.New(t, 30*time.Second, 5)
	a, _ := apptest.StartOn(t, db, withOutbox(q))
	player := uuid.NewString()
	wallet := a.Client.OpenWallet(player, "100.00")
	apptest.Must(t, a.Client.Submit(apptest.Op{Provider: "provider-a", ExternalID: "bet-1", PlayerID: player,
		WalletID: wallet, Round: "r", Game: "g", Kind: "BET", Amount: "25.00"}), http.StatusCreated, "bet")

	msgs := q.ReadQueue(4, 10*time.Second)
	if len(msgs) != 4 {
		t.Fatalf("eventos na fila = %d, want 4 (2 da abertura, 2 da aposta)", len(msgs))
	}
	types := map[string]int{}
	for _, m := range msgs {
		var env map[string]any
		if err := json.Unmarshal([]byte(m.Body), &env); err != nil {
			t.Fatal(err)
		}
		types[env["eventType"].(string)]++
		if m.DedupID != env["eventId"] || m.GroupID != env["aggregateId"] || m.Attributes["eventType"] != env["eventType"] {
			t.Errorf("roteamento: dedup=%s group=%s attrs=%v env=%v", m.DedupID, m.GroupID, m.Attributes, env)
		}
		if env["eventType"] == "WalletBalanceChanged" {
			data := env["data"].(map[string]any)
			if data["walletVersion"] == nil || data["balanceAfter"].(map[string]any)["amount"] == nil {
				t.Errorf("payload = %v", data)
			}
		}
	}
	if types["WagerTransactionProcessed"] != 2 || types["WalletBalanceChanged"] != 2 {
		t.Errorf("tipos = %v", types)
	}
	deadline := time.Now().Add(5 * time.Second)
	for unpublished(t, db) > 0 && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if n := apptest.Count(t, db, `SELECT count(*) FROM outbox_events WHERE published_at IS NOT NULL AND locked_by IS NULL`); n != 4 {
		t.Errorf("publicados e liberados = %d", n)
	}
	// tudo publicado: nada pendente e atraso zero
	a.Client.WaitMetric(`outbox_events_total{result="published"}`, 4, 5*time.Second)
	a.Client.WaitMetric(`outbox_pending_events`, 0, time.Second)
	a.Client.WaitMetric(`outbox_lag_seconds`, 0, time.Second)
}

// Evento de uma transação ainda aberta não existe para o publicador.
func TestOutbox_UncommittedEventsAreNotPublished(t *testing.T) {
	db := pgtest.New(t)
	q := sqstest.New(t, 30*time.Second, 5)
	rec := newRecorder(t, q)
	svc := directOutbox(t, db, rec, "p1", 5*time.Second, app.OutboxHooks{})
	ctx := context.Background()

	tx, err := db.App.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO outbox_events (event_id, aggregate_type, aggregate_id, event_type,
		event_version, correlation_id, payload, occurred_at, next_attempt_at)
		VALUES ($1, 'wallet', $2, 'WalletBalanceChanged', 1, 'c', '{}', now(), now())`, uuid.New(), uuid.New()); err != nil {
		t.Fatal(err)
	}
	if res, _ := svc.PublishBatch(ctx); res.Claimed != 0 || rec.count() != 0 {
		t.Fatalf("publicou antes do commit: %+v", res)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if res, err := svc.PublishBatch(ctx); err != nil || res.Published != 1 {
		t.Fatalf("depois do commit: %+v %v", res, err)
	}
}

// Vários publicadores disputando a mesma outbox: cada evento é publicado
// uma vez e a ordem por carteira (walletVersion) é preservada.
func TestOutbox_CompetingPublishersKeepOrderPerAggregate(t *testing.T) {
	db := pgtest.New(t)
	q := sqstest.New(t, 30*time.Second, 5)
	setup, stop := apptest.StartOn(t, db, nil) // gera os eventos sem publicar
	const wallets, bets = 5, 8
	walletIDs := make([]string, wallets)
	for i := range walletIDs {
		p := uuid.NewString()
		walletIDs[i] = setup.Client.OpenWallet(p, "100.00")
		for b := 0; b < bets; b++ {
			apptest.Must(t, setup.Client.Submit(apptest.Op{Provider: "provider-a", ExternalID: fmt.Sprintf("%d-%d", i, b),
				PlayerID: p, WalletID: walletIDs[i], Round: "r", Game: "g", Kind: "BET", Amount: "1.00"}), 201, "bet")
		}
	}
	stop()
	total := apptest.Count(t, db, `SELECT count(*) FROM outbox_events`)
	if total != wallets*(bets+1)*2 {
		t.Fatalf("eventos = %d", total)
	}

	rec := newRecorder(t, q)
	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		svc := directOutbox(t, db, rec, fmt.Sprintf("publisher-%d", i), 30*time.Second, app.OutboxHooks{})
		wg.Add(1)
		go func() {
			defer wg.Done()
			deadline := time.Now().Add(20 * time.Second)
			for unpublished(t, db) > 0 && time.Now().Before(deadline) {
				if _, err := svc.PublishBatch(context.Background()); err != nil {
					t.Errorf("publish: %v", err)
					return
				}
			}
		}()
	}
	wg.Wait()

	seen := map[uuid.UUID]int{}
	for _, id := range rec.sent {
		seen[id]++
	}
	if len(rec.sent) != total || len(seen) != total {
		t.Errorf("envios = %d, eventos distintos = %d, want %d", len(rec.sent), len(seen), total)
	}

	// ordem de entrega por carteira: walletVersion 1, 2, 3...
	versions := map[string][]float64{}
	for _, m := range q.ReadQueue(total, 20*time.Second) {
		var env struct {
			EventType string `json:"eventType"`
			Data      struct {
				WalletID      string  `json:"walletId"`
				WalletVersion float64 `json:"walletVersion"`
			} `json:"data"`
		}
		_ = json.Unmarshal([]byte(m.Body), &env)
		if env.EventType == "WalletBalanceChanged" {
			versions[env.Data.WalletID] = append(versions[env.Data.WalletID], env.Data.WalletVersion)
		}
	}
	for _, w := range walletIDs {
		got := versions[w]
		if len(got) != bets+1 {
			t.Errorf("carteira %s: %d eventos de saldo", w, len(got))
			continue
		}
		for i, v := range got {
			if int(v) != i+1 {
				t.Errorf("carteira %s fora de ordem: %v", w, got)
				break
			}
		}
	}
}

// Queda entre o commit e a publicação: o publicador reivindicou e caiu. O
// arrendamento vence e outra instância publica.
func TestOutbox_RecoversAbandonedClaim(t *testing.T) {
	db := pgtest.New(t)
	q := sqstest.New(t, 30*time.Second, 5)
	rec := newRecorder(t, q)
	id := insertEvent(t, db, uuid.New(), "WalletBalanceChanged")

	crashing := directOutbox(t, db, rec, "dies-after-claim", time.Second, app.OutboxHooks{
		AfterClaim: func() error { return app.ErrSimulatedCrash },
	})
	if res, _ := crashing.PublishBatch(context.Background()); res.Claimed != 1 || res.Published != 0 {
		t.Fatalf("primeira instância: %+v", res)
	}

	healthy := directOutbox(t, db, rec, "healthy", time.Second, app.OutboxHooks{})
	if res, _ := healthy.PublishBatch(context.Background()); res.Claimed != 0 {
		t.Fatal("o arrendamento ainda vale: ninguém mais pode pegar o evento")
	}
	time.Sleep(1200 * time.Millisecond)
	if res, err := healthy.PublishBatch(context.Background()); err != nil || res.Published != 1 {
		t.Fatalf("após o arrendamento vencer: %+v %v", res, err)
	}
	attempts := apptest.Count(t, db, `SELECT attempts FROM outbox_events WHERE event_id = $1`, id)
	if attempts != 2 || unpublished(t, db) != 0 {
		t.Errorf("tentativas = %d, pendentes = %d", attempts, unpublished(t, db))
	}
}

// Queda entre a publicação e a confirmação: o evento sai, mas não é marcado.
// Outra instância o republica com o MESMO eventId.
func TestOutbox_CrashBetweenPublishAndMarkRepublishesSameEventID(t *testing.T) {
	db := pgtest.New(t)
	q := sqstest.New(t, 30*time.Second, 5)
	rec := newRecorder(t, q)
	id := insertEvent(t, db, uuid.New(), "WalletBalanceChanged")

	crashing := directOutbox(t, db, rec, "dies-after-publish", time.Second, app.OutboxHooks{
		AfterPublish: func(app.OutboxMessage) error { return app.ErrSimulatedCrash },
	})
	_, _ = crashing.PublishBatch(context.Background())
	if rec.count() != 1 || unpublished(t, db) != 1 {
		t.Fatalf("publicou %d e deixou %d pendente", rec.count(), unpublished(t, db))
	}

	time.Sleep(1200 * time.Millisecond)
	healthy := directOutbox(t, db, rec, "healthy", time.Second, app.OutboxHooks{})
	if res, err := healthy.PublishBatch(context.Background()); err != nil || res.Published != 1 {
		t.Fatalf("republicação: %+v %v", res, err)
	}
	if len(rec.sent) != 2 || rec.sent[0] != id || rec.sent[1] != id {
		t.Errorf("envios = %v, want o mesmo eventId duas vezes", rec.sent)
	}
	// na fila, a deduplicação do SQS FIFO (pelo eventId) deixou uma só cópia
	msgs := q.ReadQueue(2, 3*time.Second)
	if len(msgs) != 1 || msgs[0].DedupID != id.String() {
		t.Errorf("mensagens na fila = %+v", msgs)
	}
}

// Falha de publicação: backoff, erro registrado, e o evento seguinte do mesmo
// agregado espera (sem furar a ordem).
func TestOutbox_FailureBacksOffAndKeepsOrder(t *testing.T) {
	db := pgtest.New(t)
	q := sqstest.New(t, 30*time.Second, 5)
	rec := newRecorder(t, q)
	agg := uuid.New()
	first := insertEvent(t, db, agg, "WalletBalanceChanged")
	second := insertEvent(t, db, agg, "WalletBalanceChanged")
	svc := directOutbox(t, db, rec, "p1", 5*time.Second, app.OutboxHooks{})
	ctx := context.Background()

	rec.failNext.Store(2)
	res, _ := svc.PublishBatch(ctx)
	if res.Claimed != 1 || res.Failed != 1 {
		t.Fatalf("1ª rodada: %+v (só a cabeça do agregado pode ser reivindicada)", res)
	}
	var lastError string
	var lockedBy *string
	if err := db.Owner.QueryRow(ctx, `SELECT last_error, locked_by FROM outbox_events WHERE event_id = $1`, first).
		Scan(&lastError, &lockedBy); err != nil {
		t.Fatal(err)
	}
	if lastError == "" || lockedBy != nil {
		t.Errorf("falha registrada: last_error=%q locked_by=%v", lastError, lockedBy)
	}
	if res, _ := svc.PublishBatch(ctx); res.Claimed != 0 {
		t.Error("antes do backoff vencer, nada deve ser reivindicado (nem o segundo evento)")
	}

	deadline := time.Now().Add(10 * time.Second)
	for unpublished(t, db) > 0 && time.Now().Before(deadline) {
		_, _ = svc.PublishBatch(ctx)
		time.Sleep(50 * time.Millisecond)
	}
	if len(rec.sent) != 2 || rec.sent[0] != first || rec.sent[1] != second {
		t.Errorf("ordem de publicação = %v, want [%s %s]", rec.sent, first, second)
	}
	if n := apptest.Count(t, db, `SELECT attempts FROM outbox_events WHERE event_id = $1`, first); n != 3 {
		t.Errorf("tentativas do primeiro = %d, want 3", n)
	}
}
