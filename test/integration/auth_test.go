//go:build integration

package integration

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Taino-Edu/Desafio-go-arqui/internal/testsupport/apptest"
	"github.com/Taino-Edu/Desafio-go-arqui/internal/testsupport/idptest"
)

// financialFootprint soma tudo o que um acesso não autorizado poderia ter
// alterado: se não mudou, não houve efeito financeiro.
func financialFootprint(t *testing.T, a *apptest.App) [5]int {
	return [5]int{
		apptest.Count(t, a.DB, `SELECT count(*) FROM wallets`),
		apptest.Count(t, a.DB, `SELECT count(*) FROM wager_transactions`),
		apptest.Count(t, a.DB, `SELECT count(*) FROM wallet_ledger_entries`),
		apptest.Count(t, a.DB, `SELECT count(*) FROM outbox_events`),
		apptest.Count(t, a.DB, `SELECT COALESCE(SUM(balance_minor), 0)::int FROM wallets`),
	}
}

func b64(v any) string {
	b, _ := json.Marshal(v)
	return base64.RawURLEncoding.EncodeToString(b)
}

func decodePayload(t *testing.T, tok string) map[string]any {
	parts := strings.Split(tok, ".")
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	_ = json.Unmarshal(raw, &m)
	return m
}

// forgeTokens monta tokens falsos a partir de um token verdadeiro.
func forgeTokens(t *testing.T) map[string]string {
	real := idptest.Token(t, idptest.ProviderA)
	parts := strings.Split(real, ".")
	claims := decodePayload(t, real)

	// 1. payload adulterado (provider-a -> provider-b), assinatura original
	claims["provider_id"] = "provider-b"
	tampered := parts[0] + "." + b64(claims) + "." + parts[2]
	claims["provider_id"] = "provider-a"

	// 2. alg "none", sem assinatura
	none := b64(map[string]string{"alg": "none", "typ": "JWT"}) + "." + b64(claims) + "."

	// 3. claims perfeitas, assinadas com uma chave que NÃO é do Keycloak
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	header := b64(map[string]string{"alg": "RS256", "typ": "JWT", "kid": "chave-do-atacante"})
	claims["exp"] = time.Now().Add(time.Hour).Unix()
	signingInput := header + "." + b64(claims)
	digest := sha256.Sum256([]byte(signingInput))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	selfSigned := signingInput + "." + base64.RawURLEncoding.EncodeToString(sig)

	return map[string]string{
		"lixo":                  "isto-nao-e-um-jwt",
		"payload adulterado":    tampered,
		"alg none":              none,
		"assinado por terceiro": selfSigned,
	}
}

