//go:build integration

package integration

import (
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Taino-Edu/Desafio-go-arqui/internal/testsupport/apptest"
	"github.com/Taino-Edu/Desafio-go-arqui/internal/testsupport/pgtest"
	"github.com/Taino-Edu/Desafio-go-arqui/internal/testsupport/sqstest"
)

// fullStackEnv liga tudo numa instância: consumidor SQS, publicador da
// outbox e worker de referências, com prazos curtos para o teste.
func fullStackEnv(input, events *sqstest.Queues) []string {
	return []string{
		"SQS_ENABLED=true", "AWS_REGION=us-east-1", "SQS_ENDPOINT=" + sqstest.Endpoint(),
		"SQS_ACCESS_KEY_ID=test", "SQS_SECRET_ACCESS_KEY=test",
		"SQS_INPUT_QUEUE=" + input.URL, "SQS_INPUT_DLQ=" + input.DLQURL,
		"SQS_ALLOWED_PROVIDERS=provider-a,provider-b", "SQS_WAIT_TIME=1s",
		"SQS_RETRY_BASE_DELAY=1s", "SQS_RETRY_MAX_DELAY=2s",
		"OUTBOX_PUBLISHER_ENABLED=true", "OUTBOX_QUEUE=" + events.URL,
		"OUTBOX_LEASE=3s", "OUTBOX_POLL_INTERVAL=50ms",
		"OUTBOX_RETRY_BASE_DELAY=100ms", "OUTBOX_RETRY_MAX_DELAY=1s",
		"REFERENCE_WORKER_ENABLED=true", "REFERENCE_WORKER_INTERVAL=50ms",
		"REFERENCE_RETRY_BASE_DELAY=100ms", "REFERENCE_RETRY_MAX_DELAY=1s",
		"REFERENCE_RETRY_MAX_ATTEMPTS=1000", // nos testes de queda, nada expira
	}
}

// waitFor repete cond até ser verdadeira ou o prazo acabar.
func waitFor(t *testing.T, what string, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("esperando %s: prazo de %v esgotado", what, timeout)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// assertAllEventsPublished confere que todo evento gravado na outbox foi
// publicado na fila de eventos (pelo menos uma vez).
func assertAllEventsPublished(t *testing.T, db *pgtest.DB, events *sqstest.Queues) {
	t.Helper()
	waitFor(t, "a outbox esvaziar", 30*time.Second, func() bool { return unpublished(t, db) == 0 })
	want := map[string]bool{}
	rows, err := db.Owner.Query(t.Context(), `SELECT event_id::text FROM outbox_events`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var id string
		_ = rows.Scan(&id)
		want[id] = true
	}
	rows.Close()
	got := map[string]bool{}
	for _, m := range events.ReadQueue(len(want), 30*time.Second) {
		var env struct {
			EventID string `json:"eventId"`
		}
		_ = json.Unmarshal([]byte(m.Body), &env)
		got[env.EventID] = true
	}
	missing := 0
	for id := range want {
		if !got[id] {
			missing++
		}
	}
	if missing > 0 {
		t.Errorf("%d de %d eventos confirmados nunca foram publicados", missing, len(want))
	}
}

// Teste obrigatório 8: a aplicação é derrubada com kill -9 (sem shutdown
// gracioso) e outra instância sobe no mesmo banco. Idempotência,
// pendências, inbox, outbox e consistência financeira sobrevivem.
func TestRestart_KillDashNinePreservesEverything(t *testing.T) {
	db := pgtest.New(t)
	input := sqstest.New(t, 3*time.Second, 10)
	events := sqstest.New(t, 30*time.Second, 5)
	env := fullStackEnv(input, events)

	first := launchInstance(t, db, "before-crash", env...)
	first.waitReady(t)
	c := first.client
	player := uuid.NewString()
	wallet := c.OpenWallet(player, "100.00")

	bet := betOp(player, wallet, "bet-1", "10.00")
	original := apptest.Must(t, c.Submit(bet), http.StatusCreated, "aposta")
	// REFUND antes da aposta que ele estorna: aceito como pendente
	refund := refundOp(player, wallet, "refund-1", "bet-late", "5.00")
	apptest.Must(t, c.Submit(refund), http.StatusAccepted, "refund pendente")
	// e uma operação pela fila
	sqsBet := betOp(player, wallet, "bet-sqs", "10.00")
	input.Send(sqstest.Message("msg-1", data(sqsBet, "")), wallet, "d-1")
	c.WaitStatus("provider-a", "bet-sqs", "PROCESSED", 15*time.Second)

	first.kill(t)

	second := launchInstance(t, db, "after-crash", env...)
	second.waitReady(t)
	c = second.client

	// idempotência: o replay devolve o resultado ORIGINAL (saldo 90.00)
	r := apptest.Must(t, c.Submit(bet), http.StatusOK, "replay depois do reinício")
	if r.Body["idempotentReplay"] != true || r.Str("transactionId") != original.Str("transactionId") || r.Amount("balance") != "90.00" {
		t.Errorf("replay = %v; original = %v", r.Body, original.Body)
	}
	// a pendência continua lá, e o reenvio a reconhece
	r = apptest.Must(t, c.Submit(refund), http.StatusAccepted, "reenvio do refund")
	if r.Body["idempotentReplay"] != true || r.Str("status") != "PENDING_REFERENCE" {
		t.Errorf("refund = %v", r.Body)
	}
	// o produtor reenvia a mensagem da fila: a inbox reconhece
	input.Send(sqstest.Message("msg-1", data(sqsBet, "")), wallet, "d-1-resend")
	input.WaitEmpty(15 * time.Second)

	// a referência chega: o worker da NOVA instância conclui a pendência
	apptest.Must(t, c.Submit(betOp(player, wallet, "bet-late", "5.00")), http.StatusCreated, "aposta referenciada")
	c.WaitStatus("provider-a", "refund-1", "PROCESSED", 15*time.Second)

	got := c.Do("GET", "/wallets/"+wallet, nil)
	if got.Amount("balance") != "80.00" { // 100 - 10 - 10 - 5 + 5
		t.Errorf("saldo = %s, want 80.00", got.Amount("balance"))
	}
	if n := apptest.Count(t, db, `SELECT count(*) FROM wallet_ledger_entries WHERE wallet_id = $1 AND direction = 'DEBIT'`, wallet); n != 3 {
		t.Errorf("débitos = %d, want 3 (nenhum repetido)", n)
	}
	if n := apptest.Count(t, db, `SELECT count(*) FROM inbox_messages WHERE completed_at IS NOT NULL`); n != 1 {
		t.Errorf("inbox = %d, want 1", n)
	}
	rec := apptest.Must(t, c.Do("POST", "/wallets/"+wallet+"/reconciliation", nil), http.StatusOK, "reconciliação")
	if rec.Body["consistent"] != true {
		t.Errorf("reconciliação = %v", rec.Body)
	}
	assertAllEventsPublished(t, db, events)
	apptest.AssertReconciled(t, db)
}

// cluster distribui as requisições entre as instâncias vivas. Como um
// cliente de verdade atrás de um balanceador: se a conexão cai (instância
// morta no meio da requisição) ou volta 503, repete em outra instância com
// a MESMA chave de idempotência.
type cluster struct {
	mu      sync.Mutex
	insts   []*instance
	retries atomic.Int64
}

func (c *cluster) add(i *instance) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.insts = append(c.insts, i)
}

