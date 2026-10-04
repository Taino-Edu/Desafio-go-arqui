package httpapi

import (
	"context"
	"errors"
	"log/slog"
	"net/http"

	"github.com/Taino-Edu/Desafio-go-arqui/internal/app"
	"github.com/Taino-Edu/Desafio-go-arqui/internal/domain/domainerr"
	"github.com/Taino-Edu/Desafio-go-arqui/internal/domain/money"
)

// Códigos de erro do contrato HTTP (campo error.code).
const (
	CodeInvalidRequest         = "INVALID_REQUEST"
	CodeNotFound               = "NOT_FOUND"
	CodeWalletAlreadyExists    = "WALLET_ALREADY_EXISTS"
	CodeIdempotencyKeyReused   = "IDEMPOTENCY_KEY_REUSED"
	CodeDuplicateTransaction   = "DUPLICATE_TRANSACTION"
	CodeTemporarilyUnavailable = "TEMPORARILY_UNAVAILABLE"
	CodeInternal               = "INTERNAL_ERROR"
)

// ErrorBody é o corpo de toda resposta de erro.
type ErrorBody struct {
	Error ErrorDetail `json:"error"`
}

type ErrorDetail struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Field   string `json:"field,omitempty"`
}

// retryAfterSeconds é sugerido ao cliente em falhas temporárias.
const retryAfterSeconds = "1"

// writeError traduz erros de domínio e de aplicação para status HTTP.
// Erros desconhecidos viram 500 sem expor detalhes internos.
func writeError(w http.ResponseWriter, r *http.Request, log *slog.Logger, err error) {
	status, body := classifyError(err)
	switch {
	case status >= 500 && status != http.StatusServiceUnavailable:
		log.ErrorContext(r.Context(), "request failed", "error", err)
	case status == http.StatusServiceUnavailable:
		log.WarnContext(r.Context(), "transient failure", "error", err)
		w.Header().Set("Retry-After", retryAfterSeconds)
	}
	writeJSON(w, status, body)
}

func classifyError(err error) (int, ErrorBody) {
	var field *domainerr.FieldError
	switch {
	case errors.As(err, &field):
		return http.StatusBadRequest, errBody(CodeInvalidRequest, field.Reason, field.Field)
	case errors.Is(err, domainerr.ErrValidation),
		errors.Is(err, money.ErrInvalidAmount),
		errors.Is(err, money.ErrInvalidCurrency),
		errors.Is(err, app.ErrInvalidCursor),
		errors.Is(err, errBadRequest):
		return http.StatusBadRequest, errBody(CodeInvalidRequest, err.Error(), "")
	case errors.Is(err, app.ErrWalletNotFound):
		return http.StatusNotFound, errBody(CodeNotFound, "wallet not found", "")
	case errors.Is(err, app.ErrTransactionNotFound):
		return http.StatusNotFound, errBody(CodeNotFound, "transaction not found", "")
	case errors.Is(err, app.ErrIdempotencyKeyReused):
		return http.StatusConflict, errBody(CodeIdempotencyKeyReused, err.Error(), "")
	case errors.Is(err, app.ErrDuplicateTransaction):
		return http.StatusConflict, errBody(CodeDuplicateTransaction, err.Error(), "")
	case errors.Is(err, app.ErrWalletAlreadyExists):
		return http.StatusConflict, errBody(CodeWalletAlreadyExists, err.Error(), "")
	case errors.Is(err, app.ErrTransient), errors.Is(err, context.DeadlineExceeded):
		return http.StatusServiceUnavailable, errBody(CodeTemporarilyUnavailable, "temporarily unavailable, retry later", "")
	default:
		return http.StatusInternalServerError, errBody(CodeInternal, "internal error", "")
	}
}

func errBody(code, msg, field string) ErrorBody {
	return ErrorBody{Error: ErrorDetail{Code: code, Message: msg, Field: field}}
}
