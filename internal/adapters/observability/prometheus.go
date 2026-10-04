// Package observability implementa as métricas Prometheus e o log com
// contexto. Os casos de uso e os outros adaptadores falam com portas
// (app.Metrics, sqsconsumer.Metrics, httpapi.RequestObserver); só este pacote
// conhece a biblioteca do Prometheus.
package observability

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/Taino-Edu/Desafio-go-arqui/internal/app"
)

// latencyBuckets vão de 1ms a 10s: operações saudáveis ficam nas primeiras
// faixas; espera por lock e novas tentativas aparecem nas últimas.
var latencyBuckets = []float64{.001, .0025, .005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10}

// Prometheus guarda as métricas num registry PRÓPRIO (nada global): cada
// instância da aplicação, inclusive as dos testes, tem as suas.
type Prometheus struct {
	reg *prometheus.Registry

	wagerOutcomes   *prometheus.CounterVec
	replays         *prometheus.CounterVec
	idemConflicts   *prometheus.CounterVec
	processing      *prometheus.HistogramVec
	retries         *prometheus.CounterVec
	lockConflicts   *prometheus.CounterVec
	references      *prometheus.CounterVec
	outbox          *prometheus.CounterVec
	reconciliations *prometheus.CounterVec
	mismatches      prometheus.Counter
	queueMessages   *prometheus.CounterVec
	httpRequests    *prometheus.CounterVec
	httpDuration    *prometheus.HistogramVec
}

var _ app.Metrics = (*Prometheus)(nil)

// NewPrometheus cria o registry com as métricas da aplicação e as do runtime
// Go e do processo (memória, goroutines, CPU, descritores abertos).
func NewPrometheus() *Prometheus {
	counter := func(name, help string, labels ...string) *prometheus.CounterVec {
		return prometheus.NewCounterVec(prometheus.CounterOpts{Name: name, Help: help}, labels)
	}
	histogram := func(name, help string, labels ...string) *prometheus.HistogramVec {
		return prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: name, Help: help, Buckets: latencyBuckets}, labels)
	}
	p := &Prometheus{
		reg: prometheus.NewRegistry(),
		wagerOutcomes: counter("wager_transactions_total",
			"New wager transactions with a recorded outcome, by entry point, kind and status.", "source", "kind", "status"),
		replays: counter("idempotent_replays_total",
			"Repeated requests answered with the stored result.", "source"),
		idemConflicts: counter("idempotency_conflicts_total",
			"Requests refused because the key or the external transaction id was already used with other content.", "source", "reason"),
		processing: histogram("wager_processing_duration_seconds",
			"Use case latency, including lock waits and automatic retries.", "source"),
		retries: counter("transient_retries_total",
			"Automatic retries after a transient failure.", "source"),
		lockConflicts: counter("wallet_lock_conflicts_total",
			"Write contention detected by the database (lock timeout, deadlock, serialization failure, stale version).", "source"),
		references: counter("reference_resolutions_total",
			"Rounds of the pending reference worker, by outcome.", "outcome"),
		outbox: counter("outbox_events_total",
			"Outbox publication attempts, by result.", "result"),
		reconciliations: counter("reconciliations_total",
			"Wallet reconciliations, by result.", "result"),
		mismatches: prometheus.NewCounter(prometheus.CounterOpts{Name: "reconciliation_mismatch_total",
			Help: "Reconciliations whose stored balance differs from the ledger."}),
		queueMessages: counter("sqs_messages_total",
			"Messages received from the input queue, by outcome.", "outcome"),
		httpRequests: counter("http_requests_total",
			"HTTP requests, by method, route pattern and status code.", "method", "route", "status"),
		httpDuration: histogram("http_request_duration_seconds",
			"HTTP request latency, by method and route pattern.", "method", "route"),
	}
	p.reg.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		p.wagerOutcomes, p.replays, p.idemConflicts, p.processing, p.retries, p.lockConflicts,
		p.references, p.outbox, p.reconciliations, p.mismatches, p.queueMessages,
		p.httpRequests, p.httpDuration,
	)
	// séries de rótulos fixos começam em zero: rate() e alertas funcionam
	// desde a partida, antes do primeiro evento
	for _, r := range []string{"published", "failed"} {
		p.outbox.WithLabelValues(r)
	}
	for _, r := range []string{"consistent", "divergent"} {
		p.reconciliations.WithLabelValues(r)
	}
	for _, o := range []app.ReferenceOutcome{app.ReferenceResolved, app.ReferenceRescheduled, app.ReferenceExpired} {
		p.references.WithLabelValues(string(o))
	}
	return p
}

// --- app.Metrics ---

func (p *Prometheus) WagerOutcome(source, kind, status string) {
	p.wagerOutcomes.WithLabelValues(source, kind, status).Inc()
}

func (p *Prometheus) IdempotentReplay(source string) { p.replays.WithLabelValues(source).Inc() }

func (p *Prometheus) IdempotencyConflict(source, reason string) {
	p.idemConflicts.WithLabelValues(source, reason).Inc()
}

func (p *Prometheus) ProcessingDuration(source string, d time.Duration) {
	p.processing.WithLabelValues(source).Observe(d.Seconds())
}

func (p *Prometheus) TransientRetry(source string) { p.retries.WithLabelValues(source).Inc() }

func (p *Prometheus) ConcurrencyConflict(source string) {
	p.lockConflicts.WithLabelValues(source).Inc()
}

func (p *Prometheus) ReferenceResolution(outcome string) { p.references.WithLabelValues(outcome).Inc() }

func (p *Prometheus) OutboxPublished(n int) { p.outbox.WithLabelValues("published").Add(float64(n)) }