func (c *cluster) live() []*instance {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []*instance
	for _, i := range c.insts {
		if !i.exited() {
			out = append(out, i)
		}
	}
	return out
}

func (c *cluster) submit(t *testing.T, op apptest.Op) apptest.Response {
	deadline := time.Now().Add(60 * time.Second)
	for {
		live := c.live()
		if len(live) > 0 {
			r, err := live[rand.IntN(len(live))].client.TrySubmit(op)
			if err == nil && r.Status != http.StatusServiceUnavailable {
				return r
			}
		}
		c.retries.Add(1)
		if time.Now().After(deadline) {
			t.Errorf("%s: sem resposta definitiva em 60s", op.ExternalID)
			return apptest.Response{}
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// Caos com 3 processos completos (HTTP, consumidor SQS, publicador da outbox
// e worker), sob carga de HTTP e SQS ao mesmo tempo: uma instância morre com
// kill -9, outra sobe no lugar, e uma segunda morre. Requisições, mensagens,
// reivindicações da outbox e pendências ficam pela metade em processos
// mortos. No fim: cada operação aplicada exatamente uma vez, saldos exatos,
// ledger consistente e nenhum evento confirmado perdido.
func TestChaos_KillInstancesUnderLoad(t *testing.T) {
	db := pgtest.New(t)
	input := sqstest.New(t, 3*time.Second, 50)
	events := sqstest.New(t, 30*time.Second, 5)
	env := fullStackEnv(input, events)

	cl := &cluster{}
	for i := 1; i <= 3; i++ {
		cl.add(launchInstance(t, db, fmt.Sprintf("instance-%d", i), env...))
	}
	for _, i := range cl.live() {
		i.waitReady(t)
	}

	const wallets, httpBets, sqsBets, late = 4, 15, 8, 2
	type wal struct{ player, id string }
	ws := make([]wal, wallets)
	for i := range ws {
		ws[i].player = uuid.NewString()
		ws[i].id = cl.live()[0].client.OpenWallet(ws[i].player, "1000.00")
	}

	// todas as mensagens da fila de uma vez: os 3 consumidores disputam
	for i, w := range ws {
		for j := range sqsBets {
			ext := fmt.Sprintf("sqs-%d-%d", i, j)
			input.Send(sqstest.Message("msg-"+ext, data(betOp(w.player, w.id, ext, "1.00"), "")), w.id, "d-"+ext)
		}
	}

	// trabalho HTTP: apostas, e REFUNDs que chegam antes das apostas que
	// eles estornam (as apostas "late" vêm depois, embaralhadas)
	var jobs []apptest.Op
	for i, w := range ws {
		for j := range httpBets {
			jobs = append(jobs, betOp(w.player, w.id, fmt.Sprintf("http-%d-%d", i, j), "1.00"))
		}
		for j := range late {
			jobs = append(jobs, refundOp(w.player, w.id, fmt.Sprintf("refund-%d-%d", i, j), fmt.Sprintf("late-%d-%d", i, j), "1.00"))
		}
	}
	rand.Shuffle(len(jobs), func(a, b int) { jobs[a], jobs[b] = jobs[b], jobs[a] })
	for i, w := range ws {
		for j := range late {
			jobs = append(jobs, betOp(w.player, w.id, fmt.Sprintf("late-%d-%d", i, j), "1.00"))
		}
	}

	var (
		done       atomic.Int64
		unexpected sync.Map
		wg         sync.WaitGroup
		queue      = make(chan apptest.Op)
	)
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for op := range queue {
				r := cl.submit(t, op)
				switch r.Status {
				case http.StatusCreated, http.StatusOK, http.StatusAccepted:
				default:
					unexpected.Store(op.ExternalID, fmt.Sprintf("%d %v", r.Status, r.Body))
				}
				done.Add(1)
			}
		}()
	}

	// o caos acontece no meio da carga
	chaos := make(chan struct{})
	go func() {
		defer close(chaos)
		total := int64(len(jobs))
		waitDone := func(frac float64) {
			for float64(done.Load()) < frac*float64(total) {
				time.Sleep(5 * time.Millisecond)
			}
		}
		insts := cl.live()
		waitDone(0.25)
		insts[0].kill(t)
		t.Logf("kill -9 em %s com %d/%d operações concluídas", insts[0].name, done.Load(), total)
		replacement := launchInstance(t, db, "instance-4", env...)
		replacement.waitReady(t)
		cl.add(replacement)
		waitDone(0.60)
		insts[1].kill(t)
		t.Logf("kill -9 em %s com %d/%d operações concluídas", insts[1].name, done.Load(), total)
	}()

	for _, op := range jobs {
		queue <- op
	}
	close(queue)
	wg.Wait()
	<-chaos

	unexpected.Range(func(k, v any) bool {
		t.Errorf("%s: resposta inesperada %s", k, v)
		return true
	})

	// tudo converge: mensagens consumidas, pendências resolvidas, outbox vazia
	total := wallets * (httpBets + sqsBets + 2*late)
	waitFor(t, "todas as operações gravadas", 60*time.Second, func() bool {
		return apptest.Count(t, db, `SELECT count(*) FROM wager_transactions WHERE origin = 'EXTERNAL'`) == total
	})
	waitFor(t, "as pendências serem resolvidas", 30*time.Second, func() bool {
		return apptest.Count(t, db, `SELECT count(*) FROM wager_transactions WHERE status <> 'PROCESSED'`) == 0
	})
	input.WaitEmpty(30 * time.Second)

	c := cl.live()[0].client
	for i, w := range ws {
		// 1000 − apostas HTTP − apostas SQS − apostas late + refunds
		if got := c.Do("GET", "/wallets/"+w.id, nil).Amount("balance"); got != "977.00" {
			t.Errorf("carteira %d: saldo %s, want 977.00", i, got)
		}
		debits := apptest.Count(t, db, `SELECT count(*) FROM wallet_ledger_entries WHERE wallet_id = $1 AND direction = 'DEBIT'`, w.id)
		credits := apptest.Count(t, db, `SELECT count(*) FROM wallet_ledger_entries WHERE wallet_id = $1 AND direction = 'CREDIT'`, w.id)
		if debits != httpBets+sqsBets+late || credits != 1+late {
			t.Errorf("carteira %d: %d débitos e %d créditos, want %d e %d", i, debits, credits, httpBets+sqsBets+late, 1+late)
		}
		rec := c.Do("POST", "/wallets/"+w.id+"/reconciliation", nil)
		if rec.Body["consistent"] != true {
			t.Errorf("carteira %d: reconciliação %v", i, rec.Body)
		}
	}
	if n := apptest.Count(t, db, `SELECT count(*) FROM inbox_messages WHERE completed_at IS NOT NULL`); n != wallets*sqsBets {
		t.Errorf("inbox concluída = %d, want %d", n, wallets*sqsBets)
	}
	if n := input.Depth(true); n != 0 {
		t.Errorf("DLQ = %d: nada deveria ir para a DLQ", n)
	}
	assertAllEventsPublished(t, db, events)
	apptest.AssertReconciled(t, db)
	t.Logf("%d operações (%d HTTP + %d SQS), %d repetições do cliente por queda ou 503",
		total, len(jobs), wallets*sqsBets, cl.retries.Load())
}
