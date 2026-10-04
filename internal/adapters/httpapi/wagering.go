package httpapi

import (
	"log/slog"
	"net/http"
	"regexp"
	"time"

	"github.com/Taino-Edu/Desafio-go-arqui/internal/app"
	"github.com/Taino-Edu/Desafio-go-arqui/internal/domain/domainerr"
	"github.com/Taino-Edu/Desafio-go-arqui/internal/domain/wagering"
)

// HeaderIdempotencyKey é obrigatório no envio de operações.
const HeaderIdempotencyKey = "Idempotency-Key"

// chave: texto visível, sem espaços nem controle, até 255 caracteres
var validIdempotencyKey = regexp.MustCompile(`^[\x21-\x7E]{1,255}$`)

type wagerHandlers struct {
	svc *app.WagerService
	log *slog.Logger
}

type submitRequest struct {
	ProviderID                     string    `json:"providerId"`
	ExternalTransactionID          string    `json:"externalTransactionId"`
	PlayerID                       string    `json:"playerId"`
	WalletID                       string    `json:"walletId"`
	RoundID                        string    `json:"roundId"`
	GameID                         string    `json:"gameId"`
	Kind                           string    `json:"kind"`
	Money                          *MoneyDTO `json:"money"`
	ReferenceExternalTransactionID string    `json:"referenceExternalTransactionId,omitempty"`
}

// submitResponse é a resposta do envio (novo ou replay).
type submitResponse struct {
	TransactionID      string     `json:"transactionId"`
	Status             string     `json:"status"`
	Balance            *MoneyDTO  `json:"balance,omitempty"` // saldo observado no processamento original
	FailureCode        string     `json:"failureCode,omitempty"`
	FailureCorrectable *bool      `json:"failureCorrectable,omitempty"`
	NextAttemptAt      *time.Time `json:"nextAttemptAt,omitempty"`
	IdempotentReplay   bool       `json:"idempotentReplay"`
}

// POST /wagering/transactions
//
// Status HTTP pelo desfecho:
//
//	201 PROCESSED (novo)             200 PROCESSED (replay)
//	202 PENDING_REFERENCE            422 REJECTED / FAILED (novo ou replay)
//	400 entrada inválida             409 chave reutilizada / operação duplicada
//	503 indisponibilidade transitória
func (h wagerHandlers) submit(w http.ResponseWriter, r *http.Request) {
	key := r.Header.Get(HeaderIdempotencyKey)
	if !validIdempotencyKey.MatchString(key) {
		writeError(w, r, h.log, domainerr.Field(HeaderIdempotencyKey, "header is required (1-255 visible ASCII characters)"))
		return
	}
	var req submitRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, r, h.log, err)
		return
	}
	in, err := req.toInput(key)
	if err != nil {
		writeError(w, r, h.log, err)
		return
	}
	// O provedor é o da identidade autenticada. Um corpo com outro
	// providerId é recusado ANTES de qualquer gravação.
	p, err := principal(r)
	if err == nil {
		err = p.CanSubmitFor(in.ProviderID)
	}
	if err != nil {
		h.log.WarnContext(r.Context(), "submit forbidden", "clientId", p.ClientID,
			"identityProviderId", p.ProviderID, "bodyProviderId", in.ProviderID)
		writeError(w, r, h.log, err)
		return
	}

	res, err := h.svc.Submit(r.Context(), in)
	if err != nil {
		writeError(w, r, h.log, err)
		return
	}
	tx := res.Transaction
	h.log.InfoContext(r.Context(), "wager transaction submitted",
		"transactionId", tx.ID(), "walletId", tx.WalletID(), "providerId", in.ProviderID,
		"kind", tx.Kind(), "status", tx.Status(), "failureCode", tx.FailureCode(),
		"idempotentReplay", res.Replay)

	writeJSON(w, submitStatus(tx.Status(), res.Replay), toSubmitResponse(tx, res.Replay))
}

func (req submitRequest) toInput(key string) (app.SubmitInput, error) {
	required := []struct{ name, value string }{
		{"providerId", req.ProviderID}, {"externalTransactionId", req.ExternalTransactionID},
		{"roundId", req.RoundID}, {"gameId", req.GameID}, {"kind", req.Kind},
	}
	for _, f := range required {
		if f.value == "" {
			return app.SubmitInput{}, domainerr.Field(f.name, "required")
		}
	}
	playerID, err := parseUUID("playerId", req.PlayerID)
	if err != nil {
		return app.SubmitInput{}, err
	}
	walletID, err := parseUUID("walletId", req.WalletID)
	if err != nil {
		return app.SubmitInput{}, err
	}
	m, err := parseExternalMoney("money", req.Money)
	if err != nil {
		return app.SubmitInput{}, err
	}
	return app.SubmitInput{
		ProviderID: req.ProviderID, ExternalTransactionID: req.ExternalTransactionID,
		IdempotencyKey: key, PlayerID: playerID, WalletID: walletID,
		RoundID: req.RoundID, GameID: req.GameID, Kind: req.Kind, Money: m,
		ReferenceExternalTransactionID: req.ReferenceExternalTransactionID,
	}, nil
}

