package sqsconsumer

import (
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"
	"go.opentelemetry.io/otel/propagation"
)

// attrCarrier deixa o propagador do OpenTelemetry ler e gravar o contexto de
// trace (traceparent, tracestate) nos atributos de uma mensagem SQS.
type attrCarrier map[string]types.MessageAttributeValue

var _ propagation.TextMapCarrier = attrCarrier{}

func (c attrCarrier) Get(key string) string {
	if v, ok := c[key]; ok {
		return aws.ToString(v.StringValue)
	}
	return ""
}

func (c attrCarrier) Set(key, value string) {
	c[key] = types.MessageAttributeValue{DataType: aws.String("String"), StringValue: aws.String(value)}
}

func (c attrCarrier) Keys() []string {
	keys := make([]string, 0, len(c))
	for k := range c {
		keys = append(keys, k)
	}
	return keys
}

// traceAttributes são os atributos de trace pedidos no ReceiveMessage.
var traceAttributes = []string{"traceparent", "tracestate"}