func (p *Prometheus) OutboxFailed(n int) { p.outbox.WithLabelValues("failed").Add(float64(n)) }

func (p *Prometheus) Reconciliation(consistent bool) {
	if consistent {
		p.reconciliations.WithLabelValues("consistent").Inc()
		return
	}
	p.reconciliations.WithLabelValues("divergent").Inc()
	p.mismatches.Inc()
}

// --- sqsconsumer.Metrics ---

func (p *Prometheus) QueueMessage(outcome string) { p.queueMessages.WithLabelValues(outcome).Inc() }

// --- httpapi.RequestObserver ---

// ObserveRequest registra uma requisição. route já vem como padrão da rota
// (ex.: "/wallets/{walletId}"); o método é normalizado para que um cliente
// não crie séries novas inventando métodos.
func (p *Prometheus) ObserveRequest(method, route string, status int, d time.Duration) {
	method = normalizeMethod(method)
	p.httpRequests.WithLabelValues(method, route, strconv.Itoa(status)).Inc()
	p.httpDuration.WithLabelValues(method, route).Observe(d.Seconds())
}

func normalizeMethod(m string) string {
	switch m {
	case http.MethodGet, http.MethodPost, http.MethodPut, http.MethodPatch,
		http.MethodDelete, http.MethodHead, http.MethodOptions:
		return m
	}
	return "OTHER"
}

// Handler serve as métricas no formato de exposição do Prometheus. Uma
// falha de coleta (banco fora, por exemplo) não derruba a página: as demais
// métricas continuam sendo servidas e o erro vai para o log. No máximo duas
// coletas simultâneas, porque cada uma consulta o banco.
func (p *Prometheus) Handler(log *slog.Logger) http.Handler {
	return promhttp.HandlerFor(p.reg, promhttp.HandlerOpts{
		ErrorLog:            promLogger{log},
		ErrorHandling:       promhttp.ContinueOnError,
		Registry:            p.reg, // promhttp_metric_handler_errors_total
		MaxRequestsInFlight: 2,
	})
}

type promLogger struct{ log *slog.Logger }

func (l promLogger) Println(v ...any) {
	l.log.Warn("metrics collection error", "error", fmt.Sprint(v...))
}

// --- coletores lidos na hora da coleta ---

// RegisterBacklog registra os medidores do trabalho assíncrono acumulado:
// eventos da outbox ainda não publicados, idade do mais antigo (o atraso da
// outbox) e operações aguardando referência. read roda a cada coleta, com
// prazo timeout; assim o valor é sempre o do banco, visto por qualquer
// instância, e não um contador local que cada instância teria diferente.
func (p *Prometheus) RegisterBacklog(read func(context.Context) (app.Backlog, error), timeout time.Duration) error {
	return p.reg.Register(&backlogCollector{
		read: read, timeout: timeout,
		pending: prometheus.NewDesc("outbox_pending_events",
			"Outbox events not yet published.", nil, nil),
		lag: prometheus.NewDesc("outbox_lag_seconds",
			"Age of the oldest unpublished outbox event (0 when there is none).", nil, nil),
		references: prometheus.NewDesc("wager_pending_references",
			"Wager transactions waiting for their reference (PENDING_REFERENCE).", nil, nil),
	})
}

type backlogCollector struct {
	read                     func(context.Context) (app.Backlog, error)
	timeout                  time.Duration
	pending, lag, references *prometheus.Desc
}

func (c *backlogCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.pending
	ch <- c.lag
	ch <- c.references
}

func (c *backlogCollector) Collect(ch chan<- prometheus.Metric) {
	ctx, cancel := context.WithTimeout(context.Background(), c.timeout)
	defer cancel()
	b, err := c.read(ctx)
	if err != nil {
		ch <- prometheus.NewInvalidMetric(c.pending, fmt.Errorf("read backlog: %w", err))
		return
	}
	lag := 0.0
	if b.OutboxOldestPending != nil {
		lag = max(time.Since(*b.OutboxOldestPending).Seconds(), 0)
	}
	ch <- prometheus.MustNewConstMetric(c.pending, prometheus.GaugeValue, float64(b.OutboxPending))
	ch <- prometheus.MustNewConstMetric(c.lag, prometheus.GaugeValue, lag)
	ch <- prometheus.MustNewConstMetric(c.references, prometheus.GaugeValue, float64(b.PendingReferences))
}

// RegisterQueueDepth registra o medidor de mensagens visíveis nas filas
// (rótulo queue: "input", "dlq"). Inclui o que o próprio SQS move para a DLQ
// pela redrive policy, que o consumidor não vê.
func (p *Prometheus) RegisterQueueDepth(read func(context.Context) (map[string]int64, error), timeout time.Duration) error {
	return p.reg.Register(&queueDepthCollector{
		read: read, timeout: timeout,
		desc: prometheus.NewDesc("sqs_queue_messages",
			"Approximate number of visible messages in each queue.", []string{"queue"}, nil),
	})
}

type queueDepthCollector struct {
	read    func(context.Context) (map[string]int64, error)
	timeout time.Duration
	desc    *prometheus.Desc
}

func (c *queueDepthCollector) Describe(ch chan<- *prometheus.Desc) { ch <- c.desc }

func (c *queueDepthCollector) Collect(ch chan<- prometheus.Metric) {
	ctx, cancel := context.WithTimeout(context.Background(), c.timeout)
	defer cancel()
	depths, err := c.read(ctx)
	if err != nil {
		ch <- prometheus.NewInvalidMetric(c.desc, fmt.Errorf("read queue depth: %w", err))
		return
	}
	for queue, n := range depths {
		ch <- prometheus.MustNewConstMetric(c.desc, prometheus.GaugeValue, float64(n), queue)
	}
}
