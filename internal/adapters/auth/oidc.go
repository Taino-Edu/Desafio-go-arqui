// Package auth valida access tokens JWT emitidos por um IdP OAuth 2.0/OIDC
// (Keycloak) e os converte na identidade da aplicação (app.Principal).
//
// O serviço não emite tokens nem guarda senhas: só verifica
//   - a assinatura, com as chaves públicas do IdP (JWKS, com cache e
//     renovação automática quando aparece uma chave nova);
//   - o emissor (iss), a audiência (aud contém wallet-api) e a validade (exp);
//   - o tipo (typ = Bearer), para recusar ID tokens usados como access token.
package auth

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"

	"github.com/Taino-Edu/Desafio-go-arqui/internal/app"
)

// Config descreve o IdP.
type Config struct {
	Issuer   string // ex.: http://localhost:8081/realms/wallet
	Audience string // ex.: wallet-api
	// JWKSURL é onde buscar as chaves. Pode diferir do emissor quando o
	// serviço alcança o IdP por outro endereço (ex.: keycloak:8080 dentro do
	// docker compose). Vazio: <Issuer>/protocol/openid-connect/certs.
	JWKSURL string
}

// Verifier valida tokens.
type Verifier struct {
	verifier *oidc.IDTokenVerifier
}

// NewVerifier prepara a validação. Não acessa a rede: as chaves são buscadas
// no primeiro uso e mantidas em cache.
func NewVerifier(cfg Config) (*Verifier, error) {
	if cfg.Issuer == "" || cfg.Audience == "" {
		return nil, errors.New("auth: issuer and audience are required")
	}
	jwks := cfg.JWKSURL
	if jwks == "" {
		jwks = strings.TrimRight(cfg.Issuer, "/") + "/protocol/openid-connect/certs"
	}
	httpClient := &http.Client{Timeout: 5 * time.Second}
	keySet := oidc.NewRemoteKeySet(oidc.ClientContext(context.Background(), httpClient), jwks)
	return &Verifier{verifier: oidc.NewVerifier(cfg.Issuer, keySet, &oidc.Config{
		ClientID:             cfg.Audience, // exige que aud contenha a audiência
		SupportedSigningAlgs: []string{oidc.RS256},
	})}, nil
}

type claims struct {
	Typ         string `json:"typ"`
	Azp         string `json:"azp"`
	ProviderID  string `json:"provider_id"`
	RealmAccess struct {
		Roles []string `json:"roles"`
	} `json:"realm_access"`
}

// Verify valida o token e devolve a identidade. Erros:
//   - app.ErrUnauthenticated: token inválido, expirado, de outro emissor ou
//     de outra audiência;
//   - app.ErrTransient: não foi possível buscar as chaves do IdP.
func (v *Verifier) Verify(ctx context.Context, raw string) (app.Principal, error) {
	tok, err := v.verifier.Verify(ctx, raw)
	if err != nil {
		if isNetworkError(err) {
			return app.Principal{}, fmt.Errorf("%w: identity provider unreachable: %v", app.ErrTransient, err)
		}
		return app.Principal{}, fmt.Errorf("%w: %v", app.ErrUnauthenticated, err)
	}
	var c claims
	if err := tok.Claims(&c); err != nil {
		return app.Principal{}, fmt.Errorf("%w: claims: %v", app.ErrUnauthenticated, err)
	}
	if !strings.EqualFold(c.Typ, "Bearer") {
		return app.Principal{}, fmt.Errorf("%w: not an access token (typ=%q)", app.ErrUnauthenticated, c.Typ)
	}
	return app.Principal{
		Subject: tok.Subject, ClientID: c.Azp, ProviderID: c.ProviderID, Roles: c.RealmAccess.Roles,
	}, nil
}

func isNetworkError(err error) bool {
	var netErr net.Error
	var urlErr *url.Error
	return errors.As(err, &netErr) || errors.As(err, &urlErr)
}
