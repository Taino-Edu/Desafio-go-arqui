package app

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Taino-Edu/Desafio-go-arqui/internal/domain/money"
	"github.com/Taino-Edu/Desafio-go-arqui/internal/domain/wagering"
)

var (
	providerA = Principal{Subject: "a", ClientID: "provider-a", ProviderID: "provider-a", Roles: []string{RoleProvider}}
	providerB = Principal{Subject: "b", ClientID: "provider-b", ProviderID: "provider-b", Roles: []string{RoleProvider}}
	admin     = Principal{Subject: "s", ClientID: "wallet-service", Roles: []string{RoleWalletAdmin}}
	nobody    = Principal{Subject: "x", ClientID: "other"}
	// papel de provedor sem a claim provider_id: não pode agir como provedor
	providerNoClaim = Principal{Subject: "y", Roles: []string{RoleProvider}}
)

func TestAuthz_Wallets(t *testing.T) {
	if admin.CanManageWallets() != nil {
		t.Error("admin deveria operar carteiras")
	}
	for _, p := range []Principal{providerA, nobody, providerNoClaim} {
		if !errors.Is(p.CanManageWallets(), ErrForbidden) {
			t.Errorf("%s não deveria operar carteiras", p.ClientID)
		}
	}
}

func TestAuthz_Submit(t *testing.T) {
	if providerA.CanSubmitFor("provider-a") != nil {
		t.Error("provider-a envia como provider-a")
	}
	cases := map[string]struct {
		p        Principal
		provider string
	}{
		"A em nome de B":        {providerA, "provider-b"},
		"B em nome de A":        {providerB, "provider-a"},
		"serviço interno":       {admin, "provider-a"},
		"sem papel":             {nobody, "provider-a"},
		"papel sem provider_id": {providerNoClaim, ""},
	}
	for name, c := range cases {
		if !errors.Is(c.p.CanSubmitFor(c.provider), ErrForbidden) {
			t.Errorf("%s deveria ser proibido", name)
		}
	}
}

func TestAuthz_ReadTransaction(t *testing.T) {
	amt, _ := money.Parse("1.00", money.BRL)
	tx, err := wagering.NewExternal(wagering.NewExternalParams{
		ID: uuid.New(), Kind: wagering.KindBet, WalletID: uuid.New(), PlayerID: uuid.New(), Amount: amt, Now: time.Now(),
		External: wagering.External{ProviderID: "provider-a", ExternalTransactionID: "e", IdempotencyKey: "k",
			PayloadHash: "h", RoundID: "r", GameID: "g"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if providerA.CanReadTransaction(tx) != nil || admin.CanReadTransaction(tx) != nil {
		t.Error("dono e serviço interno leem")
	}
	for _, p := range []Principal{providerB, nobody, providerNoClaim} {
		if !errors.Is(p.CanReadTransaction(tx), ErrTransactionNotFound) {
			t.Errorf("%s deveria receber 'não encontrada'", p.ClientID)
		}
	}
	if providerA.CanQueryProvider("provider-a") != nil || admin.CanQueryProvider("provider-b") != nil {
		t.Error("consulta por provedor permitida")
	}
	if !errors.Is(providerA.CanQueryProvider("provider-b"), ErrForbidden) {
		t.Error("A não consulta o namespace de B")
	}
}
