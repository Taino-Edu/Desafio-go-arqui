//go:build integration

package integration

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Taino-Edu/Desafio-go-arqui/internal/platform/config"
	"github.com/Taino-Edu/Desafio-go-arqui/internal/testsupport/apptest"
	"github.com/Taino-Edu/Desafio-go-arqui/internal/testsupport/idptest"
	"github.com/Taino-Edu/Desafio-go-arqui/internal/testsupport/pgtest"
)

func reconcile(c *apptest.Client, walletID string) apptest.Response {
	return c.Do("POST", "/wallets/"+walletID+"/reconciliation", nil)
}

// O exemplo do enunciado: abertura de 1000.00 e aposta de 25.00.
func TestReconciliation_ConsistentWallet(t *testing.T) {
	a := apptest.Start(t)
	player := uuid.NewString()
	wallet := a.Client.OpenWallet(player, "1000.00")
	apptest.Must(t, a.Client.Submit(apptest.Op{Provider: "provider-a", ExternalID: "bet-1", PlayerID: player,
		WalletID: wallet, Round: "r", Game: "g", Kind: "BET", Amount: "25.00"}), http.StatusCreated, "bet")

	r := apptest.Must(t, reconcile(a.Client, wallet), http.StatusOK, "reconciliation")
	want := map[string]string{"storedBalance": "975.00", "calculatedBalance": "975.00", "difference": "0.00"}
	for field, amount := range want {
		if r.Amount(field) != amount {
			t.Errorf("%s = %s, want %s", field, r.Amount(field), amount)
		}
		if m := r.Body[field].(map[string]any); m["currency"] != "BRL" {
			t.Errorf("%s.currency = %v", field, m["currency"])
		}
	}
	if r.Str("walletId") != wallet || r.Body["consistent"] != true || r.Body["checkedEntries"] != float64(2) {
		t.Errorf("reconciliation = %v", r.Body)
	}

	t.Run("só o serviço interno reconcilia", func(t *testing.T) {
		r := reconcile(a.Client.AsClient(idptest.ProviderA), wallet)
		if r.Status != http.StatusForbidden {
			t.Errorf("provedor = %d %v", r.Status, r.Body)
		}
		if r := reconcile(a.Client.As(""), wallet); r.Status != http.StatusUnauthorized {
			t.Errorf("sem token = %d", r.Status)
		}
	})
	t.Run("carteira inexistente e id inválido", func(t *testing.T) {
		if r := reconcile(a.Client, uuid.NewString()); r.Status != http.StatusNotFound {
			t.Errorf("inexistente = %d %v", r.Status, r.Body)
		}
		if r := reconcile(a.Client, "nao-e-uuid"); r.Status != http.StatusBadRequest {
			t.Errorf("id inválido = %d %v", r.Status, r.Body)
		}
	})
	t.Run("carteira aberta com saldo zero: nenhum lançamento", func(t *testing.T) {
		empty := a.Client.OpenWallet(uuid.NewString(), "0.00")
		r := apptest.Must(t, reconcile(a.Client, empty), http.StatusOK, "reconciliation")
		if r.Amount("calculatedBalance") != "0.00" || r.Body["consistent"] != true || r.Body["checkedEntries"] != float64(0) {
			t.Errorf("= %v", r.Body)
		}
	})
}

// corruptBalance simula um bug que mexeu no saldo sem lançamento no ledger.
// Só o dono da tabela consegue: desliga a checagem adiada saldo = ledger,
// altera o saldo (a versão sobe junto, exigida pelo outro gatilho) e religa.
func corruptBalance(t *testing.T, db *pgtest.DB, walletID string, deltaMinor int64) {
	t.Helper()
	ctx := context.Background()
	tx, err := db.Owner.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `ALTER TABLE wallets DISABLE TRIGGER wallets_check_ledger`); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `UPDATE wallets SET balance_minor = balance_minor + $2, version = version + 1
		WHERE id = $1`, walletID, deltaMinor); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `ALTER TABLE wallets ENABLE TRIGGER wallets_check_ledger`); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}

type walletRow struct {
	balance, version, updatedAtMicros int64
	entries                           int
}

func readWallet(t *testing.T, db *pgtest.DB, walletID string) walletRow {
	t.Helper()
	var (
		w       walletRow
		updated time.Time
	)
	if err := db.Owner.QueryRow(context.Background(), `
		SELECT balance_minor, version, updated_at,
		       (SELECT count(*) FROM wallet_ledger_entries WHERE wallet_id = $1)
		  FROM wallets WHERE id = $1`, walletID).Scan(&w.balance, &w.version, &updated, &w.entries); err != nil {
		t.Fatal(err)
	}
	w.updatedAtMicros = updated.UnixMicro()
	return w
}

