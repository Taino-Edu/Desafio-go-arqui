// Package accounting é o razão em partidas dobradas: cada movimentação de
// dinheiro é um lançamento contábil (JournalEntry) com pelo menos duas
// partidas cujos débitos somam exatamente o mesmo que os créditos. O
// dinheiro nunca aparece nem some: sai de uma conta e entra em outra.
//
// Plano de contas, do ponto de vista da operadora:
//
//	wallet:<walletId>             PASSIVO  o que a casa deve ao jogador (saldo da carteira)
//	cash:<moeda>                  ATIVO    dinheiro que entrou na plataforma (saldo de abertura)
//	provider:<providerId>:<moeda> RECEITA  resultado com o provedor: apostas − prêmios (GGR)
//
// O ledger por carteira (wallet.LedgerEntry) continua sendo o extrato do
// jogador: ele é exatamente a partida da conta wallet:<id> de cada
// lançamento, e o banco confere essa correspondência no COMMIT.
package accounting

import (
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/Taino-Edu/Desafio-go-arqui/internal/domain/domainerr"
	"github.com/Taino-Edu/Desafio-go-arqui/internal/domain/money"
	"github.com/Taino-Edu/Desafio-go-arqui/internal/domain/wagering"
	"github.com/Taino-Edu/Desafio-go-arqui/internal/domain/wallet"
)

// AccountType é a natureza contábil da conta.
type AccountType string

const (
	Asset     AccountType = "ASSET"     // ativo: cresce a débito
	Liability AccountType = "LIABILITY" // passivo: cresce a crédito
	Revenue   AccountType = "REVENUE"   // receita: cresce a crédito
)

// NormalSide é o lado em que o saldo da conta cresce.
func (t AccountType) NormalSide() wallet.Direction {
	if t == Asset {
		return wallet.Debit
	}
	return wallet.Credit
}

// Account identifica uma conta do plano de contas. O código é derivado dos
// outros campos (o banco confere a mesma regra).
type Account struct {
	code       string
	typ        AccountType
	currency   money.Currency
	walletID   uuid.UUID // só contas de carteira
	providerID string    // só contas de provedor
}

// WalletAccount é o passivo da casa com o dono da carteira.
func WalletAccount(walletID uuid.UUID, currency money.Currency) (Account, error) {
	if walletID == uuid.Nil {
		return Account{}, domainerr.Field("walletId", "required")
	}
	if currency.IsZero() {
		return Account{}, domainerr.Field("currency", "required")
	}
	return Account{code: "wallet:" + walletID.String(), typ: Liability, currency: currency, walletID: walletID}, nil
}

// CashAccount é o ativo que recebe o dinheiro que entra na plataforma.
func CashAccount(currency money.Currency) (Account, error) {
	if currency.IsZero() {
		return Account{}, domainerr.Field("currency", "required")
	}
	return Account{code: "cash:" + currency.Code(), typ: Asset, currency: currency}, nil
}

// ProviderAccount acumula o resultado das apostas de um provedor: crédito
// nas apostas, débito nos prêmios e devoluções. O saldo é o GGR do provedor.
func ProviderAccount(providerID string, currency money.Currency) (Account, error) {
	if strings.TrimSpace(providerID) == "" {
		return Account{}, domainerr.Field("providerId", "required")
	}
	if currency.IsZero() {
		return Account{}, domainerr.Field("currency", "required")
	}
	return Account{code: "provider:" + providerID + ":" + currency.Code(), typ: Revenue,
		currency: currency, providerID: providerID}, nil
}

func (a Account) Code() string             { return a.code }
func (a Account) Type() AccountType        { return a.typ }
func (a Account) Currency() money.Currency { return a.currency }
func (a Account) WalletID() uuid.UUID      { return a.walletID }
func (a Account) ProviderID() string       { return a.providerID }

// Posting é uma partida: um lado de um lançamento, numa conta.
type Posting struct {
	Account   Account
	Direction wallet.Direction
	Amount    money.Money
}

// JournalEntry é um lançamento contábil balanceado e imutável.
type JournalEntry struct {
	transactionID uuid.UUID
	postings      []Posting
	createdAt     time.Time
}

