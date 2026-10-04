//go:build integration

package integration

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Taino-Edu/Desafio-go-arqui/internal/platform/config"
	"github.com/Taino-Edu/Desafio-go-arqui/internal/testsupport/apptest"
	"github.com/Taino-Edu/Desafio-go-arqui/internal/testsupport/chaostest"
	"github.com/Taino-Edu/Desafio-go-arqui/internal/testsupport/pgtest"
	"github.com/Taino-Edu/Desafio-go-arqui/internal/testsupport/sqstest"
)

// viaProxy troca o host:porta de uma URL pelo do proxy.
func viaProxy(t *testing.T, raw string, p *chaostest.Proxy) string {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	u.Host = p.Addr()
	return u.String()
}

func hostOf(t *testing.T, raw string) string {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u.Host
}

// waitHTTP espera GET path responder want.
func waitHTTP(t *testing.T, c *apptest.Client, path string, want int, timeout time.Duration) apptest.Response {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var r apptest.Response
	for time.Now().Before(deadline) {
		if r = c.Do("GET", path, nil); r.Status == want {
			return r
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("GET %s = %d %v, want %d", path, r.Status, r.Body, want)
	return r
}

// Postgres fora do ar no meio da operação: a API responde 503 com
// Retry-After (nada gravado pela metade), o readiness sai do balanceador, o
// processo continua vivo e, com o banco de volta, a MESMA requisição é
// processada uma única vez.
func TestChaos_PostgresOutage(t *testing.T) {
	db := pgtest.New(t)
	proxy := chaostest.NewProxy(t, hostOf(t, db.AppURL))
	a, _ := apptest.StartOn(t, db, func(c *config.Config) {
		c.Database.URL = viaProxy(t, db.AppURL, proxy)
		c.Database.LockTimeout = 500 * time.Millisecond
	})
	player := uuid.NewString()
	wallet := a.Client.OpenWallet(player, "100.00")
	bet := func(ext string) apptest.Op {
		return apptest.Op{Provider: "provider-a", ExternalID: ext, PlayerID: player, WalletID: wallet,
			Round: "r", Game: "g", Kind: "BET", Amount: "10.00"}
	}
	apptest.Must(t, a.Client.Submit(bet("bet-1")), http.StatusCreated, "antes da queda")

	proxy.Cut()

	r := a.Client.Submit(bet("bet-2"))
	if r.Status != http.StatusServiceUnavailable || r.ErrorCode() != "TEMPORARILY_UNAVAILABLE" || r.Header.Get("Retry-After") == "" {
		t.Errorf("com o banco fora = %d %v (Retry-After %q)", r.Status, r.Body, r.Header.Get("Retry-After"))
	}
	ready := waitHTTP(t, a.Client, "/health/ready", http.StatusServiceUnavailable, 5*time.Second)
	if checks, _ := ready.Body["checks"].(map[string]any); checks["postgres"] != "unavailable" {
		t.Errorf("ready = %v", ready.Body)
	}
	if r := a.Client.Do("GET", "/health/live", nil); r.Status != http.StatusOK {
		t.Errorf("live = %d: o processo continua vivo", r.Status)
	}
	// o /metrics continua respondendo; só as métricas lidas do banco somem
	if !strings.Contains(a.Client.Metrics(), "wager_transactions_total") {
		t.Error("/metrics deveria continuar respondendo")
	}

	proxy.Restore()
	waitHTTP(t, a.Client, "/health/ready", http.StatusOK, 10*time.Second)

	// o cliente repete a requisição que falhou, com a mesma chave
	r = apptest.Must(t, a.Client.Submit(bet("bet-2")), http.StatusCreated, "repetição depois da volta")
	if r.Amount("balance") != "80.00" {
		t.Errorf("saldo = %s", r.Amount("balance"))
	}
	apptest.Must(t, a.Client.Submit(bet("bet-2")), http.StatusOK, "replay")
	if n := apptest.Count(t, db, `SELECT count(*) FROM wallet_ledger_entries WHERE wallet_id = $1 AND direction = 'DEBIT'`, wallet); n != 2 {
		t.Errorf("débitos = %d, want 2", n)
	}
	apptest.AssertReconciled(t, db)
}

// SQS fora do ar: as operações por HTTP continuam (o banco está de pé); os
// eventos ficam guardados na outbox e as mensagens na fila. Com o SQS de
// volta, o consumidor retoma e o publicador entrega TODOS os eventos
// confirmados: nenhum se perde.
func TestChaos_SQSOutage(t *testing.T) {
	db := pgtest.New(t)
	input := sqstest.New(t, 5*time.Second, 5)
	events := sqstest.New(t, 30*time.Second, 5)
	proxy := chaostest.NewProxy(t, hostOf(t, sqstest.Endpoint()))
	a, _ := apptest.StartOn(t, db, func(c *config.Config) {
		withSQS(input)(c)
		c.SQS.Endpoint = "http://" + proxy.Addr()
		c.Outbox = config.Outbox{
			Enabled: true, Queue: events.URL, BatchSize: 50, PollInterval: 50 * time.Millisecond,
			Lease: 5 * time.Second, RetryBaseDelay: 100 * time.Millisecond, RetryMaxDelay: 500 * time.Millisecond,
		}
	})
	player := uuid.NewString()
	wallet := a.Client.OpenWallet(player, "100.00")
	op := func(ext string) apptest.Op {
		return apptest.Op{Provider: "provider-a", ExternalID: ext, PlayerID: player, WalletID: wallet,
			Round: "r", Game: "g", Kind: "BET", Amount: "10.00"}
	}

	proxy.Cut()

	ready := waitHTTP(t, a.Client, "/health/ready", http.StatusServiceUnavailable, 5*time.Second)
	if checks, _ := ready.Body["checks"].(map[string]any); checks["sqs"] != "unavailable" || checks["postgres"] != "ok" {
		t.Errorf("ready = %v", ready.Body)
	}
	// HTTP segue funcionando; o evento fica na outbox
	apptest.Must(t, a.Client.Submit(op("bet-http")), http.StatusCreated, "http com o SQS fora")
	// o produtor ainda alcança o SQS (só a aplicação perdeu o acesso)
	input.Send(sqstest.Message("msg-1", data(op("bet-sqs"), "")), wallet, "d-1")
	// o publicador tenta, falha e registra (o SDK da AWS ainda retenta
	// algumas vezes antes de devolver o erro)
	deadline := time.Now().Add(20 * time.Second)
	for {
		failed, _ := apptest.MetricValue(a.Client.Metrics(), `outbox_events_total{result="failed"}`)
		if failed > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("as falhas de publicação deveriam aparecer na métrica")
		}
		time.Sleep(100 * time.Millisecond)
	}
	if n := unpublished(t, db); n == 0 {
		t.Error("com o SQS fora, os eventos deveriam esperar na outbox")
	}
	if n := apptest.Count(t, db, `SELECT count(*) FROM wager_transactions WHERE external_transaction_id = 'bet-sqs'`); n != 0 {
		t.Error("a mensagem não poderia ter sido consumida com o SQS fora")
	}

	proxy.Restore()

	a.Client.WaitStatus("provider-a", "bet-sqs", "PROCESSED", 30*time.Second)
	input.WaitEmpty(10 * time.Second)
	deadline = time.Now().Add(15 * time.Second)
	for unpublished(t, db) > 0 && time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
	}
	if n := unpublished(t, db); n != 0 {
		t.Fatalf("eventos não publicados depois da volta: %d", n)
	}
	waitHTTP(t, a.Client, "/health/ready", http.StatusOK, 10*time.Second)

	// todo evento confirmado chegou à fila de eventos
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
	for _, m := range events.ReadQueue(len(want), 15*time.Second) {
		var env struct {
			EventID string `json:"eventId"`
		}
		_ = json.Unmarshal([]byte(m.Body), &env)
		got[env.EventID] = true
	}
	for id := range want {
		if !got[id] {
			t.Errorf("evento %s confirmado no banco e nunca publicado", id)
		}
	}
	if len(want) != 6 { // abertura 2 + aposta HTTP 2 + aposta SQS 2
		t.Errorf("eventos na outbox = %d, want 6", len(want))
	}
	apptest.AssertReconciled(t, db)
}