// A divergência aparece na resposta, no log e na métrica, e a reconciliação
// não conserta nada sozinha.
func TestReconciliation_ReportsDivergenceWithoutChangingAnything(t *testing.T) {
	a := apptest.Start(t)
	player := uuid.NewString()
	wallet := a.Client.OpenWallet(player, "1000.00")
	apptest.Must(t, a.Client.Submit(apptest.Op{Provider: "provider-a", ExternalID: "bet-1", PlayerID: player,
		WalletID: wallet, Round: "r", Game: "g", Kind: "BET", Amount: "25.00"}), http.StatusCreated, "bet")

	corruptBalance(t, a.DB, wallet, 1000) // +10.00 sem lançamento
	before := readWallet(t, a.DB, wallet)

	r := apptest.Must(t, a.Client.Do("POST", "/wallets/"+wallet+"/reconciliation", nil,
		"X-Correlation-Id", "recon-check-1"), http.StatusOK, "reconciliation")
	if r.Amount("storedBalance") != "985.00" || r.Amount("calculatedBalance") != "975.00" ||
		r.Amount("difference") != "10.00" || r.Body["consistent"] != false || r.Body["checkedEntries"] != float64(2) {
		t.Errorf("reconciliation = %v", r.Body)
	}

	// nada mudou: nem saldo, nem versão, nem updated_at, nem o ledger
	if after := readWallet(t, a.DB, wallet); after != before {
		t.Errorf("a reconciliação alterou a carteira: antes %+v, depois %+v", before, after)
	}

	// métrica
	text := a.Client.Metrics()
	for series, want := range map[string]float64{
		`reconciliation_mismatch_total`:              1,
		`reconciliations_total{result="divergent"}`:  1,
		`reconciliations_total{result="consistent"}`: 0,
	} {
		if got, ok := apptest.MetricValue(text, series); !ok || got != want {
			t.Errorf("%s = %v (presente %v), want %v", series, got, ok, want)
		}
	}

	// log de erro, com os ids para investigar e sem os saldos
	logs := a.Logs.Find("reconciliation mismatch")
	if len(logs) != 1 {
		t.Fatalf("logs de divergência = %d", len(logs))
	}
	l := logs[0]
	if l["level"] != "ERROR" || l["walletId"] != wallet || l["difference"] != "10.00 BRL" ||
		l["correlationId"] != "recon-check-1" || l["checkedEntries"] != float64(2) {
		t.Errorf("log = %v", l)
	}
	if _, ok := l["storedBalance"]; ok {
		t.Error("o log não deve expor saldos")
	}
}

// A reconciliação lê saldo e ledger na MESMA foto do banco. Sob carga, uma
// aposta que confirma entre as duas leituras não pode gerar falso alarme.
func TestReconciliation_UnderLoadHasNoFalseAlarms(t *testing.T) {
	a := apptest.Start(t)
	player := uuid.NewString()
	wallet := a.Client.OpenWallet(player, "10000.00")

	const workers, betsEach = 8, 25
	var (
		betting      sync.WaitGroup
		done         atomic.Bool
		checks, bad  atomic.Int64
		failedBets   atomic.Int64
		reconcilers  sync.WaitGroup
		firstFailure atomic.Value
	)
	for w := range workers {
		betting.Add(1)
		go func() {
			defer betting.Done()
			for i := range betsEach {
				r := a.Client.Submit(apptest.Op{Provider: "provider-a", ExternalID: fmt.Sprintf("bet-%d-%d", w, i),
					PlayerID: player, WalletID: wallet, Round: "r", Game: "g", Kind: "BET", Amount: "1.00"})
				if r.Status != http.StatusCreated {
					failedBets.Add(1)
				}
			}
		}()
	}
	for range 4 {
		reconcilers.Add(1)
		go func() {
			defer reconcilers.Done()
			for !done.Load() {
				r := reconcile(a.Client, wallet)
				checks.Add(1)
				if r.Status != http.StatusOK || r.Body["consistent"] != true ||
					r.Amount("storedBalance") != r.Amount("calculatedBalance") {
					bad.Add(1)
					firstFailure.CompareAndSwap(nil, fmt.Sprintf("%d %v", r.Status, r.Body))
				}
			}
		}()
	}
	betting.Wait()
	done.Store(true)
	reconcilers.Wait()

	if failedBets.Load() > 0 {
		t.Fatalf("%d apostas não foram processadas", failedBets.Load())
	}
	if bad.Load() > 0 {
		t.Fatalf("%d de %d reconciliações acusaram divergência falsa; primeira: %v", bad.Load(), checks.Load(), firstFailure.Load())
	}
	r := apptest.Must(t, reconcile(a.Client, wallet), http.StatusOK, "final")
	if r.Amount("storedBalance") != "9800.00" || r.Body["checkedEntries"] != float64(1+workers*betsEach) {
		t.Errorf("final = %v", r.Body)
	}
	t.Logf("%d reconciliações durante %d apostas, nenhuma divergência falsa", checks.Load(), workers*betsEach)
}

