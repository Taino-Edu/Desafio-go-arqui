package httpapi

import (
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"

	"github.com/Taino-Edu/Desafio-go-arqui/internal/app"
	"github.com/Taino-Edu/Desafio-go-arqui/internal/domain/domainerr"
	"github.com/Taino-Edu/Desafio-go-arqui/internal/domain/money"
	"github.com/Taino-Edu/Desafio-go-arqui/internal/domain/wallet"
)

// MoneyDTO é o contrato externo de dinheiro: {"amount":"25.00","currency":"BRL"}.
// Os dois campos são strings: o valor nunca passa por float.
type MoneyDTO struct {
	Amount   string `json:"amount"`
	Currency string `json:"currency"`
}

func toMoneyDTO(m money.Money) MoneyDTO {
	return MoneyDTO{Amount: m.Amount(), Currency: m.Currency().Code()}
}

// parseExternalMoney aplica as regras de entrada externa: moeda ISO válida,
// formato canônico "0.00", sem negativos.
func parseExternalMoney(field string, d *MoneyDTO) (money.Money, error) {
	if d == nil {
		return money.Money{}, domainerr.Field(field, "required")
	}
	cur, err := money.NewCurrency(d.Currency)
	if err != nil {
		return money.Money{}, domainerr.Field(field+".currency", err.Error())
	}
	m, err := money.Parse(d.Amount, cur)
	if err != nil {
		return money.Money{}, domainerr.Field(field+".amount", err.Error())
	}
	return m, nil
}

func parseUUID(field, s string) (uuid.UUID, error) {
	id, err := uuid.Parse(s)
	if err != nil || id == uuid.Nil {
		return uuid.Nil, domainerr.Field(field, "must be a valid UUID")
	}
	return id, nil
}

type walletHandlers struct {
	svc *app.WalletService
	log *slog.Logger
}

type openWalletRequest struct {
	PlayerID       string    `json:"playerId"`
	InitialBalance *MoneyDTO `json:"initialBalance"`
}

