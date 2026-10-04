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
	}
	QueueURL func() string
}

func (p Publisher) Publish(ctx context.Context, m app.OutboxMessage) error {
	url := p.QueueURL()
	if url == "" {
		return fmt.Errorf("events queue not resolved")
	}
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
	_, err := p.API.SendMessage(ctx, &sqs.SendMessageInput{
		QueueUrl:               aws.String(url),
		MessageBody:            aws.String(string(m.Payload)),
		MessageGroupId:         aws.String(m.AggregateID.String()),
		MessageDeduplicationId: aws.String(m.EventID.String()),
		MessageAttributes:      attrs,
	})
	return err
}
