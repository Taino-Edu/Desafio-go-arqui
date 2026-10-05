package postgres

import (
	"context"
	"strings"

	"github.com/jackc/pgx/v5"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

// QueryTracer cria um span por comando SQL. Só quando já existe um trace em
// andamento (uma requisição, uma mensagem): as varreduras periódicas dos
// workers, sem trace, não geram spans soltos a cada segundo. O texto do SQL
// vai no span; os valores não (são parâmetros $1, $2...).
type QueryTracer struct {
	Tracer trace.Tracer
}

var _ pgx.QueryTracer = QueryTracer{}

type querySpanKey struct{}

func (t QueryTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if !trace.SpanContextFromContext(ctx).IsValid() {
		return ctx
	}
	sql := compactSQL(data.SQL)
	ctx, span := t.Tracer.Start(ctx, "db "+sqlOperation(sql),
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(
			attribute.String("db.system.name", "postgresql"),
			attribute.String("db.query.text", sql),
		))
	return context.WithValue(ctx, querySpanKey{}, span)
}

func (t QueryTracer) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryEndData) {
	// só encerra o span que esta query abriu (nunca o da requisição)
	span, ok := ctx.Value(querySpanKey{}).(trace.Span)
	if !ok {
		return
	}
	if data.Err != nil {
		span.RecordError(data.Err)
		span.SetStatus(codes.Error, "query failed")
	} else {
		span.SetAttributes(attribute.Int64("db.response.rows", data.CommandTag.RowsAffected()))
	}
	span.End()
}

// compactSQL junta o SQL numa linha e limita o tamanho.
func compactSQL(sql string) string {
	s := strings.Join(strings.Fields(sql), " ")
	if len(s) > 1000 {
		s = s[:1000] + "..."
	}
	return s
}

// sqlOperation devolve a primeira palavra (SELECT, INSERT, UPDATE, WITH...).
func sqlOperation(sql string) string {
	op, _, _ := strings.Cut(sql, " ")
	return strings.ToUpper(op)
}

// traceParent devolve o traceparent W3C do contexto ("" sem trace): é o que
// a outbox grava junto com o evento, para que a publicação, mais tarde e
// talvez em outra instância, continue o mesmo trace.
func traceParent(ctx context.Context) *string {
	if !trace.SpanContextFromContext(ctx).IsValid() {
		return nil
	}
	carrier := propagation.MapCarrier{}
	propagation.TraceContext{}.Inject(ctx, carrier)
	v := carrier.Get("traceparent")
	if v == "" {
		return nil
	}
	return &v
}
