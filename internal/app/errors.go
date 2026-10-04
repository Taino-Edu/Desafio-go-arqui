package app

import "errors"

var (
	// ErrWalletNotFound: a carteira pedida não existe.
	ErrWalletNotFound = errors.New("wallet not found")

	// ErrWalletAlreadyExists: o jogador já tem carteira nessa moeda.
	ErrWalletAlreadyExists = errors.New("wallet already exists for player and currency")

	// ErrInvalidCursor: o cursor de paginação não foi gerado por este serviço.
	ErrInvalidCursor = errors.New("invalid pagination cursor")

	// ErrTransient: falha temporária de infraestrutura (banco fora, timeout,
	// deadlock, disputa de lock). Pode ser tentada de novo; nada foi confirmado.
	// Os adaptadores embrulham seus erros com este sentinela.
	ErrTransient = errors.New("temporarily unavailable")
)
