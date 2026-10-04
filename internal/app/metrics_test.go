package app

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Taino-Edu/Desafio-go-arqui/internal/domain/money"
	"github.com/Taino-Edu/Desafio-go-arqui/internal/domain/wagering"
	"github.com/Taino-Edu/Desafio-go-arqui/internal/domain/wallet"
)

// recordingMetrics conta as chamadas, para conferir o que cada caminho mede.
type recordingMetrics struct {
	NopMetrics
	mu    sync.Mutex
	calls map[string]int
}

func (m *recordingMetrics) add(key string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.calls == nil {
		m.calls = map[string]int{}
	}
	m.calls[key]++
}

func (m *recordingMetrics) TransientRetry(source string)      { m.add("retry:" + source) }
func (m *recordingMetrics) ConcurrencyConflict(source string) { m.add("conflict:" + source) }
func (m *recordingMetrics) Reconciliation(consistent bool) {
	m.add(fmt.Sprint("reconciliation:", consistent))
}

func (m *recordingMetrics) count(key string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.calls[key]
}

func TestRetryTransient_CountsRetriesAndConflicts(t *testing.T) {
	conflict := fmt.Errorf("%w: %w: lock timeout", ErrTransient, ErrConcurrencyConflict)
	dbDown := fmt.Errorf("%w: connection refused", ErrTransient)

	tests := map[string]struct {
		failures                   []error // erro de cada tentativa; depois disso, sucesso
		wantErr                    bool
		wantRetries, wantConflicts int
	}{
		"sucesso de primeira":        {nil, false, 0, 0},
		"conflito e depois sucesso":  {[]error{conflict}, false, 1, 1},
		"três conflitos: desiste":    {[]error{conflict, conflict, conflict}, true, 2, 3},
		"banco fora não é conflito":  {[]error{dbDown, dbDown}, false, 2, 0},
		"erro permanente não repete": {[]error{ErrIdempotencyKeyReused}, true, 0, 0},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			m := &recordingMetrics{}
			s := NewWagerService(nil, nil, nil, wagering.ReferenceRetryPolicy{}, WithMetrics(m))
			attempt := 0
			err := s.retryTransient(context.Background(), SourceHTTP, func() error {
				attempt++
				if attempt <= len(tt.failures) {
					return tt.failures[attempt-1]
				}
				return nil
			})
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if got := m.count("retry:http"); got != tt.wantRetries {
				t.Errorf("retries = %d, want %d", got, tt.wantRetries)
			}
			if got := m.count("conflict:http"); got != tt.wantConflicts {
				t.Errorf("conflicts = %d, want %d", got, tt.wantConflicts)
			}
		})
	}
}

func TestWithMetrics_NilKeepsNop(t *testing.T) {
	if _, ok := applyOptions([]Option{WithMetrics(nil)}).metrics.(NopMetrics); !ok {
		t.Error("WithMetrics(nil) deveria manter NopMetrics")
	}
}

func TestCompare(t *testing.T) {
	brl := func(s string) money.Money {
		m, err := money.Parse(s, money.BRL)
		if err != nil {
			t.Fatal(err)
		}
		return m
	}
	walletWith := func(balance money.Money) *wallet.Wallet {
		w, err := wallet.Rehydrate(wallet.RehydrateParams{
			ID: uuid.New(), PlayerID: uuid.New(), Balance: balance, Version: 2,
			CreatedAt: time.Now(), UpdatedAt: time.Now(),
		})
		if err != nil {
			t.Fatal(err)
		}
		return w
	}
	// o exemplo do enunciado: abertura 1000.00 e aposta 25.00
	totals := LedgerTotals{Credits: 100000, Debits: 2500, Entries: 2}

	tests := map[string]struct {
		stored         money.Money
		wantDiff       string
		wantConsistent bool
	}{
		"bate com o ledger":          {brl("975.00"), "0.00", true},
		"saldo a mais que o ledger":  {brl("985.00"), "10.00", false},
		"saldo a menos que o ledger": {brl("965.00"), "-10.00", false},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			rec, err := compare(walletWith(tt.stored), totals)
			if err != nil {
				t.Fatal(err)
			}
			if rec.CalculatedBalance.Amount() != "975.00" || rec.Difference.Amount() != tt.wantDiff ||
				rec.Consistent != tt.wantConsistent || rec.CheckedEntries != 2 || !rec.StoredBalance.Equal(tt.stored) {
				t.Errorf("rec = %+v (calc %s, diff %s)", rec, rec.CalculatedBalance, rec.Difference)
			}
		})
	}
}

func TestCompare_OverflowIsAnErrorNotAWrongNumber(t *testing.T) {
	w, err := wallet.Rehydrate(wallet.RehydrateParams{
		ID: uuid.New(), PlayerID: uuid.New(), Balance: mustMinor(t, 1), Version: 2,
		CreatedAt: time.Now(), UpdatedAt: time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	// ledger absurdo: reconstruído = −MaxInt64; armazenado − reconstruído
	// = 1 + MaxInt64, que não cabe em int64
	_, err = compare(w, LedgerTotals{Credits: 0, Debits: math.MaxInt64})
	if !errors.Is(err, money.ErrOverflow) {
		t.Errorf("err = %v, want ErrOverflow", err)
	}
}

func mustMinor(t *testing.T, minor int64) money.Money {
	m, err := money.FromMinorUnits(minor, money.BRL)
	if err != nil {
		t.Fatal(err)
	}
	return m
}
