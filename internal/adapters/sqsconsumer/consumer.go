// Package sqsconsumer consome operações da fila SQS FIFO de entrada.
//
// Garantias:
//   - a mensagem só é apagada DEPOIS do commit do seu tratamento durável
//     (inbox + domínio + ledger + outbox na mesma transação SQL);
//   - reentregas (at-least-once) são reconhecidas pela inbox
//     (consumidor, messageId) e pela idempotência (provedor, chave);
//   - rejeição de negócio confirmada é desfecho final: a mensagem é apagada;
//   - erro permanente (mensagem inválida, conflito, provedor não permitido)
//     vai para a DLQ na hora, com o motivo;
//   - erro transitório: a mensagem volta para a fila com backoff exponencial
//     (ChangeMessageVisibility); depois de maxReceiveCount recebimentos o
//     próprio SQS a move para a DLQ (redrive policy);
//   - parada: para de buscar mensagens, conclui as que estão em andamento
//     dentro do prazo e libera (visibilidade 0) as que não começaram.
package sqsconsumer

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"

	"github.com/Taino-Edu/Desafio-go-arqui/internal/app"
)

// API é o subconjunto do cliente SQS usado pelo consumidor.
type API interface {
	ReceiveMessage(ctx context.Context, in *sqs.ReceiveMessageInput, opts ...func(*sqs.Options)) (*sqs.ReceiveMessageOutput, error)
	DeleteMessage(ctx context.Context, in *sqs.DeleteMessageInput, opts ...func(*sqs.Options)) (*sqs.DeleteMessageOutput, error)
	ChangeMessageVisibility(ctx context.Context, in *sqs.ChangeMessageVisibilityInput, opts ...func(*sqs.Options)) (*sqs.ChangeMessageVisibilityOutput, error)
	SendMessage(ctx context.Context, in *sqs.SendMessageInput, opts ...func(*sqs.Options)) (*sqs.SendMessageOutput, error)
}

// Handler é o caso de uso (app.WagerService.HandleQueueMessage).
type Handler interface {
	HandleQueueMessage(ctx context.Context, m app.QueueMessage) (app.QueueOutcome, error)
}

// Config parametriza o consumidor.
type Config struct {
	ConsumerName     string // escopo da inbox
	QueueURL         string
	DLQURL           string
	Pollers          int           // goroutines recebendo em paralelo
	MaxMessages      int32         // por ReceiveMessage (1..10)
	WaitTime         time.Duration // long polling (até 20s)
	ItemTimeout      time.Duration // prazo do tratamento de uma mensagem
	RetryBaseDelay   time.Duration // backoff da visibilidade em erro transitório
	RetryMaxDelay    time.Duration
	AllowedProviders map[string]bool // vazio = qualquer provedor
	Metrics          Metrics         // nil = sem métricas
}

// Metrics recebe o desfecho de cada mensagem recebida.
type Metrics interface {
	QueueMessage(outcome string)
}

// Desfechos de uma mensagem (rótulo "outcome").
const (
	OutcomeProcessed = "processed" // tratada e apagada
	OutcomeDuplicate = "duplicate" // reentrega reconhecida pela inbox e apagada
	OutcomeRetry     = "retry"     // falha transitória: volta para a fila com atraso
	OutcomeDLQ       = "dlq"       // erro permanente: enviada à DLQ pelo consumidor
	OutcomeReleased  = "released"  // não iniciada no desligamento: devolvida já
)

type nopMetrics struct{}

func (nopMetrics) QueueMessage(string) {}

// Hooks permitem aos testes simular falhas em pontos exatos. Em produção
// são nil.
type Hooks struct {
	// BeforeHandle roda antes do caso de uso; erro = falha transitória.
	BeforeHandle func(ctx context.Context, messageID string) error
	// AfterCommit roda depois do commit e antes de apagar a mensagem. Se
	// devolver ErrSimulatedCrash, o consumidor "morre": não apaga nem altera
	// a visibilidade, exatamente como um processo derrubado nesse instante.
	AfterCommit func(messageID string) error
}

// ErrSimulatedCrash é usado pelos testes de queda.
var ErrSimulatedCrash = errors.New("simulated crash")

// Consumer recebe e trata mensagens.
type Consumer struct {
	cfg     Config
	api     API
	handler Handler
	log     *slog.Logger
	hooks   Hooks

	stop       chan struct{}
	stopOnce   sync.Once
	pollCancel context.CancelFunc // interrompe long polls em andamento
	workCancel context.CancelFunc // interrompe tratamentos (só no prazo esgotado)
	wg         sync.WaitGroup
	started    bool
}

