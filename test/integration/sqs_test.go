//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Taino-Edu/Desafio-go-arqui/internal/adapters/postgres"
	"github.com/Taino-Edu/Desafio-go-arqui/internal/adapters/sqsconsumer"
	"github.com/Taino-Edu/Desafio-go-arqui/internal/app"
	"github.com/Taino-Edu/Desafio-go-arqui/internal/domain/wagering"
	"github.com/Taino-Edu/Desafio-go-arqui/internal/platform/config"
	"github.com/Taino-Edu/Desafio-go-arqui/internal/testsupport/apptest"
	"github.com/Taino-Edu/Desafio-go-arqui/internal/testsupport/pgtest"
	"github.com/Taino-Edu/Desafio-go-arqui/internal/testsupport/sqstest"
)

const consumerName = "wager-transactions-consumer"

// withSQS liga o consumidor da aplicação nas filas do teste.
func withSQS(q *sqstest.Queues) func(*config.Config) {
	return func(c *config.Config) {
		cc := sqstest.ClientConfig()
		c.SQS = config.SQS{
			Enabled: true, Region: cc.Region, Endpoint: cc.Endpoint,
			AccessKeyID: cc.AccessKeyID, SecretAccessKey: cc.SecretAccessKey,
			InputQueue: q.URL, DLQ: q.DLQURL, ConsumerName: consumerName,
			Pollers: 2, MaxMessages: 10, WaitTime: time.Second,
			RetryBaseDelay: time.Second, RetryMaxDelay: 2 * time.Second,
			AllowedProviders: []string{"provider-a", "provider-b"},
		}
	}
}

func data(o apptest.Op, key string) map[string]any {
	d := o.Body()
	if key == "" {
		key = o.Provider + ":" + o.ExternalID
	}
	d["idempotencyKey"] = key
	return d
}

type sqsFixture struct {
	t              *testing.T
	db             *pgtest.DB
	q              *sqstest.Queues
	app            *apptest.App
	player, wallet string
}

func newSQSFixture(t *testing.T, balance string, visibility time.Duration, maxReceive int, consumerOn bool) *sqsFixture {
	db := pgtest.New(t)
	q := sqstest.New(t, visibility, maxReceive)
	mutate := withSQS(q)
	if !consumerOn {
		mutate = nil
	}
	a, _ := apptest.StartOn(t, db, mutate)
	player := uuid.NewString()
	return &sqsFixture{t: t, db: db, q: q, app: a, player: player, wallet: a.Client.OpenWallet(player, balance)}
}

func (f *sqsFixture) bet(ext, amount string) apptest.Op {
	return apptest.Op{Provider: "provider-a", ExternalID: ext, PlayerID: f.player, WalletID: f.wallet,
		Round: "r", Game: "g", Kind: "BET", Amount: amount}
}

func (f *sqsFixture) send(messageID string, o apptest.Op) {
	f.q.Send(sqstest.Message(messageID, data(o, "")), f.wallet, sqstest.Unique("dedup"))
}

func (f *sqsFixture) debits() int {
	return apptest.Count(f.t, f.db, `SELECT count(*) FROM wallet_ledger_entries WHERE wallet_id = $1 AND direction = 'DEBIT'`, f.wallet)
}

func (f *sqsFixture) balance() string {
	return f.app.Client.Do("GET", "/wallets/"+f.wallet, nil).Amount("balance")
}

// ------------------------------------------------------------------

func TestSQS_ProcessesAndDeletesAfterCommit(t *testing.T) {
	f := newSQSFixture(t, "100.00", 5*time.Second, 5, true)
	f.send("msg-1", f.bet("bet-1", "25.00"))

	r := f.app.Client.WaitStatus("provider-a", "bet-1", "PROCESSED", 10*time.Second)
	f.q.WaitEmpty(5 * time.Second)
	if r.Amount("balanceAfter") != "75.00" || f.debits() != 1 {
		t.Errorf("tx = %v, débitos %d", r.Body, f.debits())
	}
	if n := apptest.Count(t, f.db, `SELECT count(*) FROM inbox_messages WHERE consumer_name = $1 AND message_id = 'msg-1' AND completed_at IS NOT NULL`, consumerName); n != 1 {
		t.Errorf("inbox concluída = %d", n)
	}
	if f.q.Depth(true) != 0 {
		t.Error("nada deveria ir para a DLQ")
	}
	// com o consumidor ligado, o readiness confere também o SQS
	if r := f.app.Client.Do("GET", "/health/ready", nil); !strings.Contains(fmtBody(r), `"sqs":"ok"`) {
		t.Errorf("ready = %v", r.Body)
	}
	// o correlationId do evento é o messageId da fila
	if n := apptest.Count(t, f.db, `SELECT count(*) FROM outbox_events WHERE correlation_id = 'msg-1'`); n != 2 {
		t.Errorf("eventos com correlationId = %d", n)
	}
	// métricas: desfecho da mensagem, da operação, e as filas vazias
	c := f.app.Client
	c.WaitMetric(`sqs_messages_total{outcome="processed"}`, 1, 5*time.Second)
	c.WaitMetric(`wager_transactions_total{kind="BET",source="sqs",status="PROCESSED"}`, 1, time.Second)
	c.WaitMetric(`sqs_queue_messages{queue="input"}`, 0, 5*time.Second)
	c.WaitMetric(`sqs_queue_messages{queue="dlq"}`, 0, time.Second)
}

