package sqsconsumer

import (
	"context"
	"fmt"
	"strconv"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"

	"github.com/Taino-Edu/Desafio-go-arqui/internal/app"
)

// Publisher entrega eventos da outbox na fila FIFO de saída.
//
// Contrato de roteamento e consumo:
//   - corpo: o envelope JSON gravado na outbox (snapshot imutável; a coluna é
//     JSONB, que normaliza espaços e a ordem das chaves, sem mudar o conteúdo);
//   - MessageGroupId = aggregateId: ordem por carteira / por transação;
//   - MessageDeduplicationId = eventId: republicações dentro de 5 minutos são
//     descartadas pelo próprio SQS; depois disso, o consumidor deduplica por
//     eventId (a entrega é at-least-once);
//   - atributos: eventType, eventVersion, aggregateType, correlationId, para
//     filtrar sem abrir o corpo; com tracing, também traceparent: quem
//     consome continua o trace da requisição que gerou o evento.
type Publisher struct {
	API interface {
		SendMessage(ctx context.Context, in *sqs.SendMessageInput, opts ...func(*sqs.Options)) (*sqs.SendMessageOutput, error)
		SendMessageBatch(ctx context.Context, in *sqs.SendMessageBatchInput, opts ...func(*sqs.Options)) (*sqs.SendMessageBatchOutput, error)
	}
	QueueURL func() string
	// Tracer abre o span "publish" como filho do trace gravado na outbox
	// (OutboxMessage.TraceParent). nil desliga.
	Tracer     trace.Tracer
	Propagator propagation.TextMapPropagator
}

var _ app.BatchPublisher = Publisher{}

// maxSQSBatch é o limite do SendMessageBatch.
const maxSQSBatch = 10

func attributes(m app.OutboxMessage) map[string]types.MessageAttributeValue {
	str := func(v string) types.MessageAttributeValue {
		return types.MessageAttributeValue{DataType: aws.String("String"), StringValue: aws.String(v)}
	}
	attrs := map[string]types.MessageAttributeValue{
		"eventType":     str(m.EventType),
		"eventVersion":  {DataType: aws.String("Number"), StringValue: aws.String(strconv.Itoa(m.EventVersion))},
		"aggregateType": str(m.AggregateType),
	}
	if m.CorrelationID != "" {
		attrs["correlationId"] = str(m.CorrelationID)
	}
	return attrs
}

// startSpan continua o trace gravado na outbox: o publicador roda num
// worker, sem o contexto da requisição, então o pai vem da coluna
// trace_parent. Sem pai (evento gravado sem tracing) não abre span: um trace
// de uma publicação solta só faria ruído. O span devolvido é sempre
// encerrável (noop quando não há pai).
func (p Publisher) startSpan(ctx context.Context, m app.OutboxMessage, attrs map[string]types.MessageAttributeValue) trace.Span {
	if p.Tracer == nil || m.TraceParent == "" {
		return trace.SpanFromContext(context.Background())
	}
	prop := p.Propagator
	if prop == nil {
		prop = propagation.TraceContext{}
	}
	parent := prop.Extract(ctx, propagation.MapCarrier{"traceparent": m.TraceParent})
	if !trace.SpanContextFromContext(parent).IsValid() {
		return trace.SpanFromContext(context.Background())
	}
	ctx, span := p.Tracer.Start(parent, "publish "+m.EventType,
		trace.WithSpanKind(trace.SpanKindProducer),
		trace.WithAttributes(
			attribute.String("messaging.system", "aws_sqs"),
			attribute.String("messaging.operation.type", "send"),
			attribute.String("messaging.message.id", m.EventID.String()),
			attribute.String("messaging.message.conversation_id", m.AggregateID.String()),
		))
	// o consumidor continua a partir DESTE span
	prop.Inject(ctx, attrCarrier(attrs))
	return span
}

func endSpan(span trace.Span, err error) {
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
	}
	span.End()
}

func (p Publisher) Publish(ctx context.Context, m app.OutboxMessage) error {
	url := p.QueueURL()
	if url == "" {
		return fmt.Errorf("events queue not resolved")
	}
	attrs := attributes(m)
	span := p.startSpan(ctx, m, attrs)
	_, err := p.API.SendMessage(ctx, &sqs.SendMessageInput{
		QueueUrl:               aws.String(url),
		MessageBody:            aws.String(string(m.Payload)),
		MessageGroupId:         aws.String(m.AggregateID.String()),
		MessageDeduplicationId: aws.String(m.EventID.String()),
		MessageAttributes:      attrs,
	})
	endSpan(span, err)
	return err
}

// PublishBatch envia em chamadas SendMessageBatch de até 10 mensagens, com o
// mesmo roteamento de Publish. Devolve um erro por mensagem (nil = enviada):
// o SQS aceita ou recusa cada entrada separadamente.
func (p Publisher) PublishBatch(ctx context.Context, msgs []app.OutboxMessage) []error {
	errs := make([]error, len(msgs))
	url := p.QueueURL()
	if url == "" {
		for i := range errs {
			errs[i] = fmt.Errorf("events queue not resolved")
		}
		return errs
	}
	for start := 0; start < len(msgs); start += maxSQSBatch {
		chunk := msgs[start:min(start+maxSQSBatch, len(msgs))]
		entries := make([]types.SendMessageBatchRequestEntry, len(chunk))
		spans := make([]trace.Span, len(chunk)) // um por mensagem: cada uma tem o seu trace
		for i, m := range chunk {
			attrs := attributes(m)
			spans[i] = p.startSpan(ctx, m, attrs)
			entries[i] = types.SendMessageBatchRequestEntry{
				Id:                     aws.String(strconv.Itoa(start + i)),
				MessageBody:            aws.String(string(m.Payload)),
				MessageGroupId:         aws.String(m.AggregateID.String()),
				MessageDeduplicationId: aws.String(m.EventID.String()),
				MessageAttributes:      attrs,
			}
		}
		out, err := p.API.SendMessageBatch(ctx, &sqs.SendMessageBatchInput{QueueUrl: aws.String(url), Entries: entries})
		if err != nil {
			for i := range chunk {
				errs[start+i] = err
				endSpan(spans[i], err)
			}
			continue
		}
		answered := map[int]bool{}
		for _, ok := range out.Successful {
			i, _ := strconv.Atoi(aws.ToString(ok.Id))
			answered[i] = true
		}
		for _, f := range out.Failed {
			i, _ := strconv.Atoi(aws.ToString(f.Id))
			answered[i] = true
			errs[i] = fmt.Errorf("sqs batch entry %s: %s", aws.ToString(f.Code), aws.ToString(f.Message))
		}
		for i := range chunk { // entrada sem resposta: trata como falha
			if !answered[start+i] {
				errs[start+i] = fmt.Errorf("sqs batch entry %d without result", start+i)
			}
			endSpan(spans[i], errs[start+i])
		}
	}
	return errs
}
