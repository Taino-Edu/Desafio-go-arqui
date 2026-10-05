package app

import (
	"context"

	"github.com/Taino-Edu/Desafio-go-arqui/internal/domain/accounting"
	"github.com/Taino-Edu/Desafio-go-arqui/internal/domain/money"
	"github.com/Taino-Edu/Desafio-go-arqui/internal/domain/wallet"
)

// TrialBalance é o balancete de verificação de uma moeda: as somas de cada
// conta do razão. Em partidas dobradas, Σ débitos = Σ créditos sempre; e o
// passivo com os jogadores no razão tem de ser igual à soma dos saldos
// gravados nas carteiras.
type TrialBalance struct {
	Currency     money.Currency
	Accounts     []TrialBalanceAccount
	TotalDebits  money.Money
	TotalCredits money.Money
	Balanced     bool // Σ débitos = Σ créditos

	WalletLiabilities    money.Money // saldo das contas de carteira no razão
	StoredWalletBalances money.Money // Σ saldo gravado nas carteiras
	WalletsMatch         bool
	Wallets              int64
}

// TrialBalanceAccount é uma linha do balancete.
type TrialBalanceAccount struct {
	Account  string // código da conta, ou WalletAccountsRow
	Type     accounting.AccountType
	Accounts int64
	Debits   money.Money
	Credits  money.Money
	// Balance fica no lado natural da conta: ativo = débitos − créditos;
	// passivo e receita = créditos − débitos. Receita negativa = prejuízo
	// com o provedor (pagou mais prêmios do que recebeu de apostas).
	Balance money.Money
}

// TrialBalance soma o razão de uma moeda. As leituras acontecem na mesma
// foto do banco (ReadSnapshot): com tráfego no meio, os números continuam
// fechando. Custo: varre as partidas da moeda; é uma consulta de auditoria,
// não de caminho quente.
func (s *WalletService) TrialBalance(ctx context.Context, currency money.Currency) (TrialBalance, error) {
	var d TrialBalanceData
	err := s.store.ReadSnapshot(ctx, func(ctx context.Context, r Repositories) error {
		var err error
		d, err = r.Journal().TrialBalance(ctx, currency)
		return err
	})
	if err != nil {
		return TrialBalance{}, err
	}

	zero, err := money.Zero(currency)
	if err != nil {
		return TrialBalance{}, err
	}
	tb := TrialBalance{Currency: currency, TotalDebits: zero, TotalCredits: zero,
		WalletLiabilities: zero, Wallets: d.Wallets}
	for _, row := range d.Rows {
		line := TrialBalanceAccount{Account: row.Account, Type: row.Type, Accounts: row.Accounts}
		if line.Debits, err = money.FromMinorUnits(row.Debits, currency); err != nil {
			return TrialBalance{}, err
		}
		if line.Credits, err = money.FromMinorUnits(row.Credits, currency); err != nil {
			return TrialBalance{}, err
		}
		if row.Type.NormalSide() == wallet.Debit {
			line.Balance, err = line.Debits.Sub(line.Credits)
		} else {
			line.Balance, err = line.Credits.Sub(line.Debits)
		}
		if err != nil {
			return TrialBalance{}, err
		}
		if tb.TotalDebits, err = tb.TotalDebits.Add(line.Debits); err != nil {
			return TrialBalance{}, err
		}
		if tb.TotalCredits, err = tb.TotalCredits.Add(line.Credits); err != nil {
			return TrialBalance{}, err
		}
		if row.Account == WalletAccountsRow {
			tb.WalletLiabilities = line.Balance
		}
		tb.Accounts = append(tb.Accounts, line)
	}
	if tb.StoredWalletBalances, err = money.FromMinorUnits(d.StoredWalletBalances, currency); err != nil {
		return TrialBalance{}, err
	}
	tb.Balanced = tb.TotalDebits.Equal(tb.TotalCredits)
	tb.WalletsMatch = tb.WalletLiabilities.Equal(tb.StoredWalletBalances)
	return tb, nil
}
