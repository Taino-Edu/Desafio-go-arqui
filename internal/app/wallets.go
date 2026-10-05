package app

import (
	"context"
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"

	"github.com/google/uuid"

	"github.com/Taino-Edu/Desafio-go-arqui/internal/domain/accounting"
	"github.com/Taino-Edu/Desafio-go-arqui/internal/domain/events"
	"github.com/Taino-Edu/Desafio-go-arqui/internal/domain/money"
	"github.com/Taino-Edu/Desafio-go-arqui/internal/domain/wagering"
	"github.com/Taino-Edu/Desafio-go-arqui/internal/domain/wallet"
)

// Limites da paginação do ledger.
const (
	DefaultLedgerPageSize = 50
	MaxLedgerPageSize     = 200
)

// WalletService reúne os casos de uso de carteira.
type WalletService struct {
	store   Store
	clock   Clock
	ids     IDGenerator
	metrics Metrics
}

func NewWalletService(store Store, clock Clock, ids IDGenerator, opts ...Option) *WalletService {
	o := applyOptions(opts)
	return &WalletService{store: store, clock: clock, ids: ids, metrics: o.metrics}
}

// OpenWalletInput são os dados de abertura.
type OpenWalletInput struct {
	PlayerID       uuid.UUID
	InitialBalance money.Money
}

// OpenWallet abre uma carteira. Com saldo inicial positivo, grava no MESMO
// commit: a carteira, a transação OPENING (PROCESSED), o lançamento de
// crédito, as partidas dobradas (caixa / carteira) e os eventos WagerTransactionProcessed e WalletBalanceChanged na
// outbox. Com saldo zero, grava só a carteira.
func (s *WalletService) OpenWallet(ctx context.Context, in OpenWalletInput) (*wallet.Wallet, error) {
	walletID, err := s.ids.NewID()
	if err != nil {
		return nil, err
	}
	openingID, err := s.ids.NewID()
	if err != nil {
		return nil, err
	}
	entryID, err := s.ids.NewID()
	if err != nil {
		return nil, err
	}
	now := s.clock.Now()

	w, entry, err := wallet.Open(wallet.OpenParams{
		ID: walletID, PlayerID: in.PlayerID, InitialBalance: in.InitialBalance,
		OpeningTransactionID: openingID, LedgerEntryID: entryID, Now: now,
	})
	if err != nil {
		return nil, err
	}

	var opening *wagering.WagerTransaction
	if entry != nil {
		opening, err = wagering.NewOpening(openingID, walletID, in.PlayerID, in.InitialBalance, now)
		if err != nil {
			return nil, err
		}
	}

	// partidas dobradas: D cash / C wallet (o dinheiro entrou na plataforma)
	var journal accounting.JournalEntry
	if opening != nil {
		if journal, err = accounting.ForTransaction(opening, *entry); err != nil {
			return nil, err
		}
	}

	// eventos na ordem em que aconteceram: a operação, depois o saldo
	var evs []events.Event
	if opening != nil {
		evs = append(evs, opening.PullEvents()...)
	}
	evs = append(evs, w.PullEvents()...)
	var causation *uuid.UUID
	if opening != nil {
		causation = &openingID
	}
	records, err := toOutboxRecords(ctx, s.ids, causation, evs...)
	if err != nil {
		return nil, err
	}

	err = s.store.WithinTx(ctx, func(ctx context.Context, r Repositories) error {
		if err := r.Wallets().Insert(ctx, w); err != nil {
			return err
		}
		if opening == nil {
			return nil
		}
		if err := r.Transactions().Insert(ctx, opening); err != nil {
			return err
		}
		if err := r.Ledger().Insert(ctx, *entry); err != nil {
			return err
		}
		if err := r.Journal().Post(ctx, journal); err != nil {
			return err
		}
		return r.Outbox().Append(ctx, records...)
	})
	if err != nil {
		return nil, err
	}
	return w, nil
}

// GetWallet devolve a carteira ou ErrWalletNotFound.
func (s *WalletService) GetWallet(ctx context.Context, id uuid.UUID) (*wallet.Wallet, error) {
	return s.store.Reader().Wallets().Get(ctx, id)
}

// LedgerPage é uma página do ledger.
type LedgerPage struct {
	Entries    []wallet.LedgerEntry
	NextCursor string // vazio quando não há mais páginas
}

// ListLedger pagina o ledger em ordem crescente de versão da carteira.
//
// O cursor é opaco para o cliente (base64 de "v1:<última versão vista>"), o
// que dá uma ordenação estável: lançamentos novos sempre entram depois.
func (s *WalletService) ListLedger(ctx context.Context, walletID uuid.UUID, cursor string, limit int) (LedgerPage, error) {
	if limit <= 0 {
		limit = DefaultLedgerPageSize
	}
	if limit > MaxLedgerPageSize {
		limit = MaxLedgerPageSize
	}
	after, err := decodeCursor(cursor)
	if err != nil {
		return LedgerPage{}, err
	}

	repos := s.store.Reader()
	if _, err := repos.Wallets().Get(ctx, walletID); err != nil {
		return LedgerPage{}, err
	}
	// pede um a mais para saber se existe próxima página
	entries, err := repos.Ledger().List(ctx, walletID, after, limit+1)
	if err != nil {
		return LedgerPage{}, err
	}
	page := LedgerPage{Entries: entries}
	if len(entries) > limit {
		page.Entries = entries[:limit]
		page.NextCursor = encodeCursor(page.Entries[limit-1].WalletVersion())
	}
	return page, nil
}

const cursorPrefix = "v1:"

func encodeCursor(version int64) string {
	return base64.RawURLEncoding.EncodeToString([]byte(cursorPrefix + strconv.FormatInt(version, 10)))
}

func decodeCursor(c string) (int64, error) {
	if c == "" {
		return 0, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(c)
	if err != nil {
		return 0, fmt.Errorf("%w: not base64", ErrInvalidCursor)
	}
	s, ok := strings.CutPrefix(string(raw), cursorPrefix)
	if !ok {
		return 0, fmt.Errorf("%w: unknown format", ErrInvalidCursor)
	}
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil || v < 1 {
		return 0, fmt.Errorf("%w: bad position", ErrInvalidCursor)
	}
	return v, nil
}
