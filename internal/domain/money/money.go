// Package money implementa o value object Money.
//
// Representação: int64 em unidades mínimas (centavos), com escala fixa de 2
// casas. "25.00" BRL é guardado como 2500. Nenhuma operação usa float.
//
// Limites: de -92.233.720.368.547.758,08 a 92.233.720.368.547.758,07.
// Qualquer parsing, soma, subtração ou negação que ultrapasse esse intervalo
// devolve ErrOverflow ou ErrAmountTooLarge em vez de estourar silenciosamente.
package money

import (
	"fmt"
	"math"
)

// Scale é o número fixo de casas decimais.
const Scale = 2

const minorPerUnit = 100 // 10^Scale

// Money é imutável: todos os métodos devolvem um novo valor.
// O valor zero (Money{}) é inválido e rejeitado pelas operações.
type Money struct {
	minor    int64
	currency Currency
}

// Parse lê um valor de ENTRADA EXTERNA no formato canônico "25.00".
//
// Rejeita: vazio, sinal (+/-), espaços, notação científica, NaN, Infinity,
// separador de milhar, vírgula, zeros à esquerda ("025.00"), escala diferente
// de 2 ("25", "25.0", "25.001") e valores que não cabem em int64.
// Nada é arredondado nem normalizado: o texto aceito já é a forma canônica,
// então o hash de idempotência pode usá-lo diretamente.
func Parse(amount string, currency Currency) (Money, error) {
	if currency.IsZero() {
		return Money{}, ErrUninitialized
	}
	minor, err := parseMinor(amount, false)
	if err != nil {
		return Money{}, err
	}
	return Money{minor: minor, currency: currency}, nil
}

// FromMinorUnits cria um Money a partir de unidades mínimas já confiáveis,
// por exemplo uma coluna BIGINT do banco. Aceita negativos (diferenças internas).
func FromMinorUnits(minor int64, currency Currency) (Money, error) {
	if currency.IsZero() {
		return Money{}, ErrUninitialized
	}
	return Money{minor: minor, currency: currency}, nil
}

// Zero devolve 0.00 na moeda informada.
func Zero(currency Currency) (Money, error) {
	return FromMinorUnits(0, currency)
}

// MinorUnits devolve o valor em centavos (para persistência em BIGINT).
func (m Money) MinorUnits() int64 { return m.minor }

// Currency devolve a moeda.
func (m Money) Currency() Currency { return m.currency }

// IsValid informa se o Money foi construído pelas funções do pacote.
func (m Money) IsValid() bool { return !m.currency.IsZero() }

func (m Money) IsZero() bool     { return m.IsValid() && m.minor == 0 }
func (m Money) IsPositive() bool { return m.IsValid() && m.minor > 0 }
func (m Money) IsNegative() bool { return m.IsValid() && m.minor < 0 }

// Add soma dois valores da mesma moeda.
func (m Money) Add(other Money) (Money, error) {
	if err := m.compatible(other); err != nil {
		return Money{}, err
	}
	a, b := m.minor, other.minor
	if (b > 0 && a > math.MaxInt64-b) || (b < 0 && a < math.MinInt64-b) {
		return Money{}, ErrOverflow
	}
	return Money{minor: a + b, currency: m.currency}, nil
}

// Sub subtrai other de m. O resultado pode ser negativo; quem proíbe saldo
// negativo é a Wallet, não o Money.
func (m Money) Sub(other Money) (Money, error) {
	if err := m.compatible(other); err != nil {
		return Money{}, err
	}
	a, b := m.minor, other.minor
	if (b < 0 && a > math.MaxInt64+b) || (b > 0 && a < math.MinInt64+b) {
		return Money{}, ErrOverflow
	}
	return Money{minor: a - b, currency: m.currency}, nil
}