// NewJournalEntry valida e cria um lançamento:
//   - pelo menos duas partidas, cada uma com valor positivo;
//   - uma moeda só, igual à moeda de cada conta;
//   - no máximo uma partida por conta;
//   - Σ débitos = Σ créditos.
func NewJournalEntry(transactionID uuid.UUID, createdAt time.Time, postings ...Posting) (JournalEntry, error) {
	switch {
	case transactionID == uuid.Nil:
		return JournalEntry{}, domainerr.Field("transactionId", "required")
	case createdAt.IsZero():
		return JournalEntry{}, domainerr.Field("createdAt", "required")
	case len(postings) < 2:
		return JournalEntry{}, domainerr.Field("postings", "a journal entry needs at least two postings")
	}

	cur := postings[0].Amount.Currency()
	debits, _ := money.Zero(cur)
	credits := debits
	seen := map[string]bool{}
	for _, p := range postings {
		if p.Account.code == "" {
			return JournalEntry{}, domainerr.Field("account", "required")
		}
		if seen[p.Account.code] {
			return JournalEntry{}, domainerr.Field("postings", "one posting per account")
		}
		seen[p.Account.code] = true
		if !p.Amount.IsPositive() {
			return JournalEntry{}, domainerr.Field("amount", "must be greater than zero")
		}
		if p.Amount.Currency() != cur || p.Account.currency != cur {
			return JournalEntry{}, money.ErrCurrencyMismatch
		}
		var err error
		switch p.Direction {
		case wallet.Debit:
			debits, err = debits.Add(p.Amount)
		case wallet.Credit:
			credits, err = credits.Add(p.Amount)
		default:
			return JournalEntry{}, domainerr.Field("direction", "must be DEBIT or CREDIT")
		}
		if err != nil { // overflow
			return JournalEntry{}, err
		}
	}
	if !debits.Equal(credits) {
		return JournalEntry{}, domainerr.Field("postings", "debits must equal credits")
	}
	return JournalEntry{transactionID: transactionID, postings: append([]Posting(nil), postings...),
		createdAt: createdAt.UTC()}, nil
}

// ForWalletMovement monta o lançamento de uma movimentação de carteira: a
// partida da conta da carteira repete o lançamento do ledger (mesma direção,
// mesmo valor) e a contrapartida vai, no lado oposto, para counterpart.
//
//	BET 25,00:  D wallet:<id> 25,00  /  C provider:<p>:BRL 25,00
//	WIN 40,00:  D provider:<p>:BRL 40,00  /  C wallet:<id> 40,00
func ForWalletMovement(e wallet.LedgerEntry, counterpart Account) (JournalEntry, error) {
	w, err := WalletAccount(e.WalletID(), e.Amount().Currency())
	if err != nil {
		return JournalEntry{}, err
	}
	return NewJournalEntry(e.TransactionID(), e.CreatedAt(),
		Posting{Account: w, Direction: e.Direction(), Amount: e.Amount()},
		Posting{Account: counterpart, Direction: e.Direction().Opposite(), Amount: e.Amount()},
	)
}

// ForTransaction monta o lançamento da movimentação e de uma operação,
// escolhendo a contrapartida pela natureza da operação:
//
//	OPENING           -> cash:<moeda>   (o dinheiro entrou na plataforma)
//	BET/WIN/REFUND... -> provider:<p>:<moeda> (resultado com aquele provedor)
//
// O banco confere a mesma regra no COMMIT.
func ForTransaction(tx *wagering.WagerTransaction, e wallet.LedgerEntry) (JournalEntry, error) {
	if tx == nil || tx.ID() != e.TransactionID() || tx.WalletID() != e.WalletID() {
		return JournalEntry{}, domainerr.Field("transactionId", "ledger entry does not belong to the transaction")
	}
	cur := e.Amount().Currency()
	var (
		counterpart Account
		err         error
	)
	if tx.Kind() == wagering.KindOpening {
		counterpart, err = CashAccount(cur)
	} else if ext := tx.External(); ext != nil {
		counterpart, err = ProviderAccount(ext.ProviderID, cur)
	} else {
		return JournalEntry{}, domainerr.Field("providerId", "external transaction without provider")
	}
	if err != nil {
		return JournalEntry{}, err
	}
	return ForWalletMovement(e, counterpart)
}

func (j JournalEntry) TransactionID() uuid.UUID { return j.transactionID }
func (j JournalEntry) CreatedAt() time.Time     { return j.createdAt }

// Postings devolve uma cópia das partidas.
func (j JournalEntry) Postings() []Posting { return append([]Posting(nil), j.postings...) }