// Reentrega da mesma mensagem (inbox) e mesma operação em outra mensagem
// (idempotência por chave): um único débito.
func TestSQS_DuplicatesAreDeduplicated(t *testing.T) {
	f := newSQSFixture(t, "100.00", 5*time.Second, 5, true)
	bet := f.bet("bet-1", "10.00")

	f.send("msg-1", bet)
	f.send("msg-1", bet) // mesmo messageId, reenviado pelo produtor
	f.send("msg-2", bet) // outra mensagem com a MESMA operação (mesma chave)

	f.app.Client.WaitStatus("provider-a", "bet-1", "PROCESSED", 10*time.Second)
	f.q.WaitEmpty(10 * time.Second)
	if f.debits() != 1 || f.balance() != "90.00" {
		t.Errorf("débitos %d, saldo %s", f.debits(), f.balance())
	}
	if n := apptest.Count(t, f.db, `SELECT count(*) FROM inbox_messages WHERE completed_at IS NOT NULL`); n != 2 {
		t.Errorf("inbox = %d (msg-1 e msg-2)", n)
	}
	if f.q.Depth(true) != 0 {
		t.Error("duplicatas não são erro: nada na DLQ")
	}
	// as duas formas de duplicata aparecem separadas nas métricas: a
	// reentrega (inbox) e a mesma operação em outra mensagem (replay)
	c := f.app.Client
	c.WaitMetric(`sqs_messages_total{outcome="duplicate"}`, 1, 5*time.Second)
	c.WaitMetric(`sqs_messages_total{outcome="processed"}`, 2, 5*time.Second)
	c.WaitMetric(`idempotent_replays_total{source="sqs"}`, 1, time.Second)
	c.WaitMetric(`wager_transactions_total{kind="BET",source="sqs",status="PROCESSED"}`, 1, time.Second)
}

// A mesma operação pelas duas portas: HTTP e SQS compartilham a idempotência.
func TestSQS_SameOperationViaHTTPAndSQS(t *testing.T) {
	f := newSQSFixture(t, "100.00", 5*time.Second, 5, true)

	t.Run("HTTP primeiro, depois SQS", func(t *testing.T) {
		apptest.Must(t, f.app.Client.Submit(f.bet("bet-h", "10.00")), http.StatusCreated, "http")
		f.send("msg-h", f.bet("bet-h", "10.00"))
		f.q.WaitEmpty(10 * time.Second)
	})
	t.Run("SQS primeiro, depois HTTP", func(t *testing.T) {
		f.send("msg-s", f.bet("bet-s", "10.00"))
		f.app.Client.WaitStatus("provider-a", "bet-s", "PROCESSED", 10*time.Second)
		r := apptest.Must(t, f.app.Client.Submit(f.bet("bet-s", "10.00")), http.StatusOK, "http replay")
		if r.Body["idempotentReplay"] != true {
			t.Errorf("= %v", r.Body)
		}
	})
	t.Run("ao mesmo tempo", func(t *testing.T) {
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); f.send("msg-c", f.bet("bet-c", "10.00")) }()
		go func() { defer wg.Done(); f.app.Client.Submit(f.bet("bet-c", "10.00")) }()
		wg.Wait()
		f.q.WaitEmpty(10 * time.Second)
	})
	if f.debits() != 3 || f.balance() != "70.00" {
		t.Errorf("débitos %d (esperado 3, um por operação), saldo %s", f.debits(), f.balance())
	}
	apptest.AssertReconciled(t, f.db)
}

