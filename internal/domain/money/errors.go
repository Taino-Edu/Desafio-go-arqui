package money

import "errors"

// Erros "guarda-chuva": use errors.Is para classificar.
var (
	// ErrInvalidAmount agrupa todo erro de valor de entrada inválido
	// (formato, escala, sinal ou tamanho). Os erros específicos abaixo
	// também respondem a errors.Is(err, ErrInvalidAmount).
	ErrInvalidAmount = errors.New("money: invalid amount")

	// ErrInvalidCurrency indica um código de moeda desconhecido ou fora do padrão ISO 4217.
	ErrInvalidCurrency = errors.New("money: invalid currency")

	// ErrCurrencyMismatch indica uma operação entre moedas diferentes.
	ErrCurrencyMismatch = errors.New("money: currency mismatch")

	// ErrOverflow indica que o resultado não cabe em int64 de unidades mínimas.
	ErrOverflow = errors.New("money: overflow")

	// ErrUninitialized indica uso de um Money ou Currency de valor zero (não construído).
	ErrUninitialized = errors.New("money: uninitialized value")
)

// Erros específicos de parsing. Todos também casam com ErrInvalidAmount.
var (
	ErrInvalidFormat  error = &amountError{"money: amount must be a plain decimal like \"25.00\""}
	ErrInvalidScale   error = &amountError{"money: amount must have exactly 2 decimal places"}
	ErrNegativeAmount error = &amountError{"money: amount must not be negative"}
	ErrAmountTooLarge error = &amountError{"money: amount is too large"}
)

type amountError struct{ msg string }

func (e *amountError) Error() string { return e.msg }

// Is faz errors.Is(ErrInvalidScale, ErrInvalidAmount) == true.
func (e *amountError) Is(target error) bool { return target == ErrInvalidAmount }
