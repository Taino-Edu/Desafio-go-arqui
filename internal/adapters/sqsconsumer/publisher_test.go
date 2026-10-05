package sqsconsumer

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"
	"github.com/google/uuid"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"github.com/Taino-Edu/Desafio-go-arqui/internal/app"
)

const (
	storedTraceID = "4bf92f3577b34da6a3ce929d0e0e4736"
	storedSpanID  = "00f067aa0ba902b7"
	storedParent  = "00-" + storedTraceID + "-" + storedSpanID + "-01"
)

// fakeSQS guarda o que foi enviado; failIDs recusa essas entradas do lote.
type fakeSQS struct {
	single  []*sqs.SendMessageInput
	batches []*sqs.SendMessageBatchInput
	failIDs map[string]bool
	callErr error
}

func (f *fakeSQS) SendMessage(_ context.Context, in *sqs.SendMessageInput, _ ...func(*sqs.Options)) (*sqs.SendMessageOutput, error) {
	f.single = append(f.single, in)
	return &sqs.SendMessageOutput{}, f.callErr
}

func (f *fakeSQS) SendMessageBatch(_ context.Context, in *sqs.SendMessageBatchInput, _ ...func(*sqs.Options)) (*sqs.SendMessageBatchOutput, error) {
	f.batches = append(f.batches, in)
	if f.callErr != nil {
		return nil, f.callErr
	}
	out := &sqs.SendMessageBatchOutput{}
	for _, e := range in.Entries {
		if f.failIDs[aws.ToString(e.Id)] {
			out.Failed = append(out.Failed, types.BatchResultErrorEntry{Id: e.Id, Code: aws.String("InternalError"), Message: aws.String("boom")})
		} else {
			out.Successful = append(out.Successful, types.SendMessageBatchResultEntry{Id: e.Id})
		}
	}
	return out, nil
}

func msg(traceParent string) app.OutboxMessage {
	return app.OutboxMessage{
		EventID: uuid.New(), AggregateType: "wallet", AggregateID: uuid.New(),
		EventType: "WalletBalanceChanged", EventVersion: 1, Payload: []byte(`{}`), TraceParent: traceParent,
	}
}

func newPublisher(api *fakeSQS) (Publisher, *tracetest.SpanRecorder) {
	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
	return Publisher{API: api, QueueURL: func() string { return "q" }, Tracer: tp.Tracer("test"),
		Propagator: propagation.TraceContext{}}, rec
}

// traceparent gravado na outbox -> span "publish" filho dele -> atributo
// traceparent na mensagem apontando para o span "publish".
func TestPublisher_ContinuesStoredTrace(t *testing.T) {
	api := &fakeSQS{failIDs: map[string]bool{"2": true}}
	p, rec := newPublisher(api)

	msgs := []app.OutboxMessage{msg(storedParent), msg(""), msg(storedParent)}
	errs := p.PublishBatch(context.Background(), msgs)
	if errs[0] != nil || errs[1] != nil || errs[2] == nil {
		t.Fatalf("errs = %v", errs)
	}

	spans := rec.Ended()
	if len(spans) != 2 { // a mensagem sem trace gravado não abre span
		t.Fatalf("spans = %d", len(spans))
	}
	entries := api.batches[0].Entries
	for i, s := range spans {
		if s.Name() != "publish WalletBalanceChanged" || s.SpanKind() != trace.SpanKindProducer {
			t.Errorf("span %d = %q %v", i, s.Name(), s.SpanKind())
		}
		if s.SpanContext().TraceID().String() != storedTraceID || s.Parent().SpanID().String() != storedSpanID {
			t.Errorf("span %d não continua o trace gravado: %s / %s", i, s.SpanContext().TraceID(), s.Parent().SpanID())
		}
	}
	if spans[0].Status().Code == codes.Error || spans[1].Status().Code != codes.Error {
		t.Errorf("status = %v / %v (a entrada recusada é erro)", spans[0].Status(), spans[1].Status())
	}

	// o consumidor continua a partir do span "publish", não do original
	got := aws.ToString(entries[0].MessageAttributes["traceparent"].StringValue)
	want := "00-" + storedTraceID + "-" + spans[0].SpanContext().SpanID().String() + "-01"
	if got != want {
		t.Errorf("traceparent da mensagem = %q, want %q", got, want)
	}
	if _, ok := entries[1].MessageAttributes["traceparent"]; ok {
		t.Error("mensagem sem trace gravado não deveria levar traceparent")
	}
	if entries[0].MessageAttributes["eventType"].StringValue == nil {
		t.Error("atributos de roteamento perdidos")
	}
}

func TestPublisher_SingleSendAndCallError(t *testing.T) {
	api := &fakeSQS{callErr: errors.New("network down")}
	p, rec := newPublisher(api)

	if err := p.Publish(context.Background(), msg(storedParent)); err == nil {
		t.Fatal("esperava erro")
	}
	if errs := p.PublishBatch(context.Background(), []app.OutboxMessage{msg(storedParent)}); errs[0] == nil {
		t.Fatal("esperava erro no lote")
	}
	spans := rec.Ended()
	if len(spans) != 2 {
		t.Fatalf("spans = %d (todo span aberto precisa ser encerrado)", len(spans))
	}
	for _, s := range spans {
		if s.Status().Code != codes.Error || !strings.Contains(s.Status().Description, "network down") {
			t.Errorf("status = %v", s.Status())
		}
	}
	if !strings.HasPrefix(aws.ToString(api.single[0].MessageAttributes["traceparent"].StringValue), "00-"+storedTraceID) {
		t.Error("Publish não injetou o traceparent")
	}
}

// Sem Tracer, nada muda: nem span nem atributo novo.
func TestPublisher_WithoutTracer(t *testing.T) {
	api := &fakeSQS{}
	p := Publisher{API: api, QueueURL: func() string { return "q" }}
	if err := p.Publish(context.Background(), msg(storedParent)); err != nil {
		t.Fatal(err)
	}
	if _, ok := api.single[0].MessageAttributes["traceparent"]; ok {
		t.Error("sem tracer não deveria injetar traceparent")
	}
}
