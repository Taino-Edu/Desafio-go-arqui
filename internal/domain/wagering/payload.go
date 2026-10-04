package wagering

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"

	"github.com/google/uuid"

	"github.com/Taino-Edu/Desafio-go-arqui/internal/domain/money"
)

// BusinessPayload são os campos de negócio de uma operação externa: tudo o
// que define "o que" está sendo feito. Ficam DE FORA a chave de idempotência
// e qualquer metadado de transporte (headers, messageId do SQS, occurredAt
// do envelope, correlationId), para que a mesma operação recebida por HTTP e
// por SQS tenha exatamente o mesmo hash.
type BusinessPayload struct {
	ProviderID                     string
	ExternalTransactionID          string
	PlayerID                       uuid.UUID
	WalletID                       uuid.UUID
	RoundID                        string
	GameID                         string
	Kind                           Kind
	Money                          money.Money
	ReferenceExternalTransactionID string // vazio quando não há referência
}

// PayloadHashAlgorithm identifica o algoritmo, caso ele mude no futuro.
const PayloadHashAlgorithm = "sha256-canonical-json-v1"

// CanonicalJSON serializa os campos de negócio de forma determinística:
//
//   - objeto JSON com chaves em ordem alfabética (inclusive dentro de "money");
//   - sem espaços;
//   - UUIDs em minúsculas no formato 8-4-4-4-12;
//   - dinheiro na forma canônica já validada ("25.00", moeda ISO maiúscula);
//   - "referenceExternalTransactionId" omitido quando vazio.
//
// Não há normalização de texto além disso: só a forma canônica de valor é
// aceita na entrada, então "25.0" nunca chega aqui.
func (p BusinessPayload) CanonicalJSON() ([]byte, error) {
	// map[string]any: o encoding/json ordena as chaves de mapas
	obj := map[string]any{
		"providerId":            p.ProviderID,
		"externalTransactionId": p.ExternalTransactionID,
		"playerId":              p.PlayerID.String(),
		"walletId":              p.WalletID.String(),
		"roundId":               p.RoundID,
		"gameId":                p.GameID,
		"kind":                  string(p.Kind),
		"money": map[string]any{
			"amount":   p.Money.Amount(),
			"currency": p.Money.Currency().Code(),
		},
	}
	if p.ReferenceExternalTransactionID != "" {
		obj["referenceExternalTransactionId"] = p.ReferenceExternalTransactionID
	}
	return json.Marshal(obj)
}

// Hash devolve o SHA-256 (hex) do JSON canônico.
func (p BusinessPayload) Hash() (string, error) {
	if !p.Money.IsValid() {
		return "", money.ErrUninitialized
	}
	b, err := p.CanonicalJSON()
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}
