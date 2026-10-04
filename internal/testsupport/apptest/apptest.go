// Package apptest sobe a aplicação completa (Fx + HTTP + Postgres real) para
// testes de ponta a ponta, e oferece um cliente HTTP e verificações de
// consistência financeira.
package apptest

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"go.uber.org/fx"
	"go.uber.org/fx/fxtest"

	"github.com/Taino-Edu/Desafio-go-arqui/internal/adapters/httpapi"
	"github.com/Taino-Edu/Desafio-go-arqui/internal/platform/config"
	"github.com/Taino-Edu/Desafio-go-arqui/internal/platform/fxapp"
	"github.com/Taino-Edu/Desafio-go-arqui/internal/testsupport/idptest"
	"github.com/Taino-Edu/Desafio-go-arqui/internal/testsupport/pgtest"
)

// Config devolve uma configuração de teste apontando para dbURL.
func Config(dbURL string) config.Config {
	return config.Config{
		InstanceID: "test",
		LogLevel:   slog.LevelWarn,
		HTTP: config.HTTP{
			Addr: "127.0.0.1:0", RequestTimeout: 8 * time.Second,
			ReadTimeout: 10 * time.Second, WriteTimeout: 10 * time.Second, IdleTimeout: 10 * time.Second,
		},
		Database: config.Database{
			URL: dbURL, MaxConns: 30, StatementTimeout: 5 * time.Second, LockTimeout: 3 * time.Second,
		},
		SQS: config.SQS{Enabled: false},
		References: config.References{
			WorkerEnabled: true, PollInterval: 50 * time.Millisecond,
			BaseDelay: 100 * time.Millisecond, MaxDelay: time.Second, MaxAttempts: 5,
		},
		Auth:         config.Auth{Issuer: idptest.Issuer(), Audience: "wallet-api"},
		StartTimeout: 10 * time.Second, ShutdownTimeout: 10 * time.Second,
	}
}

// App é a aplicação rodando para um teste.
type App struct {
	DB     *pgtest.DB
	Client *Client
	Logs   *LogBuffer // logs JSON da instância (nível INFO), para asserções
}

// LogBuffer guarda os logs JSON de uma instância. Seguro para goroutines: o
// handler JSON do slog faz uma escrita por registro.
type LogBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *LogBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

