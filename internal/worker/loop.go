// Package worker executa trabalho em segundo plano com parada controlada.
//
// Um Loop chama Work repetidamente:
//   - se Work fez algo, chama de novo na hora (esvazia a fila);
//   - se não havia nada, espera Interval (com jitter);
//   - se deu erro, espera com backoff exponencial até MaxBackoff, para não
//     martelar um banco fora do ar.
//
// Parada (Stop): o loop para de buscar trabalho novo e deixa o item em
// andamento terminar. Se o prazo de Stop acabar antes, cancela o contexto do
// item (a transação SQL é desfeita e o item volta a ficar disponível) e
// espera a goroutine sair. Done() permite observar o término.
package worker

import (
	"context"
	"log/slog"
	"math/rand/v2"
	"sync"
	"time"
)

// Func executa uma unidade de trabalho. didWork=true indica que havia algo a
// fazer (o loop chama de novo sem esperar).
type Func func(ctx context.Context) (didWork bool, err error)

// Config parametriza um Loop.
type Config struct {
	Name        string
	Interval    time.Duration // espera quando não há trabalho
	ItemTimeout time.Duration // prazo de cada chamada de Work
	MaxBackoff  time.Duration // teto da espera após erros seguidos
}

// Loop é um worker de segundo plano.
type Loop struct {
	cfg  Config
	work Func
	log  *slog.Logger

	startOnce sync.Once
	stopOnce  sync.Once
	stop      chan struct{}
	done      chan struct{}
	cancel    context.CancelFunc
}

func New(cfg Config, log *slog.Logger, work Func) *Loop {
	if cfg.MaxBackoff <= 0 {
		cfg.MaxBackoff = 30 * time.Second
	}
	return &Loop{
		cfg: cfg, work: work, log: log.With("worker", cfg.Name),
		stop: make(chan struct{}), done: make(chan struct{}),
	}
}

// Start inicia o loop em segundo plano. Não bloqueia.
func (l *Loop) Start(context.Context) error {
	l.startOnce.Do(func() {
		// o contexto do trabalho NÃO deriva do contexto de partida do Fx, que
		// expira logo depois da partida; só é cancelado no Stop por prazo
		base, cancel := context.WithCancel(context.Background())
		l.cancel = cancel
		go l.run(base)
		l.log.Info("worker started", "interval", l.cfg.Interval.String())
	})
	return nil
}

// Stop pede a parada e espera o término até o prazo de ctx.
func (l *Loop) Stop(ctx context.Context) error {
	l.stopOnce.Do(func() { close(l.stop) })
	if l.cancel == nil { // nunca iniciado
		return nil
	}
	select {
	case <-l.done:
		l.log.Info("worker stopped")
		return nil
	case <-ctx.Done():
		l.cancel() // interrompe o item em andamento; a transação é desfeita
		<-l.done
		l.log.Warn("worker stopped after deadline; in-flight item was cancelled")
		return ctx.Err()
	}
}

// Done fecha quando a goroutine do loop termina.
func (l *Loop) Done() <-chan struct{} { return l.done }

func (l *Loop) run(base context.Context) {
	defer close(l.done)
	defer l.cancel()
	failures := 0
	for {
		if l.stopping() {
			return
		}
		ctx, cancel := context.WithTimeout(base, l.cfg.ItemTimeout)
		did, err := l.work(ctx)
		cancel()

		var wait time.Duration
		switch {
		case err != nil:
			failures++
			wait = backoff(l.cfg.Interval, failures, l.cfg.MaxBackoff)
			l.log.Warn("worker iteration failed", "error", err, "consecutiveFailures", failures, "retryIn", wait.String())
		case did:
			failures = 0
			continue // há mais trabalho: segue sem esperar
		default:
			failures = 0
			wait = jitter(l.cfg.Interval)
		}
		select {
		case <-l.stop:
			return
		case <-time.After(wait):
		}
	}
}

func (l *Loop) stopping() bool {
	select {
	case <-l.stop:
		return true
	default:
		return false
	}
}

func backoff(base time.Duration, failures int, max time.Duration) time.Duration {
	d := base
	for i := 1; i < failures && d < max; i++ {
		d *= 2
	}
	if d > max {
		d = max
	}
	return jitter(d)
}

// jitter soma até 20% ao intervalo.
func jitter(d time.Duration) time.Duration {
	if d <= 0 {
		return d
	}
	return d + time.Duration(rand.Int64N(int64(d)/5+1))
}
