package sqsconsumer

import (
	"errors"
	"strings"
	"testing"
)

const valid = `{"messageId":"msg-123","type":"WagerTransactionRequested","occurredAt":"2026-09-08T12:00:00.000Z",
"data":{"providerId":"provider-a","externalTransactionId":"transaction-123","idempotencyKey":"provider-a:transaction-123",
"playerId":"0192f28f-5dc0-7d58-bdb2-814ad6a0f4a1","walletId":"0192f291-27dd-7d3f-8071-5f8685deef37",
"roundId":"round-987","gameId":"fortune-chimp","kind":"BET","money":{"amount":"25.00","currency":"BRL"}}}`

func TestDecode_Valid(t *testing.T) {
	m, err := decode(valid, "wager-consumer")
	if err != nil {
		t.Fatal(err)
	}
	if m.MessageID != "msg-123" || m.Consumer != "wager-consumer" || m.Input.IdempotencyKey != "provider-a:transaction-123" ||
		m.Input.Money.Amount() != "25.00" || m.Input.Kind != "BET" {
		t.Errorf("decoded = %+v", m)
	}
}

func TestDecode_Invalid(t *testing.T) {
	cases := map[string]string{
		"JSON quebrado":       `{"messageId":`,
		"sem messageId":       strings.Replace(valid, `"messageId":"msg-123",`, ``, 1),
		"tipo desconhecido":   strings.Replace(valid, `WagerTransactionRequested`, `Other`, 1),
		"occurredAt inválido": strings.Replace(valid, `2026-09-08T12:00:00.000Z`, `ontem`, 1),
		"sem chave":           strings.Replace(valid, `"idempotencyKey":"provider-a:transaction-123",`, ``, 1),
		"valor como número":   strings.Replace(valid, `"amount":"25.00"`, `"amount":25.00`, 1),
		"escala errada":       strings.Replace(valid, `"25.00"`, `"25.0"`, 1),
		"negativo":            strings.Replace(valid, `"25.00"`, `"-25.00"`, 1),
		"moeda inválida":      strings.Replace(valid, `"BRL"`, `"brl"`, 1),
		"UUID inválido":       strings.Replace(valid, `0192f28f-5dc0-7d58-bdb2-814ad6a0f4a1`, `jogador-1`, 1),
		"campo desconhecido":  strings.Replace(valid, `"kind":"BET"`, `"kind":"BET","bonus":true`, 1),
		"lixo depois":         valid + `{}`,
		"sem data":            `{"messageId":"m","type":"WagerTransactionRequested","occurredAt":"2026-09-08T12:00:00Z"}`,
	}
	for name, body := range cases {
		if _, err := decode(body, "c"); !errors.Is(err, ErrInvalidMessage) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
}

func TestBackoff(t *testing.T) {
	for n, want := range map[int]int{1: 1, 2: 2, 3: 4, 4: 8, 10: 30} {
		if got := backoff(1e9, n, 30e9); int(got.Seconds()) != want {
			t.Errorf("backoff(%d) = %v, want %ds", n, got, want)
		}
	}
}
