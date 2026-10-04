//go:build integration

package fxapp_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/fx"
	"go.uber.org/fx/fxtest"

	"github.com/Taino-Edu/Desafio-go-arqui/internal/adapters/httpapi"
	"github.com/Taino-Edu/Desafio-go-arqui/internal/platform/config"
	"github.com/Taino-Edu/Desafio-go-arqui/internal/platform/fxapp"
	"github.com/Taino-Edu/Desafio-go-arqui/internal/testsupport/pgtest"
)

func testConfig(dbURL string) config.Config {
	return config.Config{
		InstanceID: "test",
		LogLevel:   slog.LevelWarn,
		HTTP: config.HTTP{
			Addr: "127.0.0.1:0", RequestTimeout: 5 * time.Second,
			ReadTimeout: 5 * time.Second, WriteTimeout: 5 * time.Second, IdleTimeout: 5 * time.Second,
		},
		Database: config.Database{
			URL: dbURL, MaxConns: 4, StatementTimeout: 5 * time.Second, LockTimeout: 2 * time.Second,
		},
		References: config.References{
			WorkerEnabled: true, PollInterval: 50 * time.Millisecond,
			BaseDelay: 100 * time.Millisecond, MaxDelay: time.Second, MaxAttempts: 5,
		},
		StartTimeout:    10 * time.Second,
		ShutdownTimeout: 10 * time.Second,
	}
}

// silencia os logs JSON durante os testes
var quietLogger = fx.Decorate(func(*slog.Logger) *slog.Logger {
	return slog.New(slog.NewJSONHandler(io.Discard, nil))
})

type client struct {
	t    *testing.T
	base string
}

func (c client) do(method, path, body string) (int, map[string]any) {
	c.t.Helper()
	req, _ := http.NewRequest(method, c.base+path, strings.NewReader(body))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

// Sobe a aplicação completa pelo Fx, usa a API e desliga, conferindo que o
// servidor parou de escutar e que o pool foi fechado.
func TestFxApp_LifecycleAndWalletAPI(t *testing.T) {
	db := pgtest.New(t)

	var (
		srv  *httpapi.Server
		pool *pgxpool.Pool
	)
	app := fxtest.New(t, fxapp.New(testConfig(db.AppURL), quietLogger, fx.Populate(&srv, &pool)))
	app.RequireStart()

	c := client{t: t, base: "http://" + srv.Addr()}
	player := uuid.NewString()

	if code, _ := c.do("GET", "/health/ready", ""); code != 200 {
		t.Fatalf("ready = %d", code)
	}

	code, body := c.do("POST", "/wallets", fmt.Sprintf(`{"playerId":%q,"initialBalance":{"amount":"1000.00","currency":"BRL"}}`, player))
	if code != 201 {
		t.Fatalf("open = %d %v", code, body)
	}
	walletID := body["id"].(string)
	if body["version"].(float64) != 1 || body["balance"].(map[string]any)["amount"] != "1000.00" {
		t.Errorf("wallet = %v", body)
	}

	if code, body := c.do("POST", "/wallets", fmt.Sprintf(`{"playerId":%q,"initialBalance":{"amount":"0.00","currency":"BRL"}}`, player)); code != 409 {
		t.Errorf("duplicada = %d %v", code, body)
	}
	if code, _ := c.do("POST", "/wallets", fmt.Sprintf(`{"playerId":%q,"initialBalance":{"amount":"0.00","currency":"USD"}}`, player)); code != 201 {
		t.Errorf("mesma pessoa em outra moeda = %d", code)
	}

	code, got := c.do("GET", "/wallets/"+walletID, "")
	if code != 200 || got["createdAt"] != body["createdAt"] {
		t.Errorf("get = %d; createdAt %v vs %v", code, got["createdAt"], body["createdAt"])
	}

	code, page := c.do("GET", "/wallets/"+walletID+"/ledger", "")
	items := page["items"].([]any)
	if code != 200 || len(items) != 1 || page["nextCursor"] != nil {
		t.Errorf("ledger = %d %v", code, page)
	}

	// abertura com saldo: 2 eventos; abertura com zero (USD): nenhum
	var outbox int
	if err := db.Owner.QueryRow(context.Background(), `SELECT count(*) FROM outbox_events`).Scan(&outbox); err != nil || outbox != 2 {
		t.Errorf("outbox = %d, %v", outbox, err)
	}

	for path, want := range map[string]int{
		"/wallets/nao-e-uuid":                        400,
		"/wallets/" + uuid.NewString():               404,
		"/wallets/" + walletID + "/ledger?limit=0":   400,
		"/wallets/" + walletID + "/ledger?cursor=xx": 400,
	} {
		if code, _ := c.do("GET", path, ""); code != want {
			t.Errorf("GET %s = %d, want %d", path, code, want)
		}
	}

	addr := srv.Addr()
	app.RequireStop()

	if conn, err := net.DialTimeout("tcp", addr, 500*time.Millisecond); err == nil {
		conn.Close()
		t.Error("o servidor deveria ter parado de escutar")
	}
	if err := pool.Ping(context.Background()); err == nil {
		t.Error("o pool deveria estar fechado depois do shutdown")
	}
}

// Sem banco, a aplicação não pode iniciar (validação de dependências).
func TestFxApp_FailsToStartWithoutDatabase(t *testing.T) {
	cfg := testConfig("postgres://wallet_app:wallet_app@127.0.0.1:1/wallet?sslmode=disable&connect_timeout=1")
	cfg.StartTimeout = 3 * time.Second

	app := fx.New(fxapp.New(cfg, quietLogger))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := app.Start(ctx); err == nil {
		_ = app.Stop(ctx)
		t.Fatal("deveria falhar ao iniciar sem banco")
	}
}

// A composição do Fx é válida (todas as dependências resolvem) sem iniciar nada.
func TestFxApp_GraphIsValid(t *testing.T) {
	if err := fx.ValidateApp(fxapp.New(testConfig("postgres://x@localhost/db"), quietLogger)); err != nil {
		t.Fatal(err)
	}
}
