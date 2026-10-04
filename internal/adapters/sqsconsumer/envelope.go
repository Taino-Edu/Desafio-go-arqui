package sqsconsumer

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/Taino-Edu/Desafio-go-arqui/internal/app"
	"github.com/Taino-Edu/Desafio-go-arqui/internal/domain/money"
)

// MessageType é o único tipo aceito na fila de entrada.
const MessageType = "WagerTransactionRequested"

// ErrInvalidMessage: a mensagem não pode ser interpretada. Permanente: vai
// para a DLQ sem tocar no domínio.
var ErrInvalidMessage = errors.New("invalid message")

// Envelope é o formato da mensagem de entrada.
type Envelope struct {
	MessageID  string       `json:"messageId"`
	Type       string       `json:"type"`
	OccurredAt string       `json:"occurredAt"`
	Data       *MessageData `json:"data"`
}

// MessageData são os campos da operação. Mesmos do HTTP, mais a chave de
// idempotência (no HTTP ela vem no header).
type MessageData struct {
	ProviderID                     string     `json:"providerId"`
	ExternalTransactionID          string     `json:"externalTransactionId"`
	IdempotencyKey                 string     `json:"idempotencyKey"`
	PlayerID                       string     `json:"playerId"`
	WalletID                       string     `json:"walletId"`
	RoundID                        string     `json:"roundId"`
	GameID                         string     `json:"gameId"`
	Kind                           string     `json:"kind"`
	Money                          *moneyJSON `json:"money"`
	ReferenceExternalTransactionID string     `json:"referenceExternalTransactionId,omitempty"`
}

type moneyJSON struct {
	Amount   string `json:"amount"`
	Currency string `json:"currency"`
}

func invalid(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalidMessage, fmt.Sprintf(format, args...))
}

// decode interpreta o corpo de forma estrita (campos desconhecidos, valor
// monetário como número, formatos errados: tudo é recusado) e devolve a
// entrada do caso de uso. As regras de negócio (tipo, valor zero, referência)
// continuam sendo do domínio.
func decode(body, consumer string) (app.QueueMessage, error) {
	dec := json.NewDecoder(bytes.NewReader([]byte(body)))
	dec.DisallowUnknownFields()
	var env Envelope
	if err := dec.Decode(&env); err != nil {
		return app.QueueMessage{}, invalid("malformed JSON envelope")
	}
	if dec.More() {
		return app.QueueMessage{}, invalid("unexpected data after JSON envelope")
	}
	switch {
	case env.MessageID == "":
		return app.QueueMessage{}, invalid("messageId is required")
	case len(env.MessageID) > 128:
		return app.QueueMessage{}, invalid("messageId is too long")
	case env.Type != MessageType:
		return app.QueueMessage{}, invalid("unsupported type %q", env.Type)
	case env.Data == nil:
		return app.QueueMessage{}, invalid("data is required")
	}
	if _, err := time.Parse(time.RFC3339Nano, env.OccurredAt); err != nil {
		return app.QueueMessage{}, invalid("occurredAt must be RFC 3339")
	}
	d := env.Data
	for name, v := range map[string]string{
		"providerId": d.ProviderID, "externalTransactionId": d.ExternalTransactionID,
		"idempotencyKey": d.IdempotencyKey, "roundId": d.RoundID, "gameId": d.GameID, "kind": d.Kind,
	} {
		if v == "" {
			return app.QueueMessage{}, invalid("data.%s is required", name)
		}
	}
	playerID, err := uuid.Parse(d.PlayerID)
	if err != nil || playerID == uuid.Nil {
		return app.QueueMessage{}, invalid("data.playerId must be a UUID")
	}
	walletID, err := uuid.Parse(d.WalletID)
	if err != nil || walletID == uuid.Nil {
		return app.QueueMessage{}, invalid("data.walletId must be a UUID")
	}
	if d.Money == nil {
		return app.QueueMessage{}, invalid("data.money is required")
	}
	cur, err := money.NewCurrency(d.Money.Currency)
	if err != nil {
		return app.QueueMessage{}, invalid("data.money.currency: %v", err)
	}
	amount, err := money.Parse(d.Money.Amount, cur)
	if err != nil {
		return app.QueueMessage{}, invalid("data.money.amount: %v", err)
	}
	return app.QueueMessage{
		Consumer:  consumer,
		MessageID: env.MessageID,
		Input: app.SubmitInput{
			ProviderID: d.ProviderID, ExternalTransactionID: d.ExternalTransactionID,
			IdempotencyKey: d.IdempotencyKey, PlayerID: playerID, WalletID: walletID,
			RoundID: d.RoundID, GameID: d.GameID, Kind: d.Kind, Money: amount,
			ReferenceExternalTransactionID: d.ReferenceExternalTransactionID,
		},
	}, nil
}
