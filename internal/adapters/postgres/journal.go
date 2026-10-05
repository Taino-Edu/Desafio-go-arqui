package postgres

import (
	"context"

	"github.com/Taino-Edu/Desafio-go-arqui/internal/app"
	"github.com/Taino-Edu/Desafio-go-arqui/internal/domain/accounting"
	"github.com/Taino-Edu/Desafio-go-arqui/internal/domain/money"
)

// journalRepo grava o razão em partidas dobradas (ledger_accounts e
// journal_postings). O equilíbrio do lançamento, a correspondência com o
// ledger da carteira e a contrapartida certa são conferidos pelo banco no
// COMMIT (migration 000005).
type journalRepo struct{ q querier }

// Post grava contas e partidas num comando só. As contas da casa (caixa,
// provedores) só são criadas na primeira vez: ON CONFLICT DO NOTHING não
// trava a linha existente, então apostas concorrentes não disputam a conta
// do provedor (nenhuma conta guarda saldo; o saldo é a soma das partidas).
func (r journalRepo) Post(ctx context.Context, j accounting.JournalEntry) error {
	ps := j.Postings()
	n := len(ps)
	codes, types, walletIDs, providerIDs := make([]string, n), make([]string, n), make([]string, n), make([]string, n)
	directions, amounts := make([]string, n), make([]int64, n)
	for i, p := range ps {
		codes[i], types[i], providerIDs[i] = p.Account.Code(), string(p.Account.Type()), p.Account.ProviderID()
		if p.Account.Type() == accounting.Liability {
			walletIDs[i] = p.Account.WalletID().String()
		}
		directions[i], amounts[i] = string(p.Direction), p.Amount.MinorUnits()
	}
	_, err := r.q.Exec(ctx, `
		WITH accounts AS (
			INSERT INTO ledger_accounts (code, type, currency, wallet_id, provider_id, created_at)
			SELECT code, type, $3, NULLIF(wallet_id, '')::uuid, NULLIF(provider_id, ''), $4
			  FROM unnest($5::text[], $6::text[], $7::text[], $8::text[]) AS a(code, type, wallet_id, provider_id)
			    ON CONFLICT (code) DO NOTHING
		)
		INSERT INTO journal_postings (transaction_id, account_code, direction, amount_minor, currency, created_at)
		SELECT $1, code, direction, amount, $3, $4
		  FROM unnest($5::text[], $2::text[], $9::bigint[]) AS p(code, direction, amount)`,
		j.TransactionID(), directions, ps[0].Amount.Currency().Code(), j.CreatedAt(),
		codes, types, walletIDs, providerIDs, amounts)
	return classify(err)
}

func (r journalRepo) AccountTotals(ctx context.Context, code string) (app.LedgerTotals, error) {
	var t app.LedgerTotals
	err := r.q.QueryRow(ctx, `
		SELECT COALESCE(SUM(amount_minor) FILTER (WHERE direction = 'CREDIT'), 0)::BIGINT,
		       COALESCE(SUM(amount_minor) FILTER (WHERE direction = 'DEBIT'), 0)::BIGINT,
		       COUNT(*)
		  FROM journal_postings
		 WHERE account_code = $1`, code).Scan(&t.Credits, &t.Debits, &t.Entries)
	return t, classify(err)
}

// TrialBalance faz duas leituras; quem chama as coloca na mesma foto
// (ReadSnapshot) para que fechem entre si.
func (r journalRepo) TrialBalance(ctx context.Context, currency money.Currency) (app.TrialBalanceData, error) {
	var d app.TrialBalanceData
	rows, err := r.q.Query(ctx, `
		SELECT CASE WHEN a.type = 'LIABILITY' THEN $2 ELSE a.code END AS account, a.type,
		       COUNT(DISTINCT a.code),
		       COALESCE(SUM(p.amount_minor) FILTER (WHERE p.direction = 'DEBIT'), 0)::BIGINT,
		       COALESCE(SUM(p.amount_minor) FILTER (WHERE p.direction = 'CREDIT'), 0)::BIGINT
		  FROM journal_postings p
		  JOIN ledger_accounts a ON a.code = p.account_code
		 WHERE p.currency = $1
		 GROUP BY 1, 2
		 ORDER BY 2, 1`, currency.Code(), app.WalletAccountsRow)
	if err != nil {
		return d, classify(err)
	}
	defer rows.Close()
	for rows.Next() {
		var row app.TrialBalanceRow
		var typ string
		if err := rows.Scan(&row.Account, &typ, &row.Accounts, &row.Debits, &row.Credits); err != nil {
			return d, classify(err)
		}
		row.Type = accounting.AccountType(typ)
		d.Rows = append(d.Rows, row)
	}
	if err := rows.Err(); err != nil {
		return d, classify(err)
	}
	err = r.q.QueryRow(ctx, `
		SELECT COALESCE(SUM(balance_minor), 0)::BIGINT, COUNT(*) FROM wallets WHERE currency = $1`,
		currency.Code()).Scan(&d.StoredWalletBalances, &d.Wallets)
	return d, classify(err)
}