// Neg devolve o valor com o sinal invertido.
func (m Money) Neg() (Money, error) {
	if !m.IsValid() {
		return Money{}, ErrUninitialized
	}
	if m.minor == math.MinInt64 { // -MinInt64 não cabe em int64
		return Money{}, ErrOverflow
	}
	return Money{minor: -m.minor, currency: m.currency}, nil
}

// Cmp compara: -1 se m < other, 0 se iguais, +1 se m > other.
func (m Money) Cmp(other Money) (int, error) {
	if err := m.compatible(other); err != nil {
		return 0, err
	}
	switch {
	case m.minor < other.minor:
		return -1, nil
	case m.minor > other.minor:
		return 1, nil
	default:
		return 0, nil
	}
}

// Equal informa se valor e moeda são iguais. Moedas diferentes não são iguais.
func (m Money) Equal(other Money) bool {
	return m.IsValid() && m.minor == other.minor && m.currency == other.currency
}

// Amount devolve o valor decimal canônico, sem a moeda: "25.00", "-0.05".
func (m Money) Amount() string {
	var abs uint64
	sign := ""
	if m.minor < 0 {
		sign = "-"
		abs = uint64(-(m.minor + 1)) + 1 // evita estouro em MinInt64
	} else {
		abs = uint64(m.minor)
	}
	return fmt.Sprintf("%s%d.%02d", sign, abs/minorPerUnit, abs%minorPerUnit)
}

// String devolve "25.00 BRL".
func (m Money) String() string {
	if !m.IsValid() {
		return "<invalid money>"
	}
	return m.Amount() + " " + m.currency.code
}

func (m Money) compatible(other Money) error {
	if !m.IsValid() || !other.IsValid() {
		return ErrUninitialized
	}
	if m.currency != other.currency {
		return fmt.Errorf("%w: %s vs %s", ErrCurrencyMismatch, m.currency, other.currency)
	}
	return nil
}

// maxInputLen limita o tamanho da string antes de qualquer trabalho:
// 19 dígitos inteiros + "." + 2 decimais + sinal.
const maxInputLen = 23

// parseMinor converte "123.45" em 12345 sem usar float.
// allowNegative só é true na desserialização interna (round-trip de Amount).
func parseMinor(s string, allowNegative bool) (int64, error) {
	if s == "" {
		return 0, ErrInvalidFormat
	}
	if len(s) > maxInputLen {
		return 0, ErrAmountTooLarge
	}

	negative := false
	if s[0] == '-' {
		negative = true
		s = s[1:]
	}

	dot := -1
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= '0' && c <= '9':
		case c == '.' && dot == -1:
			dot = i
		default:
			return 0, ErrInvalidFormat // letras, 'e', '+', ',', espaço, segundo '.'
		}
	}

	intPart, fracPart := s, ""
	if dot >= 0 {
		intPart, fracPart = s[:dot], s[dot+1:]
	}
	if intPart == "" {
		return 0, ErrInvalidFormat // ".50" ou "-"
	}
	if len(intPart) > 1 && intPart[0] == '0' {
		return 0, ErrInvalidFormat // "025.00" não é canônico
	}
	if len(fracPart) != Scale {
		return 0, ErrInvalidScale // "25", "25.", "25.0", "25.001"
	}
	if negative {
		if !allowNegative {
			return 0, ErrNegativeAmount
		}
		if intPart == "0" && fracPart == "00" {
			return 0, ErrInvalidFormat // "-0.00" não é canônico
		}
	}

	// Acumula em negativo: o intervalo negativo do int64 é um a mais que o
	// positivo, então MinInt64 também pode ser lido sem estourar.
	var acc int64
	for _, c := range intPart + fracPart {
		d := int64(c - '0')
		if acc < (math.MinInt64+d)/10 {
			return 0, ErrAmountTooLarge
		}
		acc = acc*10 - d
	}
	if negative {
		return acc, nil
	}
	if acc == math.MinInt64 {
		return 0, ErrAmountTooLarge
	}
	return -acc, nil
}