func TestSQS_BusinessRejectionIsTerminal(t *testing.T) {
	f := newSQSFixture(t, "10.00", 5*time.Second, 5, true)
	f.send("msg-1", f.bet("bet-big", "500.00"))
	r := f.app.Client.WaitStatus("provider-a", "bet-big", "REJECTED", 10*time.Second)
	f.q.WaitEmpty(5 * time.Second)
	if r.Str("failureCode") != "INSUFFICIENT_FUNDS" || f.q.Depth(true) != 0 {
		t.Errorf("tx = %v, dlq %d", r.Body, f.q.Depth(true))
	}
}

func TestSQS_InvalidMessagesGoToDLQ(t *testing.T) {
	f := newSQSFixture(t, "100.00", 5*time.Second, 5, true)
	loss := f.bet("loss-1", "1.00")
	loss.Kind = "LOSS"
	opening := f.bet("open-1", "1.00")
	opening.Kind = "OPENING"
	stranger := f.bet("x-1", "1.00")
	stranger.Provider = "provider-x"

	f.q.Send(`{"messageId": "quebrado"`, f.wallet, "d1")
	f.send("msg-loss", loss)
	f.send("msg-opening", opening)
	f.send("msg-stranger", stranger)

	got := f.q.ReadDLQ(4, 15*time.Second)
	f.q.WaitEmpty(5 * time.Second)
	if len(got) != 4 {
		t.Fatalf("DLQ recebeu %d mensagens: %+v", len(got), got)
	}
	reasons := ""
	for _, m := range got {
		if m.Reason == "" {
			t.Errorf("mensagem sem motivo na DLQ: %s", m.Body)
		}
		reasons += m.Reason + "\n"
	}
	for _, want := range []string{"malformed JSON", "LOSS requires", "OPENING is internal", "not allowed"} {
		if !strings.Contains(reasons, want) {
			t.Errorf("motivo %q ausente em:\n%s", want, reasons)
		}
	}
	if n := apptest.Count(t, f.db, `SELECT count(*) FROM wager_transactions WHERE origin = 'EXTERNAL'`); n != 0 {
		t.Errorf("mensagens inválidas gravaram %d transações", n)
	}
	f.app.Client.WaitMetric(`sqs_messages_total{outcome="dlq"}`, 4, 5*time.Second)
}

func TestSQS_SameMessageIDWithDifferentContent(t *testing.T) {
	f := newSQSFixture(t, "100.00", 5*time.Second, 5, true)
	f.send("msg-1", f.bet("bet-1", "10.00"))
	f.app.Client.WaitStatus("provider-a", "bet-1", "PROCESSED", 10*time.Second)
	f.send("msg-1", f.bet("bet-1", "99.00")) // mesmo messageId, outro conteúdo

	got := f.q.ReadDLQ(1, 10*time.Second)
	if len(got) != 1 || !strings.Contains(got[0].Reason, "different payload") {
		t.Fatalf("DLQ = %+v", got)
	}
	if f.debits() != 1 || f.balance() != "90.00" {
		t.Errorf("débitos %d, saldo %s", f.debits(), f.balance())
	}
}

// ------------------------------------------------------------------
// Consumidor montado diretamente, com ganchos de falha.