// String devolve o texto bruto (para procurar o que NÃO pode aparecer).
func (b *LogBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// Find devolve os registros cuja mensagem é msg.
func (b *LogBuffer) Find(msg string) []map[string]any {
	var out []map[string]any
	for _, line := range strings.Split(b.String(), "\n") {
		var rec map[string]any
		if json.Unmarshal([]byte(line), &rec) == nil && rec["msg"] == msg {
			out = append(out, rec)
		}
	}
	return out
}

// Start cria um banco descartável, sobe a aplicação e registra o shutdown.
func Start(t *testing.T) *App {
	t.Helper()
	a, _ := StartOn(t, pgtest.New(t), nil)
	return a
}

// StartOn sobe uma instância da aplicação sobre um banco existente, com a
// configuração ajustada por mutate. Devolve também uma função para parar a
// instância antes do fim do teste (simula reinício ou queda de instância).
func StartOn(t *testing.T, db *pgtest.DB, mutate func(*config.Config)) (*App, func()) {
	t.Helper()
	cfg := Config(db.AppURL)
	if mutate != nil {
		mutate(&cfg)
	}
	var srv *httpapi.Server
	logs := &LogBuffer{}
	captured := fx.Decorate(func(*slog.Logger) *slog.Logger {
		return fxapp.NewLogger(logs, slog.LevelInfo, cfg.InstanceID)
	})
	app := fxtest.New(t, fxapp.New(cfg, captured, fx.Populate(&srv)))
	app.RequireStart()
	var once sync.Once
	stop := func() { once.Do(app.RequireStop) }
	t.Cleanup(stop)
	return &App{DB: db, Client: NewClient(t, "http://"+srv.Addr()), Logs: logs}, stop
}

// Client é um cliente HTTP mínimo para os testes.
type Client struct {
	t    testing.TB
	base string
	http *http.Client
	// fixed, quando definido, força este token (ou nenhum, se fixedNone)
	// em todas as requisições: usado nos testes de autenticação.
	fixed     string
	fixedNone bool
}

func NewClient(t testing.TB, base string) *Client {
	return &Client{t: t, base: base, http: &http.Client{
		Timeout:   15 * time.Second,
		Transport: &http.Transport{MaxIdleConnsPerHost: 100},
	}}
}

// As devolve um cliente que usa sempre o token informado ("" = sem token).
func (c *Client) As(token string) *Client {
	cp := *c
	cp.fixed, cp.fixedNone = token, token == ""
	return &cp
}

// AsClient devolve um cliente que usa o token real do cliente OAuth indicado.
func (c *Client) AsClient(clientID string) *Client {
	return c.As(idptest.Token(c.t, clientID))
}

// tokenFor escolhe a identidade adequada para cada rota, imitando quem
// chamaria aquela rota de verdade:
//   - /wallets...                 -> serviço interno (wallet-admin)
//   - /providers/{p}/...          -> o próprio provedor p
//   - POST /wagering/transactions -> o provedor do corpo (ver Submit)
//   - GET /wagering/transactions  -> serviço interno
func (c *Client) tokenFor(method, path string) string {
	switch {
	case c.fixedNone:
		return ""
	case c.fixed != "":
		return c.fixed
	case strings.HasPrefix(path, "/providers/"):
		p, _, _ := strings.Cut(strings.TrimPrefix(path, "/providers/"), "/")
		return idptest.Token(c.t, p)
	case strings.HasPrefix(path, "/health/"):
		return ""
	default:
		return idptest.Token(c.t, idptest.WalletService)
	}
}

// Base devolve o endereço base (ex.: http://127.0.0.1:8080).
func (c *Client) Base() string { return c.base }

// Response guarda status, cabeçalhos e corpo decodificado.
type Response struct {
	Status int
	Header http.Header
	Body   map[string]any
}

func (r Response) Str(key string) string {
	v, _ := r.Body[key].(string)
	return v
}

// Amount devolve body[key].amount (ex.: "balance").
func (r Response) Amount(key string) string {
	m, _ := r.Body[key].(map[string]any)
	v, _ := m["amount"].(string)
	return v
}

// ErrorCode devolve body.error.code.
func (r Response) ErrorCode() string {
	m, _ := r.Body["error"].(map[string]any)
	v, _ := m["code"].(string)
	return v
}

// Do envia uma requisição. headers alterna chave e valor.
func (c *Client) Do(method, path string, body any, headers ...string) Response {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			c.t.Fatalf("marshal: %v", err)
		}
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequestWithContext(context.Background(), method, c.base+path, rd)
	req.Header.Set("Content-Type", "application/json")
	if tok := c.tokenFor(method, path); tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	resp, err := c.http.Do(req)
	if err != nil {
		c.t.Errorf("%s %s: %v", method, path, err)
		return Response{}
	}
	defer resp.Body.Close()
	out := Response{Status: resp.StatusCode, Header: resp.Header}
	_ = json.NewDecoder(resp.Body).Decode(&out.Body)
	return out
}

// OpenWallet abre uma carteira e devolve seu id.
func (c *Client) OpenWallet(playerID, amount string) string {
	c.t.Helper()
	r := c.Do("POST", "/wallets", map[string]any{
		"playerId": playerID, "initialBalance": map[string]string{"amount": amount, "currency": "BRL"},
	})
	if r.Status != http.StatusCreated {
		c.t.Fatalf("open wallet: %d %v", r.Status, r.Body)
	}
	return r.Str("id")
}

// Op descreve uma operação de provedor.
type Op struct {
	Provider, ExternalID, Key        string
	PlayerID, WalletID               string
	Round, Game, Kind, Amount, RefID string
}

// Body monta o corpo JSON da operação.
func (o Op) Body() map[string]any {
	b := map[string]any{
		"providerId": o.Provider, "externalTransactionId": o.ExternalID,
		"playerId": o.PlayerID, "walletId": o.WalletID,
		"roundId": o.Round, "gameId": o.Game, "kind": o.Kind,
		"money": map[string]string{"amount": o.Amount, "currency": "BRL"},
	}
	if o.RefID != "" {
		b["referenceExternalTransactionId"] = o.RefID
	}
	return b
}

