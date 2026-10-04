package money

import "fmt"

// Currency é um código ISO 4217 validado. O campo é privado: a única forma de
// obter uma Currency válida é por NewCurrency ou pelas variáveis abaixo, então o
// valor zero (Currency{}) é sempre reconhecido como não inicializado.
type Currency struct {
	code string
}

// Moedas suportadas. Todas usam 2 casas decimais (centavos).
var (
	BRL = Currency{"BRL"}
	USD = Currency{"USD"}
	EUR = Currency{"EUR"}
)

var supported = map[string]Currency{
	BRL.code: BRL,
	USD.code: USD,
	EUR.code: EUR,
}

// NewCurrency valida um código ISO 4217. Não normaliza: "brl" é rejeitado.
func NewCurrency(code string) (Currency, error) {
	c, ok := supported[code]
	if !ok {
		return Currency{}, fmt.Errorf("%w: %q", ErrInvalidCurrency, code)
	}
	return c, nil
}

// Code devolve o código ISO 4217, por exemplo "BRL".
func (c Currency) Code() string { return c.code }

// IsZero informa se a moeda não foi inicializada.
func (c Currency) IsZero() bool { return c.code == "" }

func (c Currency) String() string { return c.code }
