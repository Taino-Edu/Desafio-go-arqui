package money_test

import (
	"encoding/json"
	"errors"
	"math"
	"strings"
	"testing"

	"github.com/Taino-Edu/Desafio-go-arqui/internal/domain/money"
)

func mustParse(t *testing.T, amount string, cur money.Currency) money.Money {
	t.Helper()
	m, err := money.Parse(amount, cur)
	if err != nil {
		t.Fatalf("Parse(%q): %v", amount, err)
	}
	return m
}

func mustMinor(t *testing.T, minor int64, cur money.Currency) money.Money {
	t.Helper()
	m, err := money.FromMinorUnits(minor, cur)
	if err != nil {
		t.Fatalf("FromMinorUnits(%d): %v", minor, err)
	}
	return m
}

func TestParse_Valid(t *testing.T) {
	tests := []struct {
		in    string
		minor int64
	}{
		{"0.00", 0},
		{"0.01", 1},
		{"0.10", 10},
		{"1.00", 100},
		{"25.00", 2500},
		{"1000.00", 100000},
		{"123456.78", 12345678},
		{"92233720368547758.07", math.MaxInt64},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			m := mustParse(t, tt.in, money.BRL)
			if m.MinorUnits() != tt.minor {
				t.Errorf("MinorUnits = %d, want %d", m.MinorUnits(), tt.minor)
			}
			if m.Currency() != money.BRL {
				t.Errorf("Currency = %v, want BRL", m.Currency())
			}
			if got := m.Amount(); got != tt.in {
				t.Errorf("round-trip Amount = %q, want %q", got, tt.in)
			}
		})
	}
}

func TestParse_Invalid(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want error
	}{
		{"vazio", "", money.ErrInvalidFormat},
		{"sem casas", "25", money.ErrInvalidScale},
		{"ponto sem casas", "25.", money.ErrInvalidScale},
		{"uma casa", "25.0", money.ErrInvalidScale},
		{"três casas", "25.001", money.ErrInvalidScale},
		{"escala excedente com zeros", "25.000", money.ErrInvalidScale},
		{"negativo", "-1.00", money.ErrNegativeAmount},
		{"menos zero", "-0.00", money.ErrNegativeAmount},
		{"sinal positivo", "+1.00", money.ErrInvalidFormat},
		{"notação científica", "1e3", money.ErrInvalidFormat},
		{"notação científica com casas", "1.00e2", money.ErrInvalidFormat},
		{"NaN", "NaN", money.ErrInvalidFormat},
		{"Infinity", "Infinity", money.ErrInvalidFormat},
		{"inf", "inf", money.ErrInvalidFormat},
		{"vírgula", "25,00", money.ErrInvalidFormat},
		{"separador de milhar", "1,000.00", money.ErrInvalidFormat},
		{"espaço à esquerda", " 25.00", money.ErrInvalidFormat},
		{"espaço à direita", "25.00 ", money.ErrInvalidFormat},
		{"zero à esquerda", "025.00", money.ErrInvalidFormat},
		{"sem parte inteira", ".50", money.ErrInvalidFormat},
		{"dois pontos", "1.2.3", money.ErrInvalidFormat},
		{"hexadecimal", "0x10.00", money.ErrInvalidFormat},
		{"underscore", "1_000.00", money.ErrInvalidFormat},
		{"dígito unicode", "٢.00", money.ErrInvalidFormat},
		{"MaxInt64 + 1 centavo", "92233720368547758.08", money.ErrAmountTooLarge},
		{"muito grande", "99999999999999999999.00", money.ErrAmountTooLarge},
		{"string enorme", strings.Repeat("9", 1000) + ".00", money.ErrAmountTooLarge},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := money.Parse(tt.in, money.BRL)
			if !errors.Is(err, tt.want) {
				t.Fatalf("Parse(%q) err = %v, want %v", tt.in, err, tt.want)
			}
			if !errors.Is(err, money.ErrInvalidAmount) {
				t.Errorf("Parse(%q) err = %v, deveria casar com ErrInvalidAmount", tt.in, err)
			}
		})
	}
}

func TestParse_UninitializedCurrency(t *testing.T) {
	_, err := money.Parse("1.00", money.Currency{})
	if !errors.Is(err, money.ErrUninitialized) {
		t.Fatalf("err = %v, want ErrUninitialized", err)
	}
}

func TestNewCurrency(t *testing.T) {
	for _, code := range []string{"BRL", "USD", "EUR"} {
		c, err := money.NewCurrency(code)
		if err != nil || c.Code() != code {
			t.Errorf("NewCurrency(%q) = %v, %v", code, c, err)
		}
	}
	for _, code := range []string{"", "brl", "Brl", "XXX", "BR", "BRLL", " BRL"} {
		if _, err := money.NewCurrency(code); !errors.Is(err, money.ErrInvalidCurrency) {
			t.Errorf("NewCurrency(%q) err = %v, want ErrInvalidCurrency", code, err)
		}
	}
	if !(money.Currency{}).IsZero() {
		t.Error("Currency{} deveria ser IsZero")
	}
}

