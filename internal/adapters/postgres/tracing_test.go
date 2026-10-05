package postgres

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

func TestQueryTracer(t *testing.T) {
	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
	qt := QueryTracer{Tracer: tp.Tracer("test")}

	// sem trace em andamento (varredura de worker): nenhum span
	ctx := qt.TraceQueryStart(context.Background(), nil, pgx.TraceQueryStartData{SQL: "SELECT 1"})
	qt.TraceQueryEnd(ctx, nil, pgx.TraceQueryEndData{})
	if n := len(rec.Ended()) + len(rec.Started()); n != 0 {
		t.Fatalf("sem pai não deveria haver span: %d", n)
	}

	parentCtx, parent := tp.Tracer("test").Start(context.Background(), "request")
	ctx = qt.TraceQueryStart(parentCtx, nil, pgx.TraceQueryStartData{SQL: "\n  update wallets\n   SET balance = $1\n WHERE id = $2"})
	qt.TraceQueryEnd(ctx, nil, pgx.TraceQueryEndData{CommandTag: pgconn.NewCommandTag("UPDATE 1")})
	ctx = qt.TraceQueryStart(parentCtx, nil, pgx.TraceQueryStartData{SQL: "INSERT INTO x VALUES ($1)"})
	qt.TraceQueryEnd(ctx, nil, pgx.TraceQueryEndData{Err: errors.New("unique violation")})

	spans := rec.Ended()
	if len(spans) != 2 {
		t.Fatalf("spans = %d (o da requisição não pode ter sido encerrado)", len(spans))
	}
	if !parent.IsRecording() {
		t.Fatal("TraceQueryEnd encerrou o span da requisição")
	}
	upd := spans[0]
	if upd.Name() != "db UPDATE" || upd.SpanKind() != trace.SpanKindClient ||
		upd.Parent().SpanID() != parent.SpanContext().SpanID() {
		t.Errorf("update = %q %v parent %s", upd.Name(), upd.SpanKind(), upd.Parent().SpanID())
	}
	if got := attrString(upd, "db.query.text"); got != "update wallets SET balance = $1 WHERE id = $2" {
		t.Errorf("db.query.text = %q", got)
	}
	if spans[1].Name() != "db INSERT" || spans[1].Status().Code != codes.Error {
		t.Errorf("insert = %q %v", spans[1].Name(), spans[1].Status())
	}
	parent.End()
}

func TestTraceParent(t *testing.T) {
	if traceParent(context.Background()) != nil {
		t.Error("sem trace: nil (coluna NULL)")
	}
	tp := sdktrace.NewTracerProvider()
	ctx, span := tp.Tracer("test").Start(context.Background(), "request")
	defer span.End()
	got := traceParent(ctx)
	want := "00-" + span.SpanContext().TraceID().String() + "-" + span.SpanContext().SpanID().String() + "-01"
	if got == nil || *got != want {
		t.Errorf("traceParent = %v, want %s", got, want)
	}
}

func attrString(s sdktrace.ReadOnlySpan, key string) string {
	for _, kv := range s.Attributes() {
		if string(kv.Key) == key {
			return kv.Value.AsString()
		}
	}
	return ""
}
