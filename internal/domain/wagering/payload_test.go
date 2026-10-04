package wagering_test

import (
	"testing"

	"github.com/google/uuid"

	"github.com/Taino-Edu/Desafio-go-arqui/internal/domain/money"
	"github.com/Taino-Edu/Desafio-go-arqui/internal/domain/wagering"
)

func basePayload(t *testing.T) wagering.BusinessPayload {
	return wagering.BusinessPayload{
		ProviderID: "provider-a", ExternalTransactionID: "transaction-123",
		PlayerID: uuid.MustParse("0192f28f-5dc0-7d58-bdb2-814ad6a0f4a1"),
		WalletID: uuid.MustParse("0192f291-27dd-7d3f-8071-5f8685deef37"),
		RoundID:  "round-987", GameID: "fortune-chimp",
		Kind: wagering.KindBet, Money: brl(t, "25.00"),
	}
}

func TestCanonicalJSON_IsSortedAndCompact(t *testing.T) {
	b, err := basePayload(t).CanonicalJSON()
	if err != nil {
		t.Fatal(err)
	}
	want := `{"externalTransactionId":"transaction-123","gameId":"fortune-chimp","kind":"BET",` +
		`"money":{"amount":"25.00","currency":"BRL"},"playerId":"0192f28f-5dc0-7d58-bdb2-814ad6a0f4a1",` +
		`"providerId":"provider-a","roundId":"round-987","walletId":"0192f291-27dd-7d3f-8071-5f8685deef37"}`
	if string(b) != want {
		t.Errorf("canônico:\n got %s\nwant %s", b, want)
	}
}

func TestHash_StableAndSensitive(t *testing.T) {
	base := basePayload(t)
	h1, _ := base.Hash()
	h2, _ := base.Hash()
	if h1 != h2 || len(h1) != 64 {
		t.Fatalf("hash instável ou tamanho errado: %s / %s", h1, h2)
	}

	// UUID em maiúsculas na entrada vira o mesmo UUID (uuid.UUID é binário)
	upper := base
	upper.PlayerID = uuid.MustParse("0192F28F-5DC0-7D58-BDB2-814AD6A0F4A1")
	if h, _ := upper.Hash(); h != h1 {
		t.Error("UUID em maiúsculas deveria gerar o mesmo hash")
	}

	// cada campo de negócio muda o hash
	changes := map[string]func(p *wagering.BusinessPayload){
		"provedor":   func(p *wagering.BusinessPayload) { p.ProviderID = "provider-b" },
		"id externo": func(p *wagering.BusinessPayload) { p.ExternalTransactionID = "t-2" },
		"jogador":    func(p *wagering.BusinessPayload) { p.PlayerID = uuid.New() },
		"carteira":   func(p *wagering.BusinessPayload) { p.WalletID = uuid.New() },
		"rodada":     func(p *wagering.BusinessPayload) { p.RoundID = "round-1" },
		"jogo":       func(p *wagering.BusinessPayload) { p.GameID = "other" },
		"tipo":       func(p *wagering.BusinessPayload) { p.Kind = wagering.KindWin },
		"valor":      func(p *wagering.BusinessPayload) { p.Money = brl(t, "25.01") },
		"moeda":      func(p *wagering.BusinessPayload) { p.Money, _ = money.Parse("25.00", money.USD) },
		"referência": func(p *wagering.BusinessPayload) { p.ReferenceExternalTransactionID = "bet-1" },
	}
	for name, change := range changes {
		p := base
		change(&p)
		if h, _ := p.Hash(); h == h1 {
			t.Errorf("mudar %s não alterou o hash", name)
		}
	}

	if _, err := (wagering.BusinessPayload{}).Hash(); err == nil {
		t.Error("payload sem dinheiro deveria falhar")
	}
}
