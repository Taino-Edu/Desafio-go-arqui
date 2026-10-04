// Package sqstest cria filas FIFO descartáveis no LocalStack do docker
// compose para os testes de integração.
//
// Variável opcional: TEST_SQS_ENDPOINT (padrão http://localhost:4566).
package sqstest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"

	"github.com/Taino-Edu/Desafio-go-arqui/internal/adapters/sqsconsumer"
)

const region = "us-east-1"

// Endpoint do LocalStack.
func Endpoint() string {
	if v := os.Getenv("TEST_SQS_ENDPOINT"); v != "" {
		return v
	}
	return "http://localhost:4566"
}

// ClientConfig devolve a configuração de cliente para o LocalStack.
func ClientConfig() sqsconsumer.ClientConfig {
	return sqsconsumer.ClientConfig{Region: region, Endpoint: Endpoint(), AccessKeyID: "test", SecretAccessKey: "test"}
}

// Queues é um par fila + DLQ criado para um teste.
type Queues struct {
	t             testing.TB
	Client        *sqs.Client
	Name, DLQName string
	URL, DLQURL   string
}

// New cria fila e DLQ FIFO com redrive (maxReceiveCount) e visibilidade
// informados. As filas são apagadas no fim do teste.
func New(t testing.TB, visibility time.Duration, maxReceiveCount int) *Queues {
	t.Helper()
	ctx := context.Background()
	client, err := sqsconsumer.NewClient(ctx, ClientConfig())
	if err != nil {
		t.Fatal(err)
	}
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	suffix := hex.EncodeToString(b)
	q := &Queues{t: t, Client: client, Name: "test-" + suffix + ".fifo", DLQName: "test-" + suffix + "-dlq.fifo"}

	dlq, err := client.CreateQueue(ctx, &sqs.CreateQueueInput{QueueName: aws.String(q.DLQName),
		Attributes: map[string]string{"FifoQueue": "true", "ContentBasedDeduplication": "false"}})
	if err != nil {
		t.Fatalf("sqstest: criar DLQ (LocalStack no ar? docker compose up -d localstack): %v", err)
	}
	q.DLQURL = aws.ToString(dlq.QueueUrl)
	arn := "arn:aws:sqs:" + region + ":000000000000:" + q.DLQName
	redrive, _ := json.Marshal(map[string]string{"deadLetterTargetArn": arn, "maxReceiveCount": strconv.Itoa(maxReceiveCount)})
	main, err := client.CreateQueue(ctx, &sqs.CreateQueueInput{QueueName: aws.String(q.Name),
		Attributes: map[string]string{
			"FifoQueue": "true", "ContentBasedDeduplication": "false",
			"VisibilityTimeout": strconv.Itoa(int(visibility / time.Second)),
			"RedrivePolicy":     string(redrive),
		}})
	if err != nil {
		t.Fatal(err)
	}
	q.URL = aws.ToString(main.QueueUrl)
	t.Cleanup(func() {
		_, _ = client.DeleteQueue(context.Background(), &sqs.DeleteQueueInput{QueueUrl: aws.String(q.URL)})
		_, _ = client.DeleteQueue(context.Background(), &sqs.DeleteQueueInput{QueueUrl: aws.String(q.DLQURL)})
	})
	return q
}

// Message monta um envelope WagerTransactionRequested.
func Message(messageID string, data map[string]any) string {
	b, _ := json.Marshal(map[string]any{
		"messageId": messageID, "type": sqsconsumer.MessageType,
		"occurredAt": time.Now().UTC().Format(time.RFC3339Nano), "data": data,
	})
	return string(b)
}

// Send publica um corpo na fila. dedupID distinto permite simular reenvios
// do produtor (o SQS FIFO só deduplica dentro de 5 minutos e pelo mesmo id).
func (q *Queues) Send(body, groupID, dedupID string) {
	q.t.Helper()
	_, err := q.Client.SendMessage(context.Background(), &sqs.SendMessageInput{
		QueueUrl: aws.String(q.URL), MessageBody: aws.String(body),
		MessageGroupId: aws.String(groupID), MessageDeduplicationId: aws.String(dedupID),
	})
	if err != nil {
		q.t.Fatalf("send: %v", err)
	}
}