// /metrics é público e expõe os desfechos por status, duplicatas, conflitos,
// latência, pendências, atraso da outbox e as requisições HTTP por rota.
func TestMetrics_ExposeOutcomesAndBacklog(t *testing.T) {
	a := apptest.Start(t)
	player := uuid.NewString()
	wallet := a.Client.OpenWallet(player, "100.00")
	op := func(ext, kind, amount, ref string) apptest.Op {
		return apptest.Op{Provider: "provider-a", ExternalID: ext, PlayerID: player, WalletID: wallet,
			Round: "r", Game: "g", Kind: kind, Amount: amount, RefID: ref}
	}

	apptest.Must(t, a.Client.Submit(op("bet-1", "BET", "10.00", "")), http.StatusCreated, "bet")
	apptest.Must(t, a.Client.Submit(op("bet-1", "BET", "10.00", "")), http.StatusOK, "replay")
	conflict := op("bet-1", "BET", "99.00", "")
	conflict.Key = "provider-a:bet-1"
	apptest.Must(t, a.Client.Submit(conflict), http.StatusConflict, "chave reutilizada")
	// WIN que chega antes da aposta: fica pendente, e o worker a conclui
	// assim que a aposta chega
	win := apptest.Must(t, a.Client.Submit(op("win-2", "WIN", "30.00", "bet-2")), http.StatusAccepted, "win pendente")
	apptest.Must(t, a.Client.Submit(op("bet-2", "BET", "5.00", "")), http.StatusCreated, "bet referenciada")
	a.Client.WaitStatus("provider-a", "win-2", "PROCESSED", 10*time.Second)
	a.Client.Do("GET", "/wallets/"+wallet, nil)

	c := a.Client
	c.WaitMetric(`reference_resolutions_total{outcome="RESOLVED"}`, 1, 5*time.Second)
	text := c.Metrics()
	for series, want := range map[string]float64{
		`wager_transactions_total{kind="BET",source="http",status="PROCESSED"}`:          2,
		`wager_transactions_total{kind="WIN",source="http",status="PENDING_REFERENCE"}`:  1,
		`wager_transactions_total{kind="WIN",source="worker",status="PROCESSED"}`:        1,
		`idempotent_replays_total{source="http"}`:                                        1,
		`idempotency_conflicts_total{reason="key_reused",source="http"}`:                 1,
		`wager_processing_duration_seconds_count{source="http"}`:                         4, // 3 novas + 1 replay
		`wager_pending_references`:                                                       0,
		`http_requests_total{method="POST",route="/wallets",status="201"}`:               1,
		`http_requests_total{method="POST",route="/wagering/transactions",status="201"}`: 2,
		`http_requests_total{method="POST",route="/wagering/transactions",status="409"}`: 1,
		`http_requests_total{method="GET",route="/wallets/{walletId}",status="200"}`:     1,
	} {
		if got, ok := apptest.MetricValue(text, series); !ok || got != want {
			t.Errorf("%s = %v (presente %v), want %v", series, got, ok, want)
		}
	}
	// o publicador está desligado neste teste: os eventos se acumulam e o
	// atraso da outbox cresce
	if n, ok := apptest.MetricValue(text, "outbox_pending_events"); !ok || n < 8 {
		t.Errorf("outbox_pending_events = %v (presente %v)", n, ok)
	}
	if lag, ok := apptest.MetricValue(text, "outbox_lag_seconds"); !ok || lag <= 0 {
		t.Errorf("outbox_lag_seconds = %v (presente %v)", lag, ok)
	}
	// ids nunca viram rótulo (cada carteira criaria uma série nova)
	if strings.Contains(text, wallet) || strings.Contains(text, win.Str("transactionId")) {
		t.Error("/metrics contém ids de carteira ou de transação")
	}

	// a conclusão pelo worker não tem requisição de origem: o correlationId
	// dos eventos dela é o próprio id da operação
	if n := apptest.Count(t, a.DB, `SELECT count(*) FROM outbox_events WHERE correlation_id = $1`, win.Str("transactionId")); n != 2 {
		t.Errorf("eventos da conclusão com correlationId = transactionId: %d, want 2", n)
	}
}

