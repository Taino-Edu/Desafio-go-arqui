package httpapi

import (
	"net/http"

	"github.com/Taino-Edu/Desafio-go-arqui/internal/domain/domainerr"
	"github.com/Taino-Edu/Desafio-go-arqui/internal/domain/money"
)

type trialBalanceAccountResponse struct {
	Account  string   `json:"account"`
	Type     string   `json:"type"`
	Accounts int64    `json:"accounts"`
	Debits   MoneyDTO `json:"debits"`
	Credits  MoneyDTO `json:"credits"`
	Balance  MoneyDTO `json:"balance"`
}

type trialBalanceWalletsResponse struct {
	Count          int64    `json:"count"`
	JournalBalance MoneyDTO `json:"journalBalance"`
	StoredBalance  MoneyDTO `json:"storedBalance"`
	Consistent     bool     `json:"consistent"`
}

type trialBalanceResponse struct {
	Currency     string                        `json:"currency"`
	Balanced     bool                          `json:"balanced"`
	TotalDebits  MoneyDTO                      `json:"totalDebits"`
	TotalCredits MoneyDTO                      `json:"totalCredits"`
	Accounts     []trialBalanceAccountResponse `json:"accounts"`
	Wallets      trialBalanceWalletsResponse   `json:"wallets"`
}

// GET /accounting/trial-balance?currency=BRL
//
// Balancete do razão em partidas dobradas: soma de cada conta da casa, das
// carteiras (numa linha), Σ débitos x Σ créditos, e o passivo com os
// jogadores no razão x a soma dos saldos gravados. Como a reconciliação,
// uma divergência é RESULTADO (200 com balanced/consistent=false) e vira log
// de erro. Restrito ao serviço interno.
func (h walletHandlers) trialBalance(w http.ResponseWriter, r *http.Request) {
	if !h.authorize(w, r) {
		return
	}
	cur, err := money.NewCurrency(r.URL.Query().Get("currency"))
	if err != nil {
		writeError(w, r, h.log, domainerr.Field("currency", err.Error()))
		return
	}
	tb, err := h.svc.TrialBalance(r.Context(), cur)
	if err != nil {
		writeError(w, r, h.log, err)
		return
	}
	if !tb.Balanced || !tb.WalletsMatch {
		h.log.ErrorContext(r.Context(), "trial balance mismatch", "currency", cur.Code(),
			"balanced", tb.Balanced, "walletsMatch", tb.WalletsMatch)
	}
	resp := trialBalanceResponse{
		Currency: cur.Code(), Balanced: tb.Balanced,
		TotalDebits: toMoneyDTO(tb.TotalDebits), TotalCredits: toMoneyDTO(tb.TotalCredits),
		Accounts: []trialBalanceAccountResponse{},
		Wallets: trialBalanceWalletsResponse{
			Count: tb.Wallets, JournalBalance: toMoneyDTO(tb.WalletLiabilities),
			StoredBalance: toMoneyDTO(tb.StoredWalletBalances), Consistent: tb.WalletsMatch,
		},
	}
	for _, a := range tb.Accounts {
		resp.Accounts = append(resp.Accounts, trialBalanceAccountResponse{
			Account: a.Account, Type: string(a.Type), Accounts: a.Accounts,
			Debits: toMoneyDTO(a.Debits), Credits: toMoneyDTO(a.Credits), Balance: toMoneyDTO(a.Balance),
		})
	}
	writeJSON(w, http.StatusOK, resp)
}