// Depth devolve mensagens visíveis + em processamento da fila (ou da DLQ).
func (q *Queues) Depth(dlq bool) int {
	q.t.Helper()
	url := q.URL
	if dlq {
		url = q.DLQURL
	}
	out, err := q.Client.GetQueueAttributes(context.Background(), &sqs.GetQueueAttributesInput{
		QueueUrl: aws.String(url),
		AttributeNames: []types.QueueAttributeName{
			types.QueueAttributeNameApproximateNumberOfMessages,
			types.QueueAttributeNameApproximateNumberOfMessagesNotVisible,
		},
	})
	if err != nil {
		q.t.Fatalf("attributes: %v", err)
	}
	a, _ := strconv.Atoi(out.Attributes[string(types.QueueAttributeNameApproximateNumberOfMessages)])
	b, _ := strconv.Atoi(out.Attributes[string(types.QueueAttributeNameApproximateNumberOfMessagesNotVisible)])
	return a + b
}

// DLQMessage é uma mensagem lida da DLQ.
type DLQMessage struct {
	Body   string
	Reason string
}

// ReadDLQ consome (lê e apaga) até n mensagens da DLQ, esperando até
// timeout. Apagar é necessário: numa fila FIFO, enquanto uma mensagem de um
// grupo está em leitura, as seguintes do mesmo grupo não são entregues.
func (q *Queues) ReadDLQ(n int, timeout time.Duration) []DLQMessage {
	q.t.Helper()
	var out []DLQMessage
	for _, m := range q.consume(q.DLQURL, n, timeout) {
		out = append(out, DLQMessage{Body: m.Body, Reason: m.Attributes["failure-reason"]})
	}
	return out
}

// Received é uma mensagem lida de uma fila.
type Received struct {
	Body       string
	GroupID    string
	DedupID    string
	Attributes map[string]string
}

// ReadQueue consome até n mensagens da fila principal, em ordem de entrega.
func (q *Queues) ReadQueue(n int, timeout time.Duration) []Received {
	q.t.Helper()
	return q.consume(q.URL, n, timeout)
}

func (q *Queues) consume(url string, n int, timeout time.Duration) []Received {
	q.t.Helper()
	var out []Received
	deadline := time.Now().Add(timeout)
	for len(out) < n && time.Now().Before(deadline) {
		res, err := q.Client.ReceiveMessage(context.Background(), &sqs.ReceiveMessageInput{
			QueueUrl: aws.String(url), MaxNumberOfMessages: 10, WaitTimeSeconds: 1,
			VisibilityTimeout: 30, MessageAttributeNames: []string{"All"},
			MessageSystemAttributeNames: []types.MessageSystemAttributeName{
				types.MessageSystemAttributeNameMessageGroupId,
				types.MessageSystemAttributeNameMessageDeduplicationId,
			},
		})
		if err != nil {
			q.t.Fatalf("receive %s: %v", url, err)
		}
		for _, m := range res.Messages {
			r := Received{
				Body:       aws.ToString(m.Body),
				GroupID:    m.Attributes[string(types.MessageSystemAttributeNameMessageGroupId)],
				DedupID:    m.Attributes[string(types.MessageSystemAttributeNameMessageDeduplicationId)],
				Attributes: map[string]string{},
			}
			for k, v := range m.MessageAttributes {
				r.Attributes[k] = aws.ToString(v.StringValue)
			}
			out = append(out, r)
			_, _ = q.Client.DeleteMessage(context.Background(), &sqs.DeleteMessageInput{
				QueueUrl: aws.String(url), ReceiptHandle: m.ReceiptHandle,
			})
		}
	}
	return out
}

// WaitEmpty espera a fila principal esvaziar (tudo tratado e apagado).
func (q *Queues) WaitEmpty(timeout time.Duration) {
	q.t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if q.Depth(false) == 0 {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	q.t.Fatalf("fila %s não esvaziou em %v (restam %d)", q.Name, timeout, q.Depth(false))
}

// Unique devolve um id curto aleatório.
func Unique(prefix string) string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return fmt.Sprintf("%s-%s", prefix, hex.EncodeToString(b))
}
