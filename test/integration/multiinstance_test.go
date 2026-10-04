//go:build integration

package integration

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Taino-Edu/Desafio-go-arqui/internal/testsupport/apptest"
	"github.com/Taino-Edu/Desafio-go-arqui/internal/testsupport/idptest"
	"github.com/Taino-Edu/Desafio-go-arqui/internal/testsupport/pgtest"
)

var (
	buildOnce sync.Once
	binPath   string
	buildErr  error
)

// serverBinary compila cmd/server uma vez por execução dos testes.
func serverBinary(t *testing.T) string {
	t.Helper()
	buildOnce.Do(func() {
		dir, err := os.MkdirTemp("", "wallet-server-")
		if err != nil {
			buildErr = err
			return
		}
		binPath = filepath.Join(dir, "server")
		cmd := exec.Command("go", "build", "-o", binPath, "../../cmd/server")
		cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
		if out, err := cmd.CombinedOutput(); err != nil {
			buildErr = fmt.Errorf("go build: %v\n%s", err, out)
		}
	})
	if buildErr != nil {
		t.Fatal(buildErr)
	}
	return binPath
}

func freePort(t *testing.T) int {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// instance é um processo independente do servidor: memória, pool de
// conexões e ciclo de vida próprios.
type instance struct {
	name   string
	cmd    *exec.Cmd
	client *apptest.Client
	logs   *strings.Builder
}

// startInstances sobe n processos apontando para o mesmo banco.
func startInstances(t *testing.T, db *pgtest.DB, n int) []*instance {
	t.Helper()
	bin := serverBinary(t)
	out := make([]*instance, n)
	for i := range out {
		port := freePort(t)
		logs := &strings.Builder{}
		cmd := exec.Command(bin)
		cmd.Env = append(os.Environ(),
			"DATABASE_URL="+db.AppURL,
			fmt.Sprintf("HTTP_ADDR=127.0.0.1:%d", port),
			fmt.Sprintf("INSTANCE_ID=instance-%d", i+1),
			"DB_MAX_CONNS=10", "LOG_LEVEL=warn",
			"OIDC_ISSUER="+idptest.Issuer(), "OIDC_AUDIENCE=wallet-api",
			"SQS_ENABLED=false", "OUTBOX_PUBLISHER_ENABLED=false",
		)
		cmd.Stdout, cmd.Stderr = logs, logs
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		inst := &instance{name: fmt.Sprintf("instance-%d", i+1), cmd: cmd, logs: logs,
			client: apptest.NewClient(t, fmt.Sprintf("http://127.0.0.1:%d", port))}
		out[i] = inst
		t.Cleanup(func() { inst.stop(t) })
	}
	for _, inst := range out {
		inst.waitReady(t)
	}
	return out
}

func (i *instance) waitReady(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
		req, _ := http.NewRequestWithContext(ctx, "GET", i.client.Base()+"/health/ready", nil)
		resp, err := http.DefaultClient.Do(req)
		cancel()
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == 200 {
				return
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("%s não ficou pronta:\n%s", i.name, i.logs.String())
}

// stop envia SIGTERM e espera o shutdown gracioso.
func (i *instance) stop(t *testing.T) {
	if i.cmd.ProcessState != nil {
		return
	}
	_ = i.cmd.Process.Signal(syscall.SIGTERM)
	done := make(chan error, 1)
	go func() { done <- i.cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("%s terminou com erro: %v\n%s", i.name, err, i.logs.String())
		}
	case <-time.After(15 * time.Second):
		_ = i.cmd.Process.Kill()
		t.Errorf("%s não desligou a tempo", i.name)
	}
}

// Os cenários obrigatórios repetidos com 3 processos independentes. As
// requisições de uma mesma carteira são espalhadas entre as instâncias, então
// a única coordenação possível é a do banco.
func TestThreeIndependentInstances(t *testing.T) {
	db := pgtest.New(t)
	insts := startInstances(t, db, 3)
	pick := func(i int) *apptest.Client { return insts[i%len(insts)].client }

	t.Run("mesma aposta 51 vezes, 17 em cada instância", func(t *testing.T) {
		player := uuid.NewString()
		wallet := pick(0).OpenWallet(player, "100.00")
		bet := apptest.Op{Provider: "provider-a", ExternalID: "dup-1", PlayerID: player, WalletID: wallet,
			Round: "r", Game: "g", Kind: "BET", Amount: "10.00"}

		results := make([]apptest.Response, 51)
		parallel(51, func(i int) { results[i] = pick(i).Submit(bet) })

		created := 0
		for _, r := range results {
			if r.Status == http.StatusCreated {
				created++
			} else if r.Status != http.StatusOK || r.Body["idempotentReplay"] != true {
				t.Errorf("resposta inesperada: %d %v", r.Status, r.Body)
			}
		}
		debits := apptest.Count(t, db, `SELECT count(*) FROM wallet_ledger_entries WHERE wallet_id = $1 AND direction = 'DEBIT'`, wallet)
		if created != 1 || debits != 1 {
			t.Errorf("criadas %d, débitos %d", created, debits)
		}
	})

	t.Run("80 + 80 sobre 100, cada aposta em uma instância diferente", func(t *testing.T) {
		const wallets = 12
		type pair struct{ player, wallet string }
		ws := make([]pair, wallets)
		for i := range ws {
			p := uuid.NewString()
			ws[i] = pair{p, pick(i).OpenWallet(p, "100.00")}
		}
		results := make([]apptest.Response, wallets*2)
		parallel(wallets*2, func(i int) {
			w := ws[i/2]
			// aposta 0 na instância i, aposta 1 na instância i+1
			results[i] = pick(i/2 + i%2).Submit(apptest.Op{Provider: "provider-a",
				ExternalID: fmt.Sprintf("%s-%d", w.wallet, i%2), PlayerID: w.player, WalletID: w.wallet,
				Round: "r", Game: "g", Kind: "BET", Amount: "80.00"})
		})
		for i, w := range ws {
			s1, s2 := results[2*i].Str("status"), results[2*i+1].Str("status")
			if !((s1 == "PROCESSED" && s2 == "REJECTED") || (s1 == "REJECTED" && s2 == "PROCESSED")) {
				t.Errorf("carteira %d: %v / %v", i, results[2*i].Body, results[2*i+1].Body)
			}
			got := pick(i+2).Do("GET", "/wallets/"+w.wallet, nil)
			if got.Amount("balance") != "20.00" {
				t.Errorf("carteira %d: saldo %s", i, got.Amount("balance"))
			}
		}
	})

	t.Run("carteiras distintas em paralelo entre instâncias", func(t *testing.T) {
		const wallets, bets = 9, 6
		type pair struct{ player, wallet string }
		ws := make([]pair, wallets)
		for i := range ws {
			p := uuid.NewString()
			ws[i] = pair{p, pick(i).OpenWallet(p, "50.00")}
		}
		parallel(wallets*bets, func(i int) {
			w := ws[i%wallets]
			r := pick(i).Submit(apptest.Op{Provider: "provider-a", ExternalID: fmt.Sprintf("%s-m%d", w.wallet, i),
				PlayerID: w.player, WalletID: w.wallet, Round: "r", Game: "g", Kind: "BET", Amount: "5.00"})
			if r.Status != http.StatusCreated {
				t.Errorf("aposta %d: %d %v", i, r.Status, r.Body)
			}
		})
		for i, w := range ws {
			if got := pick(i).Do("GET", "/wallets/"+w.wallet, nil); got.Amount("balance") != "20.00" {
				t.Errorf("carteira %d: %v", i, got.Body)
			}
		}
	})

	apptest.AssertReconciled(t, db)
}
