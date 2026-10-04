package wagering

import (
	"time"

	"github.com/Taino-Edu/Desafio-go-arqui/internal/domain/domainerr"
)

// ReferenceRetryPolicy define o backoff exponencial da espera por referência.
//
// Atraso da tentativa n (começando em 0): min(BaseDelay * 2^n, MaxDelay).
// Depois de MaxAttempts tentativas sem sucesso, a operação é rejeitada com
// REFERENCE_NOT_FOUND. O jitter (aleatoriedade) é somado pelo worker, fora do
// domínio, para manter esta política determinística e testável.
type ReferenceRetryPolicy struct {
	BaseDelay   time.Duration
	MaxDelay    time.Duration
	MaxAttempts int
}

// DefaultReferenceRetryPolicy: 1s, 2s, 4s ... até 5min, 12 tentativas
// (cerca de 24 minutos no total).
var DefaultReferenceRetryPolicy = ReferenceRetryPolicy{
	BaseDelay:   time.Second,
	MaxDelay:    5 * time.Minute,
	MaxAttempts: 12,
}

// Validate confere se a política é coerente.
func (p ReferenceRetryPolicy) Validate() error {
	switch {
	case p.BaseDelay <= 0:
		return domainerr.Field("baseDelay", "must be positive")
	case p.MaxDelay < p.BaseDelay:
		return domainerr.Field("maxDelay", "must be >= baseDelay")
	case p.MaxAttempts < 1:
		return domainerr.Field("maxAttempts", "must be >= 1")
	}
	return nil
}

// Delay devolve o atraso antes da tentativa de número attempt (0, 1, 2...).
func (p ReferenceRetryPolicy) Delay(attempt int) time.Duration {
	if attempt < 0 {
		attempt = 0
	}
	d := p.BaseDelay
	for i := 0; i < attempt; i++ {
		if d >= p.MaxDelay/2 { // evita overflow da duração
			return p.MaxDelay
		}
		d *= 2
	}
	if d > p.MaxDelay {
		return p.MaxDelay
	}
	return d
}

// Exhausted informa se já não há mais tentativas.
func (p ReferenceRetryPolicy) Exhausted(attempts int) bool {
	return attempts >= p.MaxAttempts
}
