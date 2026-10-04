// Package idptest obtém access tokens REAIS do Keycloak do docker compose
// (fluxo client_credentials) para os testes de integração.
//
// Variáveis (opcionais):
//
//	TEST_OIDC_ISSUER  padrão http://localhost:8081/realms/wallet
package idptest

import (
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// Clientes do realm (deploy/keycloak/realm-wallet.json). Segredos de teste.
const (
	ProviderA     = "provider-a"
	ProviderB     = "provider-b"
	WalletService = "wallet-service"
	// ShortLived: provider-a com token de 1 segundo (teste de expiração).
	ShortLived = "test-provider-a-short-lived"
	// OtherAudience: provider-a sem a audiência wallet-api.
	OtherAudience = "test-other-audience"
)

var secrets = map[string]string{
	ProviderA:     "provider-a-secret",
	ProviderB:     "provider-b-secret",
	WalletService: "wallet-service-secret",
	ShortLived:    "test-short-lived-secret",
	OtherAudience: "test-other-audience-secret",
}

// Issuer devolve o emissor esperado nos tokens.
func Issuer() string {
	if v := os.Getenv("TEST_OIDC_ISSUER"); v != "" {
		return v
	}
	return "http://localhost:8081/realms/wallet"
}

type cached struct {
	token   string
	expires time.Time
}

var (
	mu    sync.Mutex
	cache = map[string]cached{}
)

// Token devolve um access token válido do cliente, com cache até perto de
// expirar.
func Token(t testing.TB, clientID string) string {
	t.Helper()
	mu.Lock()
	if c, ok := cache[clientID]; ok && time.Until(c.expires) > 30*time.Second {
		mu.Unlock()
		return c.token
	}
	mu.Unlock()

	tok, ttl := Fetch(t, clientID)
	mu.Lock()
	cache[clientID] = cached{token: tok, expires: time.Now().Add(ttl)}
	mu.Unlock()
	return tok
}

// Fetch pede um token novo, sem cache. Devolve o token e a validade.
func Fetch(t testing.TB, clientID string) (string, time.Duration) {
	t.Helper()
	secret, ok := secrets[clientID]
	if !ok {
		t.Fatalf("idptest: cliente desconhecido %q", clientID)
	}
	form := url.Values{"grant_type": {"client_credentials"}, "client_id": {clientID}, "client_secret": {secret}}
	resp, err := http.Post(strings.TrimRight(Issuer(), "/")+"/protocol/openid-connect/token",
		"application/x-www-form-urlencoded", strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatalf("idptest: Keycloak inacessível (%v). Suba com: docker compose up -d keycloak", err)
	}
	defer resp.Body.Close()
	var body struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil || resp.StatusCode != 200 || body.AccessToken == "" {
		t.Fatalf("idptest: token de %s: status %d, err %v", clientID, resp.StatusCode, err)
	}
	return body.AccessToken, time.Duration(body.ExpiresIn) * time.Second
}
