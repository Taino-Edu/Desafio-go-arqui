package app

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/Taino-Edu/Desafio-go-arqui/internal/domain/money"
	"github.com/Taino-Edu/Desafio-go-arqui/internal/domain/wallet"
)

// Reconciliation é o resultado da conferência saldo × ledger.
type Reconciliation struct {
	WalletID          uuid.UUID
	StoredBalance     money.Money // saldo gravado na carteira
	CalculatedBalance money.Money // Σ créditos − Σ débitos do ledger
	Difference        money.Money // armazenado − reconstruído (pode ser negativo)
	Consistent        bool
	CheckedEntries    int64
}

// Reconcile reconstrói o saldo a partir do ledger, abertura incluída
// (Σ créditos − Σ débitos), e compara com o saldo armazenado.
//
// As duas leituras acontecem na MESMA foto do banco (ReadSnapshot): uma
// aposta confirmada durante a conferência não aparece numa leitura e falta
// na outra, então a carga normal não gera falso alarme.
//
// Nada é alterado. A reconciliação só observa: a divergência vira resposta,
// métrica e log, e a correção é decisão humana, feita com um lançamento novo
// (o ledger nunca é editado).
func (s *WalletService) Reconcile(ctx context.Context, walletID uuid.UUID) (Reconciliation, error) {
	var (
		w      *wallet.Wallet
		totals LedgerTotals
	)
	err := s.store.ReadSnapshot(ctx, func(ctx context.Context, r Repositories) error {
		var err error
		if w, err = r.Wallets().Get(ctx, walletID); err != nil {
			return err
		}
		totals, err = r.Ledger().Totals(ctx, walletID)
		return err
	})
	if err != nil {
		return Reconciliation{}, err
	}

	rec, err := compare(w, totals)
	if err != nil {
		return Reconciliation{}, fmt.Errorf("reconcile wallet %s: %w", walletID, err)
	}
	s.metrics.Reconciliation(rec.Consistent)
	return rec, nil
}

// compare faz a aritmética da reconciliação com Money: overflow e moeda
// incompatível viram erro, nunca um número errado.
func compare(w *wallet.Wallet, t LedgerTotals) (Reconciliation, error) {
	cur := w.Currency()
	credits, err := money.FromMinorUnits(t.Credits, cur)
	if err != nil {
		return Reconciliation{}, err
	}
	debits, err := money.FromMinorUnits(t.Debits, cur)
	if err != nil {
		return Reconciliation{}, err
	}
	calculated, err := credits.Sub(debits)
	if err != nil {
		return Reconciliation{}, err
	}
	diff, err := w.Balance().Sub(calculated)
	if err != nil {
		return Reconciliation{}, err
	}
	return Reconciliation{
		WalletID: w.ID(), StoredBalance: w.Balance(), CalculatedBalance: calculated,
		Difference: diff, Consistent: diff.IsZero(), CheckedEntries: t.Entries,
	}, nil
}

// Backlog é o trabalho assíncrono acumulado, lido para as métricas.
type Backlog struct {
	OutboxPending       int64
	OutboxOldestPending *time.Time // nil quando não há evento pendente
	PendingReferences   int64
}

// ReadBacklog lê quantos eventos esperam publicação (e desde quando) e
// quantas operações esperam a referência. As duas consultas usam índices
// parciais, então são baratas mesmo com tabelas grandes.
func ReadBacklog(ctx context.Context, store Store) (Backlog, error) {
	r := store.Reader()
	var (
		b   Backlog
		err error
	)
	if b.OutboxPending, b.OutboxOldestPending, err = r.Outbox().Backlog(ctx); err != nil {
		return Backlog{}, err
	}
	if b.PendingReferences, err = r.Transactions().CountPendingReferences(ctx); err != nil {
		return Backlog{}, err
	}
	return b, nil
}
