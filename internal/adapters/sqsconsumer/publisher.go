package sqsconsumer

import (
	"context"
	"fmt"
	"strconv"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"

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
//     filtrar sem abrir o corpo.
type Publisher struct {
	API interface {
		SendMessage(ctx context.Context, in *sqs.SendMessageInput, opts ...func(*sqs.Options)) (*sqs.SendMessageOutput, error)
		SendMessageBatch(ctx context.Context, in *sqs.SendMessageBatchInput, opts ...func(*sqs.Options)) (*sqs.SendMessageBatchOutput, error)
	}
	QueueURL func() string
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

func (p Publisher) Publish(ctx context.Context, m app.OutboxMessage) error {
	url := p.QueueURL()
	if url == "" {
		return fmt.Errorf("events queue not resolved")
	}
	_, err := p.API.SendMessage(ctx, &sqs.SendMessageInput{
		QueueUrl:               aws.String(url),
		MessageBody:            aws.String(string(m.Payload)),
		MessageGroupId:         aws.String(m.AggregateID.String()),
		MessageDeduplicationId: aws.String(m.EventID.String()),
		MessageAttributes:      attributes(m),
	})
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
		for i, m := range chunk {
			entries[i] = types.SendMessageBatchRequestEntry{
				Id:                     aws.String(strconv.Itoa(start + i)),
				MessageBody:            aws.String(string(m.Payload)),
				MessageGroupId:         aws.String(m.AggregateID.String()),
				MessageDeduplicationId: aws.String(m.EventID.String()),
				MessageAttributes:      attributes(m),
			}
		}
		out, err := p.API.SendMessageBatch(ctx, &sqs.SendMessageBatchInput{QueueUrl: aws.String(url), Entries: entries})
		if err != nil {
			for i := range chunk {
				errs[start+i] = err
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
		}
	}
	return errs
}