func New(cfg Config, api API, handler Handler, log *slog.Logger, hooks Hooks) *Consumer {
	if cfg.Pollers < 1 {
		cfg.Pollers = 1
	}
	if cfg.MaxMessages < 1 || cfg.MaxMessages > 10 {
		cfg.MaxMessages = 10
	}
	if cfg.Metrics == nil {
		cfg.Metrics = nopMetrics{}
	}
	return &Consumer{cfg: cfg, api: api, handler: handler, hooks: hooks,
		log: log.With("component", "sqs-consumer", "queue", cfg.QueueURL), stop: make(chan struct{})}
}

// Start inicia os pollers. Não bloqueia.
func (c *Consumer) Start(context.Context) error {
	if c.started {
		return nil
	}
	c.started = true
	pollCtx, pollCancel := context.WithCancel(context.Background())
	workCtx, workCancel := context.WithCancel(context.Background())
	c.pollCancel, c.workCancel = pollCancel, workCancel
	for i := 0; i < c.cfg.Pollers; i++ {
		c.wg.Add(1)
		go c.poll(pollCtx, workCtx)
	}
	c.log.Info("sqs consumer started", "pollers", c.cfg.Pollers)
	return nil
}

// Stop para de receber, espera as mensagens em andamento e, se o prazo
// acabar, cancela os tratamentos (as transações são desfeitas e as mensagens
// liberadas para reentrega).
func (c *Consumer) Stop(ctx context.Context) error {
	if !c.started {
		return nil
	}
	c.stopOnce.Do(func() {
		close(c.stop)
		c.pollCancel()
	})
	done := make(chan struct{})
	go func() { c.wg.Wait(); close(done) }()
	select {
	case <-done:
		c.log.Info("sqs consumer stopped")
		return nil
	case <-ctx.Done():
		c.workCancel()
		<-done
		c.log.Warn("sqs consumer stopped after deadline; in-flight messages released")
		return ctx.Err()
	}
}

func (c *Consumer) stopping() bool {
	select {
	case <-c.stop:
		return true
	default:
		return false
	}
}

func (c *Consumer) poll(pollCtx, workCtx context.Context) {
	defer c.wg.Done()
	failures := 0
	for !c.stopping() {
		out, err := c.api.ReceiveMessage(pollCtx, &sqs.ReceiveMessageInput{
			QueueUrl:            aws.String(c.cfg.QueueURL),
			MaxNumberOfMessages: c.cfg.MaxMessages,
			WaitTimeSeconds:     int32(c.cfg.WaitTime / time.Second),
			MessageSystemAttributeNames: []types.MessageSystemAttributeName{
				types.MessageSystemAttributeNameApproximateReceiveCount,
				types.MessageSystemAttributeNameMessageGroupId,
			},
		})
		if err != nil {
			if c.stopping() {
				return
			}
			failures++
			wait := backoff(c.cfg.RetryBaseDelay, failures, 30*time.Second)
			c.log.Warn("sqs receive failed", "error", err, "retryIn", wait.String())
			select {
			case <-c.stop:
				return
			case <-time.After(wait):
			}
			continue
		}
		failures = 0
		for i, m := range out.Messages {
			if c.stopping() {
				// não começou: devolve já para outra instância pegar
				c.release(out.Messages[i:])
				return
			}
			c.handle(workCtx, m)
		}
	}
}

func (c *Consumer) handle(workCtx context.Context, m types.Message) {
	ctx, cancel := context.WithTimeout(workCtx, c.cfg.ItemTimeout)
	defer cancel()
	sqsID := aws.ToString(m.MessageId)
	log := c.log.With("sqsMessageId", sqsID, "receiveCount", receiveCount(m))

	qm, err := decode(aws.ToString(m.Body), c.cfg.ConsumerName)
	if err != nil {
		c.toDLQ(m, err, log)
		return
	}
	log = log.With("messageId", qm.MessageID, "providerId", qm.Input.ProviderID, "walletId", qm.Input.WalletID)
	if len(c.cfg.AllowedProviders) > 0 && !c.cfg.AllowedProviders[qm.Input.ProviderID] {
		c.toDLQ(m, fmt.Errorf("%w: provider %q is not allowed on this queue", app.ErrForbidden, qm.Input.ProviderID), log)
		return
	}

	if c.hooks.BeforeHandle != nil {
		if err := c.hooks.BeforeHandle(ctx, qm.MessageID); err != nil {
			c.retryLater(m, err, log)
			return
		}
	}
	out, err := c.handler.HandleQueueMessage(app.WithCorrelationID(ctx, qm.MessageID), qm)
	switch {
	case err == nil:
	case app.IsPermanentQueueError(err):
		c.toDLQ(m, err, log)
		return
	default: // transitório ou desconhecido: tenta de novo; o redrive leva à DLQ
		c.retryLater(m, err, log)
		return
	}

	if c.hooks.AfterCommit != nil && errors.Is(c.hooks.AfterCommit(qm.MessageID), ErrSimulatedCrash) {
		log.Warn("simulated crash after commit: message not deleted")
		return
	}
	c.delete(m, log)
	if out.Duplicate {
		c.cfg.Metrics.QueueMessage(OutcomeDuplicate)
		log.Info("duplicate message acknowledged (inbox)")
		return
	}
	c.cfg.Metrics.QueueMessage(OutcomeProcessed)
	tx := out.Result.Transaction
	log.Info("queue message processed", "transactionId", tx.ID(), "status", tx.Status(),
		"failureCode", tx.FailureCode(), "idempotentReplay", out.Result.Replay)
}

