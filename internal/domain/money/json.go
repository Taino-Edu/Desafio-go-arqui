package money

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// jsonMoney é o contrato externo: {"amount":"25.00","currency":"BRL"}.
// Ponteiros permitem distinguir campo ausente de campo vazio.
type jsonMoney struct {
	Amount   *string `json:"amount"`
	Currency *string `json:"currency"`
}

// MarshalJSON serializa como {"amount":"25.00","currency":"BRL"}.
// O valor sai como string, nunca como número JSON.
func (m Money) MarshalJSON() ([]byte, error) {
	if !m.IsValid() {
		return nil, ErrUninitialized
	}
	amount, code := m.Amount(), m.currency.code
	return json.Marshal(jsonMoney{Amount: &amount, Currency: &code})
}

// UnmarshalJSON lê o formato de MarshalJSON. Exige amount como string JSON
// (um número como 25.00 é rejeitado sem nunca passar por float) e recusa
// campos desconhecidos.
//
// Aceita negativos para permitir o round-trip de valores internos (diferenças
// de reconciliação, por exemplo). Entradas financeiras externas devem usar
// Parse, que rejeita negativos.
func (m *Money) UnmarshalJSON(data []byte) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var raw jsonMoney
	if err := dec.Decode(&raw); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidFormat, err)
	}
	if raw.Amount == nil || raw.Currency == nil {
		return fmt.Errorf("%w: amount and currency are required", ErrInvalidFormat)
	}
	cur, err := NewCurrency(*raw.Currency)
	if err != nil {
		return err
	}
	minor, err := parseMinor(*raw.Amount, true)
	if err != nil {
		return err
	}
	*m = Money{minor: minor, currency: cur}
	return nil
}