func directConsumer(t *testing.T, db *pgtest.DB, q *sqstest.Queues, hooks sqsconsumer.Hooks) *sqsconsumer.Consumer {
	pool, err := postgres.NewPool(postgres.PoolConfig{URL: db.AppURL, MaxConns: 5, LockTimeout: 2 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	svc := app.NewWagerService(postgres.NewStore(pool), app.SystemClock{}, app.UUIDv7{}, wagering.DefaultReferenceRetryPolicy)
	client, err := sqsconsumer.NewClient(context.Background(), sqstest.ClientConfig())
	if err != nil {
		t.Fatal(err)
	}
	c := sqsconsumer.New(sqsconsumer.Config{
		ConsumerName: consumerName, QueueURL: q.URL, DLQURL: q.DLQURL, Pollers: 1, MaxMessages: 10,
		WaitTime: time.Second, ItemTimeout: 5 * time.Second, RetryBaseDelay: time.Second, RetryMaxDelay: 2 * time.Second,
	}, client, svc, slog.New(slog.NewJSONHandler(io.Discard, nil)), hooks)
	t.Cleanup(func() { _ = c.Stop(context.Background()) })
	return c
}

// Teste obrigatório 5: o consumidor cai DEPOIS do commit e ANTES de apagar a
// mensagem. A mensagem reaparece, outro consumidor a recebe, a inbox
// reconhece e ela é apenas removida: um único débito.
func TestSQS_CrashAfterCommitBeforeDelete(t *testing.T) {
	f := newSQSFixture(t, "100.00", 2*time.Second, 5, false)

	var crashed atomic.Bool
	first := directConsumer(t, f.db, f.q, sqsconsumer.Hooks{AfterCommit: func(string) error {
		crashed.Store(true)
		return sqsconsumer.ErrSimulatedCrash
	}})
	_ = first.Start(context.Background())
	f.send("msg-1", f.bet("bet-1", "40.00"))

	deadline := time.Now().Add(10 * time.Second)
	for !crashed.Load() && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	_ = first.Stop(context.Background()) // o "processo" morreu
	if !crashed.Load() {
		t.Fatal("o primeiro consumidor não chegou ao commit")
	}
	// confirmado no banco, mas ainda na fila (invisível)
	if f.debits() != 1 || f.q.Depth(false) != 1 {
		t.Fatalf("após a queda: débitos %d, fila %d", f.debits(), f.q.Depth(false))
	}

	second := directConsumer(t, f.db, f.q, sqsconsumer.Hooks{})
	_ = second.Start(context.Background())
	f.q.WaitEmpty(10 * time.Second) // reentregue após a visibilidade (2s) e removida

	if f.debits() != 1 || f.balance() != "60.00" || f.q.Depth(true) != 0 {
		t.Errorf("débitos %d, saldo %s, dlq %d", f.debits(), f.balance(), f.q.Depth(true))
	}
	if n := apptest.Count(t, f.db, `SELECT count(*) FROM inbox_messages`); n != 1 {
		t.Errorf("inbox = %d", n)
	}
}

// Falhas transitórias: a mensagem volta com backoff e, esgotado o
// maxReceiveCount, o redrive do SQS a leva para a DLQ.
func TestSQS_TransientFailuresEndInDLQ(t *testing.T) {
	f := newSQSFixture(t, "100.00", 2*time.Second, 2, false)
	var attempts atomic.Int32
	c := directConsumer(t, f.db, f.q, sqsconsumer.Hooks{BeforeHandle: func(context.Context, string) error {
		attempts.Add(1)
		return errors.New("banco indisponível (simulado)")
	}})
	_ = c.Start(context.Background())
	f.send("msg-1", f.bet("bet-1", "10.00"))

	got := f.q.ReadDLQ(1, 20*time.Second)
	if len(got) != 1 {
		t.Fatalf("a mensagem deveria estar na DLQ (tentativas: %d)", attempts.Load())
	}
	if attempts.Load() != 2 {
		t.Errorf("tentativas = %d, want 2 (maxReceiveCount)", attempts.Load())
	}
	if f.debits() != 0 {
		t.Error("falhas transitórias não podem movimentar")
	}
}

// SIGTERM: a mensagem em andamento termina e é apagada; o long polling
// ocioso é interrompido na hora.
func TestSQS_GracefulShutdown(t *testing.T) {
	f := newSQSFixture(t, "100.00", 10*time.Second, 5, false)
	entered := make(chan struct{})
	var once sync.Once
	c := directConsumer(t, f.db, f.q, sqsconsumer.Hooks{BeforeHandle: func(context.Context, string) error {
		once.Do(func() { close(entered) })
		time.Sleep(700 * time.Millisecond) // trabalho em andamento
		return nil
	}})
	_ = c.Start(context.Background())
	f.send("msg-1", f.bet("bet-1", "10.00"))
	<-entered

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.Stop(ctx); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if f.debits() != 1 || f.q.Depth(false) != 0 {
		t.Errorf("a mensagem em andamento deveria ter concluído e saído da fila: débitos %d, fila %d", f.debits(), f.q.Depth(false))
	}

	idle := directConsumer(t, f.db, f.q, sqsconsumer.Hooks{})
	_ = idle.Start(context.Background())
	time.Sleep(200 * time.Millisecond) // dentro de um long poll
	start := time.Now()
	_ = idle.Stop(context.Background())
	if d := time.Since(start); d > time.Second {
		t.Errorf("parada ociosa levou %v", d)
	}
}

func fmtBody(r apptest.Response) string {
	b, _ := json.Marshal(r.Body)
	return string(b)
}