// Submit envia a operação com a chave de idempotência.
func (c *Client) Submit(o Op) Response {
	key := o.Key
	if key == "" {
		key = o.Provider + ":" + o.ExternalID
	}
	if c.fixed == "" && !c.fixedNone {
		// o provedor do corpo envia com o próprio token
		return c.As(idptest.Token(c.t, o.Provider)).Do("POST", "/wagering/transactions", o.Body(), httpapi.HeaderIdempotencyKey, key)
	}
	return c.Do("POST", "/wagering/transactions", o.Body(), httpapi.HeaderIdempotencyKey, key)
}

// AssertReconciled confere, para TODAS as carteiras, que o saldo armazenado
// é igual a créditos menos débitos do ledger.
func AssertReconciled(t testing.TB, db *pgtest.DB) {
	t.Helper()
	rows, err := db.Owner.Query(context.Background(), `
		SELECT w.id, w.balance_minor,
		       COALESCE(SUM(CASE l.direction WHEN 'CREDIT' THEN l.amount_minor ELSE -l.amount_minor END), 0)
		  FROM wallets w LEFT JOIN wallet_ledger_entries l ON l.wallet_id = w.id
		 GROUP BY w.id, w.balance_minor`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var stored, fromLedger int64
		if err := rows.Scan(&id, &stored, &fromLedger); err != nil {
			t.Fatal(err)
		}
		if stored != fromLedger {
			t.Errorf("carteira %s: saldo %d != ledger %d", id, stored, fromLedger)
		}
	}
}

// Count executa um SELECT count(*) ... e devolve o número.
func Count(t testing.TB, db *pgtest.DB, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := db.Owner.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	return n
}

// Metrics lê GET /metrics (público, sem token) e devolve o texto no formato
// de exposição do Prometheus.
func (c *Client) Metrics() string {
	c.t.Helper()
	resp, err := c.http.Get(c.base + "/metrics")
	if err != nil {
		c.t.Fatalf("GET /metrics: %v", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		c.t.Fatalf("GET /metrics: %d %s", resp.StatusCode, b)
	}
	return string(b)
}

// MetricValue procura uma série exata no texto do /metrics, com os rótulos
// em ordem alfabética, como o Prometheus os escreve. Exemplos:
//
//	reconciliation_mismatch_total
//	wager_transactions_total{kind="BET",source="http",status="PROCESSED"}
func MetricValue(text, series string) (float64, bool) {
	for _, line := range strings.Split(text, "\n") {
		rest, ok := strings.CutPrefix(line, series+" ")
		if !ok {
			continue
		}
		v, err := strconv.ParseFloat(strings.TrimSpace(rest), 64)
		return v, err == nil
	}
	return 0, false
}

// WaitMetric espera a série chegar a want (alguns contadores sobem logo
// depois do efeito observável, como a remoção da mensagem da fila).
func (c *Client) WaitMetric(series string, want float64, timeout time.Duration) {
	c.t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		got, ok := MetricValue(c.Metrics(), series)
		if ok && got == want {
			return
		}
		if time.Now().After(deadline) {
			c.t.Fatalf("métrica %s = %v (presente: %v), want %v", series, got, ok, want)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// Must falha o teste se o status não for o esperado.
func Must(t testing.TB, r Response, status int, what string) Response {
	t.Helper()
	if r.Status != status {
		t.Fatalf("%s: status %d, want %d; body %v", what, r.Status, status, r.Body)
	}
	return r
}

// WaitStatus consulta a operação até ela chegar ao status esperado ou o
// prazo acabar. Devolve a última resposta.
func (c *Client) WaitStatus(providerID, externalID, status string, timeout time.Duration) Response {
	c.t.Helper()
	deadline := time.Now().Add(timeout)
	var r Response
	for time.Now().Before(deadline) {
		r = c.Do("GET", "/providers/"+providerID+"/wagering/transactions/"+externalID, nil)
		if r.Str("status") == status {
			return r
		}
		time.Sleep(20 * time.Millisecond)
	}
	c.t.Fatalf("%s/%s não chegou a %s em %v; último: %d %v", providerID, externalID, status, timeout, r.Status, r.Body)
	return r
}