func TestZero(t *testing.T) {
	z, err := money.Zero(money.BRL)
	if err != nil {
		t.Fatal(err)
	}
	if !z.IsZero() || z.IsPositive() || z.IsNegative() || z.Amount() != "0.00" {
		t.Errorf("Zero(BRL) = %v", z)
	}
	if _, err := money.Zero(money.Currency{}); !errors.Is(err, money.ErrUninitialized) {
		t.Errorf("Zero(Currency{}) err = %v, want ErrUninitialized", err)
	}
}

func TestAddSub(t *testing.T) {
	a := mustParse(t, "100.00", money.BRL)
	b := mustParse(t, "80.00", money.BRL)

	sum, err := a.Add(b)
	if err != nil || sum.Amount() != "180.00" {
		t.Errorf("100 + 80 = %v, %v", sum, err)
	}

	diff, err := a.Sub(b)
	if err != nil || diff.Amount() != "20.00" {
		t.Errorf("100 - 80 = %v, %v", diff, err)
	}

	// diferenças internas podem ser negativas
	neg, err := b.Sub(a)
	if err != nil || neg.Amount() != "-20.00" || !neg.IsNegative() {
		t.Errorf("80 - 100 = %v, %v", neg, err)
	}

	// imutabilidade: os operandos não mudam
	if a.Amount() != "100.00" || b.Amount() != "80.00" {
		t.Errorf("operandos foram alterados: a=%v b=%v", a, b)
	}
}

func TestOverflow(t *testing.T) {
	maxM := mustMinor(t, math.MaxInt64, money.BRL)
	minM := mustMinor(t, math.MinInt64, money.BRL)
	one := mustMinor(t, 1, money.BRL)
	minusOne := mustMinor(t, -1, money.BRL)

	tests := []struct {
		name string
		op   func() (money.Money, error)
	}{
		{"max + 1", func() (money.Money, error) { return maxM.Add(one) }},
		{"min + (-1)", func() (money.Money, error) { return minM.Add(minusOne) }},
		{"min - 1", func() (money.Money, error) { return minM.Sub(one) }},
		{"max - (-1)", func() (money.Money, error) { return maxM.Sub(minusOne) }},
		{"0 - min", func() (money.Money, error) { return mustMinor(t, 0, money.BRL).Sub(minM) }},
		{"-min", func() (money.Money, error) { return minM.Neg() }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := tt.op(); !errors.Is(err, money.ErrOverflow) {
				t.Errorf("err = %v, want ErrOverflow", err)
			}
		})
	}

	// nos limites exatos não pode dar erro
	if got, err := maxM.Sub(one); err != nil || got.MinorUnits() != math.MaxInt64-1 {
		t.Errorf("max - 1 = %v, %v", got, err)
	}
	if got, err := minM.Add(one); err != nil || got.MinorUnits() != math.MinInt64+1 {
		t.Errorf("min + 1 = %v, %v", got, err)
	}
	if got, err := maxM.Neg(); err != nil || got.MinorUnits() != -math.MaxInt64 {
		t.Errorf("-max = %v, %v", got, err)
	}
	if got := minM.Amount(); got != "-92233720368547758.08" {
		t.Errorf("min.Amount() = %q", got)
	}
}

func TestNeg(t *testing.T) {
	m := mustParse(t, "25.00", money.BRL)
	n, err := m.Neg()
	if err != nil || n.Amount() != "-25.00" {
		t.Fatalf("Neg = %v, %v", n, err)
	}
	back, err := n.Neg()
	if err != nil || !back.Equal(m) {
		t.Errorf("Neg(Neg(x)) = %v, want %v", back, m)
	}
}

func TestCmpAndEqual(t *testing.T) {
	a := mustParse(t, "10.00", money.BRL)
	b := mustParse(t, "20.00", money.BRL)
	a2 := mustParse(t, "10.00", money.BRL)

	for _, tt := range []struct {
		x, y money.Money
		want int
	}{{a, b, -1}, {b, a, 1}, {a, a2, 0}} {
		got, err := tt.x.Cmp(tt.y)
		if err != nil || got != tt.want {
			t.Errorf("Cmp(%v, %v) = %d, %v; want %d", tt.x, tt.y, got, err, tt.want)
		}
	}
	if !a.Equal(a2) || a.Equal(b) {
		t.Error("Equal com valores iguais/diferentes falhou")
	}
	if a.Equal(mustParse(t, "10.00", money.USD)) {
		t.Error("10.00 BRL não pode ser igual a 10.00 USD")
	}
}

func TestCurrencyMismatch(t *testing.T) {
	brl := mustParse(t, "10.00", money.BRL)
	usd := mustParse(t, "10.00", money.USD)

	if _, err := brl.Add(usd); !errors.Is(err, money.ErrCurrencyMismatch) {
		t.Errorf("Add err = %v", err)
	}
	if _, err := brl.Sub(usd); !errors.Is(err, money.ErrCurrencyMismatch) {
		t.Errorf("Sub err = %v", err)
	}
	if _, err := brl.Cmp(usd); !errors.Is(err, money.ErrCurrencyMismatch) {
		t.Errorf("Cmp err = %v", err)
	}
}

