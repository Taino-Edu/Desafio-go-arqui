package sqsconsumer

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"
)

// ClientConfig descreve como acessar o SQS.
type ClientConfig struct {
	Region   string
	Endpoint string // vazio na AWS; http://localhost:4566 no LocalStack
	// Credenciais estáticas opcionais. Vazias: cadeia padrão da AWS
	// (variáveis de ambiente, perfil, papel da instância/tarefa).
	AccessKeyID     string
	SecretAccessKey string
}

// NewClient cria o cliente SQS.
func NewClient(ctx context.Context, cfg ClientConfig) (*sqs.Client, error) {
	opts := []func(*awsconfig.LoadOptions) error{awsconfig.WithRegion(cfg.Region)}
	if cfg.AccessKeyID != "" {
		opts = append(opts, awsconfig.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider(cfg.AccessKeyID, cfg.SecretAccessKey, "")))
	}
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("aws config: %w", err)
	}
	return sqs.NewFromConfig(awsCfg, func(o *sqs.Options) {
		if cfg.Endpoint != "" {
			o.BaseEndpoint = aws.String(cfg.Endpoint)
		}
	}), nil
}

// ResolveQueueURL aceita um nome ou uma URL. Com um nome, consulta o SQS:
// isso também valida, na partida, que a fila existe e é acessível.
func ResolveQueueURL(ctx context.Context, c *sqs.Client, nameOrURL string) (string, error) {
	if strings.HasPrefix(nameOrURL, "http://") || strings.HasPrefix(nameOrURL, "https://") {
		return nameOrURL, nil
	}
	out, err := c.GetQueueUrl(ctx, &sqs.GetQueueUrlInput{QueueName: aws.String(nameOrURL)})
	if err != nil {
		return "", fmt.Errorf("sqs queue %q: %w", nameOrURL, err)
	}
	return aws.ToString(out.QueueUrl), nil
}

// QueueDepth devolve o número aproximado de mensagens visíveis na fila.
func QueueDepth(ctx context.Context, c *sqs.Client, url string) (int64, error) {
	attr := types.QueueAttributeNameApproximateNumberOfMessages
	out, err := c.GetQueueAttributes(ctx, &sqs.GetQueueAttributesInput{
		QueueUrl: aws.String(url), AttributeNames: []types.QueueAttributeName{attr},
	})
	if err != nil {
		return 0, err
	}
	n, err := strconv.ParseInt(out.Attributes[string(attr)], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("sqs %s: %w", attr, err)
	}
	return n, nil
}

// HealthChecker verifica se a fila de entrada responde (readiness).
type HealthChecker struct {
	Client   *sqs.Client
	QueueURL func() string
}

func (HealthChecker) Name() string { return "sqs" }

func (h HealthChecker) Check(ctx context.Context) error {
	url := h.QueueURL()
	if url == "" {
		return fmt.Errorf("sqs queue not resolved yet")
	}
	_, err := h.Client.GetQueueAttributes(ctx, &sqs.GetQueueAttributesInput{
		QueueUrl:       aws.String(url),
		AttributeNames: []types.QueueAttributeName{types.QueueAttributeNameApproximateNumberOfMessages},
	})
	return err
}