// Logs JSON com os identificadores para rastrear a operação, e nunca o token.
func TestLogs_CarryIdentifiersAndNoSecrets(t *testing.T) {
	a := apptest.Start(t)
	player := uuid.NewString()
	wallet := a.Client.OpenWallet(player, "100.00")
	token := idptest.Token(t, idptest.ProviderA)

	r := a.Client.As(token).Do("POST", "/wagering/transactions", apptest.Op{Provider: "provider-a",
		ExternalID: "bet-1", PlayerID: player, WalletID: wallet, Round: "r", Game: "g", Kind: "BET", Amount: "10.00",
	}.Body(), "Idempotency-Key", "provider-a:bet-1", "X-Correlation-Id", "corr-log-1")
	apptest.Must(t, r, http.StatusCreated, "bet")

	submitted := a.Logs.Find("wager transaction submitted")
	if len(submitted) != 1 {
		t.Fatalf("logs de envio = %d", len(submitted))
	}
	s := submitted[0]
	if s["correlationId"] != "corr-log-1" || s["transactionId"] != r.Str("transactionId") ||
		s["walletId"] != wallet || s["providerId"] != "provider-a" || s["service"] != "wallet" {
		t.Errorf("log de envio = %v", s)
	}
	found := false
	for _, l := range a.Logs.Find("http request") {
		if l["correlationId"] == "corr-log-1" && l["path"] == "/wagering/transactions" && l["status"] == float64(201) {
			found = true
		}
	}
	if !found {
		t.Error("log de acesso sem o correlationId")
	}
	raw := a.Logs.String()
	if strings.Contains(raw, token) || strings.Contains(raw, "Bearer") {
		t.Error("o token apareceu nos logs")
	}
	if strings.Contains(raw, `"amount"`) {
		t.Error("o payload financeiro apareceu nos logs")
	}
}

// Disputa de lock: outra transação segura a carteira além do lock_timeout.
// A aplicação tenta 3 vezes, conta os conflitos e responde 503 com
// Retry-After, sem gravar nada; a repetição do cliente processa uma vez só.
func TestLockContention_IsCountedAndAnswered503(t *testing.T) {
	db := pgtest.New(t)
	a, _ := apptest.StartOn(t, db, func(c *config.Config) { c.Database.LockTimeout = 150 * time.Millisecond })
	player := uuid.NewString()
	wallet := a.Client.OpenWallet(player, "100.00")
	bet := apptest.Op{Provider: "provider-a", ExternalID: "bet-1", PlayerID: player, WalletID: wallet,
		Round: "r", Game: "g", Kind: "BET", Amount: "25.00"}

	ctx := context.Background()
	holder, err := db.Owner.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := holder.Exec(ctx, `SELECT 1 FROM wallets WHERE id = $1 FOR UPDATE`, wallet); err != nil {
		t.Fatal(err)
	}

	r := a.Client.Submit(bet)
	if r.Status != http.StatusServiceUnavailable || r.ErrorCode() != "TEMPORARILY_UNAVAILABLE" || r.Header.Get("Retry-After") == "" {
		t.Errorf("com a carteira travada = %d %v (Retry-After %q)", r.Status, r.Body, r.Header.Get("Retry-After"))
	}
	if n := apptest.Count(t, db, `SELECT count(*) FROM wager_transactions WHERE external_transaction_id = 'bet-1'`); n != 0 {
		t.Errorf("as tentativas desfeitas deixaram %d transações", n)
	}
	text := a.Client.Metrics()
	for series, want := range map[string]float64{
		`wallet_lock_conflicts_total{source="http"}`:                                     3,
		`transient_retries_total{source="http"}`:                                         2,
		`http_requests_total{method="POST",route="/wagering/transactions",status="503"}`: 1,
	} {
		if got, ok := apptest.MetricValue(text, series); !ok || got != want {
			t.Errorf("%s = %v (presente %v), want %v", series, got, ok, want)
		}
	}

	if err := holder.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	r = apptest.Must(t, a.Client.Submit(bet), http.StatusCreated, "repetição depois do lock")
	if r.Amount("balance") != "75.00" {
		t.Errorf("saldo = %s", r.Amount("balance"))
	}
	apptest.AssertReconciled(t, db)
}