// Credenciais ausentes, inválidas, expiradas ou de outra audiência: 401, sem
// nenhum efeito financeiro e sem expor dados.
func TestAuth_RejectsMissingInvalidAndExpiredCredentials(t *testing.T) {
	a := apptest.Start(t)
	player := uuid.NewString()
	wallet := a.Client.OpenWallet(player, "100.00")
	bet := apptest.Op{Provider: "provider-a", ExternalID: "bet-1", PlayerID: player, WalletID: wallet,
		Round: "r", Game: "g", Kind: "BET", Amount: "10.00"}
	before := financialFootprint(t, a)

	// token de 1 segundo, usado depois de expirar
	expired, _ := idptest.Fetch(t, idptest.ShortLived)
	time.Sleep(2500 * time.Millisecond)

	tokens := forgeTokens(t)
	tokens["sem token"] = ""
	tokens["expirado"] = expired
	tokens["outra audiência"] = idptest.Token(t, idptest.OtherAudience)

	routes := []struct{ method, path string }{
		{"POST", "/wallets"},
		{"GET", "/wallets/" + wallet},
		{"GET", "/wallets/" + wallet + "/ledger"},
		{"GET", "/wagering/transactions/" + uuid.NewString()},
		{"GET", "/providers/provider-a/wagering/transactions/bet-1"},
	}
	for name, tok := range tokens {
		c := a.Client.As(tok)
		if r := c.Submit(bet); r.Status != http.StatusUnauthorized || r.ErrorCode() != "UNAUTHENTICATED" {
			t.Errorf("%s: aposta = %d %v", name, r.Status, r.Body)
		}
		for _, rt := range routes {
			body := any(nil)
			if rt.method == "POST" {
				body = map[string]any{"playerId": uuid.NewString(), "initialBalance": map[string]string{"amount": "1.00", "currency": "BRL"}}
			}
			r := c.Do(rt.method, rt.path, body)
			if r.Status != http.StatusUnauthorized {
				t.Errorf("%s: %s %s = %d %v", name, rt.method, rt.path, r.Status, r.Body)
			}
			if _, leaked := r.Body["balance"]; leaked {
				t.Errorf("%s: %s %s expôs dados", name, rt.method, rt.path)
			}
		}
	}

	if after := financialFootprint(t, a); after != before {
		t.Errorf("acessos não autenticados alteraram dados: %v -> %v", before, after)
	}

	// health continua público
	if r := a.Client.As("").Do("GET", "/health/ready", nil); r.Status != 200 {
		t.Errorf("health = %d", r.Status)
	}
	// o cabeçalho WWW-Authenticate orienta o cliente
	req, _ := http.NewRequest("GET", a.Client.Base()+"/wallets/"+wallet, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if !strings.HasPrefix(resp.Header.Get("WWW-Authenticate"), "Bearer") {
		t.Errorf("WWW-Authenticate = %q", resp.Header.Get("WWW-Authenticate"))
	}
}

// O providerId vem da identidade. Um provedor não age em nome de outro, nem
// lê ou reproduz (replay) operações de outro.
func TestAuth_ProviderIsolation(t *testing.T) {
	a := apptest.Start(t)
	player := uuid.NewString()
	wallet := a.Client.OpenWallet(player, "100.00")
	asA := a.Client.AsClient(idptest.ProviderA)
	asB := a.Client.AsClient(idptest.ProviderB)

	betA := apptest.Op{Provider: "provider-a", ExternalID: "bet-1", PlayerID: player, WalletID: wallet,
		Round: "r", Game: "g", Kind: "BET", Amount: "10.00"}
	created := apptest.Must(t, asA.Submit(betA), http.StatusCreated, "A aposta")
	txA := created.Str("transactionId")
	before := financialFootprint(t, a)

	t.Run("B não envia em nome de A (nem replay da chave de A)", func(t *testing.T) {
		r := asB.Submit(betA) // corpo de A, mesma chave, token de B
		if r.Status != http.StatusForbidden || r.ErrorCode() != "FORBIDDEN" {
			t.Errorf("= %d %v", r.Status, r.Body)
		}
		if _, leaked := r.Body["balance"]; leaked {
			t.Error("o replay de A não pode vazar para B")
		}
	})
	t.Run("A não envia em nome de B", func(t *testing.T) {
		op := betA
		op.Provider, op.ExternalID = "provider-b", "bet-x"
		if r := asA.Submit(op); r.Status != http.StatusForbidden {
			t.Errorf("= %d %v", r.Status, r.Body)
		}
	})
	t.Run("B não lê a transação de A", func(t *testing.T) {
		if r := asB.Do("GET", "/wagering/transactions/"+txA, nil); r.Status != http.StatusNotFound {
			t.Errorf("por id = %d %v (deve parecer inexistente)", r.Status, r.Body)
		}
		if r := asB.Do("GET", "/providers/provider-a/wagering/transactions/bet-1", nil); r.Status != http.StatusForbidden {
			t.Errorf("namespace de A = %d %v", r.Status, r.Body)
		}
	})
	t.Run("A lê a própria; o serviço interno lê qualquer uma", func(t *testing.T) {
		apptest.Must(t, asA.Do("GET", "/wagering/transactions/"+txA, nil), 200, "A por id")
		apptest.Must(t, asA.Do("GET", "/providers/provider-a/wagering/transactions/bet-1", nil), 200, "A por id externo")
		apptest.Must(t, a.Client.AsClient(idptest.WalletService).Do("GET", "/wagering/transactions/"+txA, nil), 200, "interno")
	})
	t.Run("provedores não operam carteiras", func(t *testing.T) {
		for _, c := range []*apptest.Client{asA, asB} {
			if r := c.Do("POST", "/wallets", map[string]any{"playerId": uuid.NewString(),
				"initialBalance": map[string]string{"amount": "999.00", "currency": "BRL"}}); r.Status != http.StatusForbidden {
				t.Errorf("abrir carteira = %d", r.Status)
			}
			if r := c.Do("GET", "/wallets/"+wallet, nil); r.Status != http.StatusForbidden {
				t.Errorf("ler carteira = %d", r.Status)
			}
			if r := c.Do("GET", "/wallets/"+wallet+"/ledger", nil); r.Status != http.StatusForbidden {
				t.Errorf("ledger = %d", r.Status)
			}
		}
	})
	t.Run("o serviço interno não envia operações de provedor", func(t *testing.T) {
		op := betA
		op.ExternalID = "bet-internal"
		if r := a.Client.AsClient(idptest.WalletService).Submit(op); r.Status != http.StatusForbidden {
			t.Errorf("= %d %v", r.Status, r.Body)
		}
	})

	if after := financialFootprint(t, a); after != before {
		t.Errorf("acessos proibidos alteraram dados: %v -> %v", before, after)
	}

	// o mesmo id externo em B é outra operação, no espaço de B
	opB := betA
	opB.Provider = "provider-b"
	r := apptest.Must(t, asB.Submit(opB), http.StatusCreated, "B com o próprio namespace")
	if r.Str("transactionId") == txA || r.Body["idempotentReplay"] != false {
		t.Errorf("B recebeu o resultado de A: %v", r.Body)
	}
}
