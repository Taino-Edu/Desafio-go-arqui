package observability

import (
	"context"
	"log/slog"

	"go.opentelemetry.io/otel/trace"

	"github.com/Taino-Edu/Desafio-go-arqui/internal/app"
)

// ContextHandler acrescenta a cada log os identificadores guardados no
// contexto: correlationId e, com tracing ligado, traceId e spanId. Todo log
// feito com o contexto de uma requisição ou mensagem (InfoContext,
// ErrorContext...) carrega os ids de rastreio, sem depender de cada chamada
// lembrar de incluí-los.
type ContextHandler struct {
	slog.Handler
}

// NewContextHandler embrulha h.
func NewContextHandler(h slog.Handler) *ContextHandler { return &ContextHandler{Handler: h} }

func (h *ContextHandler) Handle(ctx context.Context, r slog.Record) error {
	if id := app.CorrelationID(ctx); id != "" {
		r.AddAttrs(slog.String("correlationId", id))
	}
	// com tracing ligado: do log se chega ao trace (e vice-versa)
	if sc := trace.SpanContextFromContext(ctx); sc.IsValid() {
		r.AddAttrs(slog.String("traceId", sc.TraceID().String()), slog.String("spanId", sc.SpanID().String()))
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
