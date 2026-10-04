package app

import "errors"

var (
	// ErrWalletNotFound: a carteira pedida não existe.
	ErrWalletNotFound = errors.New("wallet not found")

	// ErrWalletAlreadyExists: o jogador já tem carteira nessa moeda.
	ErrWalletAlreadyExists = errors.New("wallet already exists for player and currency")

	// ErrTransactionNotFound: a transação pedida não existe (ou não é
	// visível para quem pediu).
	ErrTransactionNotFound = errors.New("transaction not found")

	// ErrIdempotencyKeyReused: a chave já foi usada com outro conteúdo.
	ErrIdempotencyKeyReused = errors.New("idempotency key reused with a different payload")

	// ErrDuplicateTransaction: o (provedor, id externo) já foi registrado com
	// outra chave de idempotência; a operação não é reaplicada.
	ErrDuplicateTransaction = errors.New("external transaction already registered with another idempotency key")

	// ErrInboxConflict: o mesmo messageId chegou com outro conteúdo.
	ErrInboxConflict = errors.New("message id redelivered with a different payload")

	// ErrInvalidCursor: o cursor de paginação não foi gerado por este serviço.
	ErrInvalidCursor = errors.New("invalid pagination cursor")

	// ErrTransient: falha temporária de infraestrutura (banco fora, timeout,
	// deadlock, disputa de lock). Pode ser tentada de novo; nada foi confirmado.
	// Os adaptadores embrulham seus erros com este sentinela.
	ErrTransient = errors.New("temporarily unavailable")
)
