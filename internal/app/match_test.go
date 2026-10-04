package app

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Taino-Edu/Desafio-go-arqui/internal/domain/money"
	"github.com/Taino-Edu/Desafio-go-arqui/internal/domain/wagering"
)

func TestMatchExisting(t *testing.T) {
	amt, _ := money.Parse("10.00", money.BRL)
	existing, err := wagering.NewExternal(wagering.NewExternalParams{
		ID: uuid.New(), Kind: wagering.KindBet, WalletID: uuid.New(), PlayerID: uuid.New(), Amount: amt, Now: time.Now(),
		External: wagering.External{ProviderID: "p", ExternalTransactionID: "ext-1", IdempotencyKey: "key-1",
			PayloadHash: "hash-1", RoundID: "r", GameID: "g"},
	})
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name           string
		key, ext, hash string
		want           error
	}{
		{"mesma chave, mesmo conteúdo: replay", "key-1", "ext-1", "hash-1", nil},
		{"mesma chave, conteúdo diferente", "key-1", "ext-1", "hash-2", ErrIdempotencyKeyReused},
		{"mesma chave, outro id externo", "key-1", "ext-2", "hash-1", ErrIdempotencyKeyReused},
		{"mesmo id externo, outra chave", "key-2", "ext-1", "hash-1", ErrDuplicateTransaction},
	}
	for _, tt := range tests {
		if err := matchExisting(existing, tt.key, tt.ext, tt.hash); !errors.Is(err, tt.want) || (tt.want == nil && err != nil) {
			t.Errorf("%s: err = %v, want %v", tt.name, err, tt.want)
		}
	}
}