type walletResponse struct {
	ID        string    `json:"id"`
	PlayerID  string    `json:"playerId"`
	Balance   MoneyDTO  `json:"balance"`
	Version   int64     `json:"version"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

func toWalletResponse(w *wallet.Wallet) walletResponse {
	return walletResponse{
		ID: w.ID().String(), PlayerID: w.PlayerID().String(),
		Balance: toMoneyDTO(w.Balance()), Version: w.Version(),
		CreatedAt: w.CreatedAt(), UpdatedAt: w.UpdatedAt(),
	}
}

// POST /wallets
func (h walletHandlers) open(w http.ResponseWriter, r *http.Request) {
	if !h.authorize(w, r) {
		return
	}
	var req openWalletRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, r, h.log, err)
		return
	}
	playerID, err := parseUUID("playerId", req.PlayerID)
	if err != nil {
		writeError(w, r, h.log, err)
		return
	}
	initial, err := parseExternalMoney("initialBalance", req.InitialBalance)
	if err != nil {
		writeError(w, r, h.log, err)
		return
	}

	wal, err := h.svc.OpenWallet(r.Context(), app.OpenWalletInput{PlayerID: playerID, InitialBalance: initial})
	if err != nil {
		writeError(w, r, h.log, err)
		return
	}
	h.log.InfoContext(r.Context(), "wallet opened", "walletId", wal.ID())
	w.Header().Set("Location", "/wallets/"+wal.ID().String())
	writeJSON(w, http.StatusCreated, toWalletResponse(wal))
}

// GET /wallets/{walletId}
func (h walletHandlers) get(w http.ResponseWriter, r *http.Request) {
	if !h.authorize(w, r) {
		return
	}
	id, err := parseUUID("walletId", r.PathValue("walletId"))
	if err != nil {
		writeError(w, r, h.log, err)
		return
	}
	wal, err := h.svc.GetWallet(r.Context(), id)
	if err != nil {
		writeError(w, r, h.log, err)
		return
	}
	writeJSON(w, http.StatusOK, toWalletResponse(wal))
}

type ledgerEntryResponse struct {
	ID            string    `json:"id"`
	TransactionID string    `json:"transactionId"`
	WalletVersion int64     `json:"walletVersion"`
	Direction     string    `json:"direction"`
	Money         MoneyDTO  `json:"money"`
	BalanceBefore MoneyDTO  `json:"balanceBefore"`
	BalanceAfter  MoneyDTO  `json:"balanceAfter"`
	CreatedAt     time.Time `json:"createdAt"`
}

type ledgerPageResponse struct {
	WalletID   string                `json:"walletId"`
	Items      []ledgerEntryResponse `json:"items"`
	NextCursor *string               `json:"nextCursor"`
}

// GET /wallets/{walletId}/ledger?cursor=...&limit=50
func (h walletHandlers) ledger(w http.ResponseWriter, r *http.Request) {
	if !h.authorize(w, r) {
		return
	}
	id, err := parseUUID("walletId", r.PathValue("walletId"))
	if err != nil {
		writeError(w, r, h.log, err)
		return
	}
	limit := 0
	if s := r.URL.Query().Get("limit"); s != "" {
		limit, err = strconv.Atoi(s)
		if err != nil || limit < 1 || limit > app.MaxLedgerPageSize {
			writeError(w, r, h.log, domainerr.Field("limit", fmt.Sprintf("must be between 1 and %d", app.MaxLedgerPageSize)))
			return
		}
	}

	page, err := h.svc.ListLedger(r.Context(), id, r.URL.Query().Get("cursor"), limit)
	if err != nil {
		writeError(w, r, h.log, err)
		return
	}
	resp := ledgerPageResponse{WalletID: id.String(), Items: make([]ledgerEntryResponse, 0, len(page.Entries))}
	for _, e := range page.Entries {
		resp.Items = append(resp.Items, ledgerEntryResponse{
			ID: e.ID().String(), TransactionID: e.TransactionID().String(), WalletVersion: e.WalletVersion(),
			Direction: string(e.Direction()), Money: toMoneyDTO(e.Amount()),
			BalanceBefore: toMoneyDTO(e.BalanceBefore()), BalanceAfter: toMoneyDTO(e.BalanceAfter()),
			CreatedAt: e.CreatedAt(),
		})
	}
	if page.NextCursor != "" {
		resp.NextCursor = &page.NextCursor
	}
	writeJSON(w, http.StatusOK, resp)
}

type reconciliationResponse struct {
	WalletID          string   `json:"walletId"`
	StoredBalance     MoneyDTO `json:"storedBalance"`
	CalculatedBalance MoneyDTO `json:"calculatedBalance"`
	Difference        MoneyDTO `json:"difference"`
	Consistent        bool     `json:"consistent"`
	CheckedEntries    int64    `json:"checkedEntries"`
}

// POST /wallets/{walletId}/reconciliation
//
// 200 sempre que a conferência roda: divergência é um RESULTADO
// (consistent=false), não um erro do pedido. Ela também vira log de erro e
// métrica (reconciliation_mismatch_total). Nada é alterado.
func (h walletHandlers) reconcile(w http.ResponseWriter, r *http.Request) {
	if !h.authorize(w, r) {
		return
	}
	id, err := parseUUID("walletId", r.PathValue("walletId"))
	if err != nil {
		writeError(w, r, h.log, err)
		return
	}
	rec, err := h.svc.Reconcile(r.Context(), id)
	if err != nil {
		writeError(w, r, h.log, err)
		return
	}
	if rec.Consistent {
		h.log.InfoContext(r.Context(), "reconciliation consistent",
			"walletId", id, "checkedEntries", rec.CheckedEntries)
	} else {
		// só a diferença: o suficiente para investigar, sem expor saldos no log
		h.log.ErrorContext(r.Context(), "reconciliation mismatch",
			"walletId", id, "difference", rec.Difference.String(), "checkedEntries", rec.CheckedEntries)
	}
	writeJSON(w, http.StatusOK, reconciliationResponse{
		WalletID:          id.String(),
		StoredBalance:     toMoneyDTO(rec.StoredBalance),
		CalculatedBalance: toMoneyDTO(rec.CalculatedBalance),
		Difference:        toMoneyDTO(rec.Difference),
		Consistent:        rec.Consistent,
		CheckedEntries:    rec.CheckedEntries,
	})
}

// authorize: operações de carteira são restritas ao serviço interno.
func (h walletHandlers) authorize(w http.ResponseWriter, r *http.Request) bool {
	p, err := principal(r)
	if err == nil {
		err = p.CanManageWallets()
	}
	if err != nil {
		writeError(w, r, h.log, err)
		return false
	}
	return true
}
