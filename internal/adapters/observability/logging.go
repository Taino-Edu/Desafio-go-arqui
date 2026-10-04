package observability

import (
	"context"
	"log/slog"

	"github.com/Taino-Edu/Desafio-go-arqui/internal/app"
)

// ContextHandler acrescenta a cada log os identificadores guardados no
// contexto (hoje, o correlationId). Todo log feito com o contexto de uma
// requisição ou mensagem (InfoContext, ErrorContext...) carrega o id de
// rastreio, sem depender de cada chamada lembrar de incluí-lo.
type ContextHandler struct {
	slog.Handler
}

// NewContextHandler embrulha h.
func NewContextHandler(h slog.Handler) *ContextHandler { return &ContextHandler{Handler: h} }

func (h *ContextHandler) Handle(ctx context.Context, r slog.Record) error {
	if id := app.CorrelationID(ctx); id != "" {
		r.AddAttrs(slog.String("correlationId", id))
	}
	return h.Handler.Handle(ctx, r)
}

// WithAttrs e WithGroup preservam o embrulho nos loggers derivados
// (log.With(...)).
func (h *ContextHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &ContextHandler{Handler: h.Handler.WithAttrs(attrs)}
}

func (h *ContextHandler) WithGroup(name string) slog.Handler {
	return &ContextHandler{Handler: h.Handler.WithGroup(name)}
}
