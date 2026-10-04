package httpapi

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/Taino-Edu/Desafio-go-arqui/internal/app"
)

// TokenVerifier valida um access token e devolve a identidade.
type TokenVerifier interface {
	Verify(ctx context.Context, rawToken string) (app.Principal, error)
}

// withAuth exige "Authorization: Bearer <token>" válido. Sem token, ou com
// token inválido ou expirado: 401, e o handler nem é chamado (nenhum efeito
// financeiro, nenhum dado exposto). O token nunca vai para os logs.
func withAuth(v TokenVerifier, log *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, ok := bearerToken(r)
		if !ok {
			w.Header().Set("WWW-Authenticate", `Bearer realm="wallet"`)
			writeError(w, r, log, app.ErrUnauthenticated)
			return
		}
		p, err := v.Verify(r.Context(), raw)
		if err != nil {
			if errors.Is(err, app.ErrUnauthenticated) {
				w.Header().Set("WWW-Authenticate", `Bearer realm="wallet", error="invalid_token"`)
				log.InfoContext(r.Context(), "authentication rejected", "reason", err.Error())
			}
			writeError(w, r, log, err)
			return
		}
		next.ServeHTTP(w, r.WithContext(app.WithPrincipal(r.Context(), p)))
	})
}

func bearerToken(r *http.Request) (string, bool) {
	h := r.Header.Get("Authorization")
	scheme, token, ok := strings.Cut(h, " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return "", false
	}
	token = strings.TrimSpace(token)
	return token, token != ""
}

// principal devolve a identidade autenticada. As rotas de negócio só são
// alcançáveis através de withAuth, então a ausência é um erro de montagem.
func principal(r *http.Request) (app.Principal, error) {
	p, ok := app.PrincipalFrom(r.Context())
	if !ok {
		return app.Principal{}, app.ErrUnauthenticated
	}
	return p, nil
}
