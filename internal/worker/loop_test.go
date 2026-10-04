package worker

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

func TestLoop_DrainsWorkThenIdles(t *testing.T) {
	var calls atomic.Int32
	pending := atomic.Int32{}
	pending.Store(5)
	l := New(Config{Name: "t", Interval: time.Hour, ItemTimeout: time.Second}, quiet, func(context.Context) (bool, error) {
		calls.Add(1)
		return pending.Add(-1) >= 0, nil
	})
	_ = l.Start(context.Background())
	time.Sleep(100 * time.Millisecond)
	// 5 itens + 1 chamada vazia; depois espera 1h (não chama mais)
	if got := calls.Load(); got != 6 {
		t.Errorf("chamadas = %d, want 6", got)
	}
	if err := l.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-l.Done():
	default:
		t.Error("Done deveria estar fechado")
	}
}

// O item em andamento termina antes de o loop parar.
func TestLoop_StopWaitsForInFlightItem(t *testing.T) {
	started := make(chan struct{})
	var finished atomic.Bool
	l := New(Config{Name: "t", Interval: time.Hour, ItemTimeout: 5 * time.Second}, quiet, func(ctx context.Context) (bool, error) {
		close(started)
		select {
		case <-time.After(200 * time.Millisecond):
			finished.Store(true)
		case <-ctx.Done():
		}
		return false, nil
	})
	_ = l.Start(context.Background())
	<-started
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := l.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	if !finished.Load() {
		t.Error("o item em andamento deveria ter terminado")
	}
}

// Prazo de parada esgotado: o item é cancelado e o loop sai mesmo assim.
func TestLoop_StopDeadlineCancelsItem(t *testing.T) {
	started := make(chan struct{})
	var cancelled atomic.Bool
	l := New(Config{Name: "t", Interval: time.Hour, ItemTimeout: time.Minute}, quiet, func(ctx context.Context) (bool, error) {
		close(started)
		<-ctx.Done()
		cancelled.Store(true)
		return false, ctx.Err()
	})
	_ = l.Start(context.Background())
	<-started
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := l.Stop(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v", err)
	}
	if !cancelled.Load() {
		t.Error("o item deveria ter sido cancelado")
	}
	<-l.Done()
}

func TestLoop_BacksOffOnErrors(t *testing.T) {
	var calls atomic.Int32
	l := New(Config{Name: "t", Interval: 20 * time.Millisecond, ItemTimeout: time.Second, MaxBackoff: time.Second},
		quiet, func(context.Context) (bool, error) {
			calls.Add(1)
			return false, errors.New("banco fora")
		})
	_ = l.Start(context.Background())
	time.Sleep(300 * time.Millisecond)
	_ = l.Stop(context.Background())
	// 20, 40, 80, 160ms... em 300ms cabem poucas tentativas, não dezenas
	if got := calls.Load(); got < 2 || got > 6 {
		t.Errorf("chamadas com erro = %d (backoff não aplicado?)", got)
	}
}

func TestLoop_StopWithoutStart(t *testing.T) {
	l := New(Config{Name: "t", Interval: time.Second, ItemTimeout: time.Second}, quiet, func(context.Context) (bool, error) {
		return false, nil
	})
	if err := l.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestBackoff(t *testing.T) {
	for failures, max := range map[int]time.Duration{1: 120, 2: 240, 3: 480, 10: 1200} {
		d := backoff(100*time.Millisecond, failures, time.Second)
		if d > max*time.Millisecond {
			t.Errorf("backoff(%d) = %v > %v", failures, d, max*time.Millisecond)
		}
	}
}
