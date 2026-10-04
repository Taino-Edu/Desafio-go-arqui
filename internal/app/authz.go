package app

import (
	"context"
	"errors"

	"github.com/Taino-Edu/Desafio-go-arqui/internal/domain/wagering"
)

// Papéis do IdP (realm roles do Keycloak).
const (
	// RoleProvider: provedor de jogos. Envia e consulta SÓ as próprias
	// operações; o provedor vem da claim provider_id do token.
	RoleProvider = "provider"
	// RoleWalletAdmin: serviço interno. Abre e consulta carteiras, ledger e
	// reconciliação, e consulta qualquer transação. Não envia operações de
	// provedor (não tem provider_id).
	RoleWalletAdmin = "wallet-admin"
)

var (
	// ErrUnauthenticated: sem credencial válida (token ausente, inválido,
	// expirado ou de outra audiência). HTTP 401.
	ErrUnauthenticated = errors.New("unauthenticated")

	// ErrForbidden: identidade válida, mas sem permissão. HTTP 403.
	ErrForbidden = errors.New("forbidden")
)

// Principal é a identidade autenticada, extraída e validada do token.
type Principal struct {
	Subject    string // sub
	ClientID   string // azp: o cliente OAuth que obteve o token
	ProviderID string // claim provider_id (só provedores)
	Roles      []string
}

// HasRole informa se a identidade tem o papel.
func (p Principal) HasRole(role string) bool {
	for _, r := range p.Roles {
		if r == role {
			return true
		}
	}
	return false
}

// CanManageWallets: só o serviço interno opera carteiras.
func (p Principal) CanManageWallets() error {
	if p.HasRole(RoleWalletAdmin) {
		return nil
	}
	return ErrForbidden
}

// CanSubmitFor: só um provedor pode enviar operações, e só em nome dele
// mesmo. O providerId do corpo precisa ser igual ao da identidade: o servidor
// não confia no corpo para decidir quem é o provedor.
func (p Principal) CanSubmitFor(providerID string) error {
	if !p.HasRole(RoleProvider) || p.ProviderID == "" {
		return ErrForbidden
	}
	if providerID != p.ProviderID {
		return ErrForbidden
	}
	return nil
}

// CanQueryProvider: consultar operações de um provedor pelo id externo.
func (p Principal) CanQueryProvider(providerID string) error {
	if p.HasRole(RoleWalletAdmin) {
		return nil
	}
	if p.HasRole(RoleProvider) && p.ProviderID != "" && p.ProviderID == providerID {
		return nil
	}
	return ErrForbidden
}

// CanReadTransaction: o serviço interno lê qualquer uma; um provedor só as
// próprias. Para outro provedor a resposta é "não encontrada", para não
// revelar que a transação existe.
func (p Principal) CanReadTransaction(tx *wagering.WagerTransaction) error {
	if p.HasRole(RoleWalletAdmin) {
		return nil
	}
	ext := tx.External()
	if p.HasRole(RoleProvider) && p.ProviderID != "" && ext != nil && ext.ProviderID == p.ProviderID {
		return nil
	}
	return ErrTransactionNotFound
}

type principalKey struct{}

// WithPrincipal guarda a identidade no contexto da requisição.
func WithPrincipal(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, principalKey{}, p)
}

// PrincipalFrom devolve a identidade do contexto.
func PrincipalFrom(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(principalKey{}).(Principal)
	return p, ok
}