func TestUninitializedMoney(t *testing.T) {
	var zero money.Money
	valid := mustParse(t, "1.00", money.BRL)

	if zero.IsValid() || zero.IsZero() || zero.IsPositive() || zero.IsNegative() {
		t.Error("Money{} não deveria ser válido nem zero/positivo/negativo")
	}
	checks := map[string]error{}
	_, checks["Add"] = valid.Add(zero)
	_, checks["Add reverso"] = zero.Add(valid)
	_, checks["Sub"] = valid.Sub(zero)
	_, checks["Neg"] = zero.Neg()
	_, checks["Cmp"] = zero.Cmp(valid)
	_, checks["Marshal"] = json.Marshal(zero)
	for name, err := range checks {
		if !errors.Is(err, money.ErrUninitialized) {
			t.Errorf("%s err = %v, want ErrUninitialized", name, err)
		}
	}
	if zero.Equal(money.Money{}) {
		t.Error("Money{} não deveria ser Equal a nada")
	}
}

func TestString(t *testing.T) {
	if got := mustParse(t, "25.00", money.BRL).String(); got != "25.00 BRL" {
		t.Errorf("String = %q", got)
	}
	if got := mustMinor(t, -5, money.BRL).String(); got != "-0.05 BRL" {
		t.Errorf("String = %q", got)
	}
}

func TestJSON_Marshal(t *testing.T) {
	b, err := json.Marshal(mustParse(t, "25.00", money.BRL))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(b), `{"amount":"25.00","currency":"BRL"}`; got != want {
		t.Errorf("Marshal = %s, want %s", got, want)
	}
}

func TestJSON_RoundTrip(t *testing.T) {
	for _, minor := range []int64{0, 1, -1, 2500, -2000, math.MaxInt64, math.MinInt64} {
		orig := mustMinor(t, minor, money.BRL)
		b, err := json.Marshal(orig)
		if err != nil {
			t.Fatal(err)
		}
		var got money.Money
		if err := json.Unmarshal(b, &got); err != nil {
			t.Fatalf("Unmarshal(%s): %v", b, err)
		}
		if !got.Equal(orig) {
			t.Errorf("round-trip %d: got %v", minor, got)
		}
	}
}

func TestJSON_UnmarshalInvalid(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want error
	}{
		{"amount como número", `{"amount":25.00,"currency":"BRL"}`, money.ErrInvalidAmount},
		{"amount ausente", `{"currency":"BRL"}`, money.ErrInvalidAmount},
		{"currency ausente", `{"amount":"1.00"}`, money.ErrInvalidAmount},
		{"campo extra", `{"amount":"1.00","currency":"BRL","x":1}`, money.ErrInvalidAmount},
		{"null", `null`, money.ErrInvalidAmount},
		{"escala errada", `{"amount":"1.0","currency":"BRL"}`, money.ErrInvalidScale},
		{"notação científica", `{"amount":"1e2","currency":"BRL"}`, money.ErrInvalidFormat},
		{"moeda inválida", `{"amount":"1.00","currency":"XYZ"}`, money.ErrInvalidCurrency},
		{"moeda minúscula", `{"amount":"1.00","currency":"brl"}`, money.ErrInvalidCurrency},
		{"menos zero", `{"amount":"-0.00","currency":"BRL"}`, money.ErrInvalidFormat},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var m money.Money
			err := json.Unmarshal([]byte(tt.in), &m)
			if !errors.Is(err, tt.want) {
				t.Errorf("err = %v, want %v", err, tt.want)
			}
		})
	}
}

func TestJSON_NestedInStruct(t *testing.T) {
	var req struct {
		InitialBalance money.Money `json:"initialBalance"`
	}
	body := `{"initialBalance":{"amount":"1000.00","currency":"BRL"}}`
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatal(err)
	}
	if req.InitialBalance.MinorUnits() != 100000 || req.InitialBalance.Currency() != money.BRL {
		t.Errorf("got %v", req.InitialBalance)
	}
}

// FuzzParse garante que nenhuma string faz o parser entrar em pânico e que
// todo valor aceito volta idêntico pelo Amount (forma canônica).
// Rodar: go test -fuzz=FuzzParse ./internal/domain/money
func FuzzParse(f *testing.F) {
	for _, s := range []string{"0.00", "25.00", "1e3", "-1.00", "92233720368547758.07", "92233720368547758.08", "NaN", ""} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		m, err := money.Parse(s, money.BRL)
		if err != nil {
			if !errors.Is(err, money.ErrInvalidAmount) {
				t.Fatalf("Parse(%q) erro fora da família ErrInvalidAmount: %v", s, err)
			}
			return
		}
		if m.IsNegative() {
			t.Fatalf("Parse(%q) aceitou negativo", s)
		}
		if m.Amount() != s {
			t.Fatalf("Parse(%q) não é canônico: Amount() = %q", s, m.Amount())
		}
	})
}