func submitStatus(s wagering.Status, replay bool) int {
	switch s {
	case wagering.StatusProcessed:
		if replay {
			return http.StatusOK
		}
		return http.StatusCreated
	case wagering.StatusPending, wagering.StatusPendingReference:
		return http.StatusAccepted
	default: // REJECTED, FAILED
		return http.StatusUnprocessableEntity
	}
}

func toSubmitResponse(tx *wagering.WagerTransaction, replay bool) submitResponse {
	resp := submitResponse{TransactionID: tx.ID().String(), Status: string(tx.Status()), IdempotentReplay: replay}
	if b, ok := tx.BalanceAfter(); ok {
		dto := toMoneyDTO(b)
		resp.Balance = &dto
	}
	if fc := tx.FailureCode(); fc != "" {
		resp.FailureCode = string(fc)
		c := fc.IsCorrectable()
		resp.FailureCorrectable = &c
	}
	if n, ok := tx.NextAttemptAt(); ok {
		resp.NextAttemptAt = &n
	}
	return resp
}

// transactionResponse é a visão completa para consultas.
type transactionResponse struct {
	TransactionID                  string     `json:"transactionId"`
	Origin                         string     `json:"origin"`
	Kind                           string     `json:"kind"`
	Status                         string     `json:"status"`
	WalletID                       string     `json:"walletId"`
	PlayerID                       string     `json:"playerId"`
	Money                          MoneyDTO   `json:"money"`
	ProviderID                     string     `json:"providerId,omitempty"`
	ExternalTransactionID          string     `json:"externalTransactionId,omitempty"`
	RoundID                        string     `json:"roundId,omitempty"`
	GameID                         string     `json:"gameId,omitempty"`
	ReferenceExternalTransactionID string     `json:"referenceExternalTransactionId,omitempty"`
	ReferenceTransactionID         string     `json:"referenceTransactionId,omitempty"`
	BalanceAfter                   *MoneyDTO  `json:"balanceAfter,omitempty"`
	FailureCode                    string     `json:"failureCode,omitempty"`
	FailureCorrectable             *bool      `json:"failureCorrectable,omitempty"`
	Attempts                       int        `json:"attempts"`
	NextAttemptAt                  *time.Time `json:"nextAttemptAt,omitempty"`
	CreatedAt                      time.Time  `json:"createdAt"`
	UpdatedAt                      time.Time  `json:"updatedAt"`
}

func toTransactionResponse(tx *wagering.WagerTransaction) transactionResponse {
	sr := toSubmitResponse(tx, false)
	resp := transactionResponse{
		TransactionID: tx.ID().String(), Origin: string(tx.Origin()), Kind: string(tx.Kind()),
		Status: string(tx.Status()), WalletID: tx.WalletID().String(), PlayerID: tx.PlayerID().String(),
		Money: toMoneyDTO(tx.Amount()), BalanceAfter: sr.Balance, FailureCode: sr.FailureCode,
		FailureCorrectable: sr.FailureCorrectable, Attempts: tx.Attempts(), NextAttemptAt: sr.NextAttemptAt,
		CreatedAt: tx.CreatedAt(), UpdatedAt: tx.UpdatedAt(),
	}
	if ext := tx.External(); ext != nil {
		resp.ProviderID, resp.ExternalTransactionID = ext.ProviderID, ext.ExternalTransactionID
		resp.RoundID, resp.GameID = ext.RoundID, ext.GameID
		resp.ReferenceExternalTransactionID = ext.ReferenceExternalTransactionID
	}
	if id, ok := tx.ReferenceTransactionID(); ok {
		resp.ReferenceTransactionID = id.String()
	}
	return resp
}

// GET /wagering/transactions/{transactionId}
func (h wagerHandlers) getByID(w http.ResponseWriter, r *http.Request) {
	id, err := parseUUID("transactionId", r.PathValue("transactionId"))
	if err != nil {
		writeError(w, r, h.log, err)
		return
	}
	p, err := principal(r)
	if err != nil {
		writeError(w, r, h.log, err)
		return
	}
	tx, err := h.svc.GetTransaction(r.Context(), id)
	if err == nil {
		err = p.CanReadTransaction(tx) // de outro provedor: 404, sem revelar que existe
	}
	if err != nil {
		writeError(w, r, h.log, err)
		return
	}
	writeJSON(w, http.StatusOK, toTransactionResponse(tx))
}

// GET /providers/{providerId}/wagering/transactions/{externalTransactionId}
func (h wagerHandlers) getByExternalID(w http.ResponseWriter, r *http.Request) {
	p, err := principal(r)
	if err == nil {
		err = p.CanQueryProvider(r.PathValue("providerId"))
	}
	if err != nil {
		writeError(w, r, h.log, err)
		return
	}
	tx, err := h.svc.GetByExternalID(r.Context(), r.PathValue("providerId"), r.PathValue("externalTransactionId"))
	if err != nil {
		writeError(w, r, h.log, err)
		return
	}
	writeJSON(w, http.StatusOK, toTransactionResponse(tx))
}