// as operações de limpeza usam um contexto próprio: a mensagem precisa ser
// apagada ou liberada mesmo que o tratamento tenha sido cancelado
func shortCtx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 5*time.Second)
}

func (c *Consumer) delete(m types.Message, log *slog.Logger) {
	ctx, cancel := shortCtx()
	defer cancel()
	if _, err := c.api.DeleteMessage(ctx, &sqs.DeleteMessageInput{
		QueueUrl: aws.String(c.cfg.QueueURL), ReceiptHandle: m.ReceiptHandle,
	}); err != nil {
		// o tratamento já está confirmado; a reentrega cairá na inbox
		log.Warn("delete failed; redelivery will be deduplicated by the inbox", "error", err)
	}
}

// retryLater devolve a mensagem para a fila com atraso crescente.
func (c *Consumer) retryLater(m types.Message, cause error, log *slog.Logger) {
	delay := backoff(c.cfg.RetryBaseDelay, receiveCount(m), c.cfg.RetryMaxDelay)
	if c.stopping() {
		delay = 0 // desligando: libera já para outra instância
	}
	c.cfg.Metrics.QueueMessage(OutcomeRetry)
	log.Warn("transient failure; message will be redelivered", "error", cause, "retryIn", delay.String())
	ctx, cancel := shortCtx()
	defer cancel()
	if _, err := c.api.ChangeMessageVisibility(ctx, &sqs.ChangeMessageVisibilityInput{
		QueueUrl: aws.String(c.cfg.QueueURL), ReceiptHandle: m.ReceiptHandle,
		VisibilityTimeout: int32(delay / time.Second),
	}); err != nil {
		log.Warn("change visibility failed; message reappears after the queue visibility timeout", "error", err)
	}
}

// release devolve mensagens não iniciadas (visibilidade 0).
func (c *Consumer) release(ms []types.Message) {
	ctx, cancel := shortCtx()
	defer cancel()
	for _, m := range ms {
		_, _ = c.api.ChangeMessageVisibility(ctx, &sqs.ChangeMessageVisibilityInput{
			QueueUrl: aws.String(c.cfg.QueueURL), ReceiptHandle: m.ReceiptHandle, VisibilityTimeout: 0,
		})
		c.cfg.Metrics.QueueMessage(OutcomeReleased)
	}
	if len(ms) > 0 {
		c.log.Info("released unstarted messages on shutdown", "count", len(ms))
	}
}

// toDLQ envia a mensagem original para a DLQ com o motivo e a remove da fila.
func (c *Consumer) toDLQ(m types.Message, cause error, log *slog.Logger) {
	group := m.Attributes[string(types.MessageSystemAttributeNameMessageGroupId)]
	if group == "" {
		group = "invalid"
	}
	reason := cause.Error()
	if len(reason) > 256 {
		reason = reason[:256]
	}
	ctx, cancel := shortCtx()
	defer cancel()
	_, err := c.api.SendMessage(ctx, &sqs.SendMessageInput{
		QueueUrl:               aws.String(c.cfg.DLQURL),
		MessageBody:            m.Body,
		MessageGroupId:         aws.String(group),
		MessageDeduplicationId: m.MessageId, // reenvio para a DLQ também é idempotente
		MessageAttributes: map[string]types.MessageAttributeValue{
			"failure-reason": {DataType: aws.String("String"), StringValue: aws.String(reason)},
			"source-queue":   {DataType: aws.String("String"), StringValue: aws.String(c.cfg.QueueURL)},
		},
	})
	if err != nil {
		// sem DLQ disponível: não apaga; volta para a fila e o redrive resolve
		c.retryLater(m, fmt.Errorf("dlq send failed: %w (original: %v)", err, cause), log)
		return
	}
	c.cfg.Metrics.QueueMessage(OutcomeDLQ)
	log.Warn("message sent to DLQ", "reason", reason)
	c.delete(m, log)
}

func receiveCount(m types.Message) int {
	n, _ := strconv.Atoi(m.Attributes[string(types.MessageSystemAttributeNameApproximateReceiveCount)])
	if n < 1 {
		n = 1
	}
	return n
}

// backoff: base * 2^(n-1), limitado a max. O SQS trabalha em segundos.
func backoff(base time.Duration, n int, max time.Duration) time.Duration {
	if base <= 0 {
		base = time.Second
	}
	d := base
	for i := 1; i < n && d < max; i++ {
		d *= 2
	}
	if d > max {
		d = max
	}
	return d
}
