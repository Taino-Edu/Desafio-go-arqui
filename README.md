# Desafio Go: processamento distribuído de apostas

Implementação do [desafio backend em Go](https://github.com/junglegaming/backend-challenge-go):
um serviço de carteiras que processa apostas (`BET`, `WIN`, `LOSS`, `REFUND`,
`ROLLBACK`) com garantias financeiras em ambiente distribuído.

> 🚧 Em construção. Fase atual: **2 — modelo de domínio** (`Wallet`, `LedgerEntry`, `WagerTransaction`, eventos).

## Documentação

| Documento | Para quê |
|---|---|
| [ARCHITECTURE.md](ARCHITECTURE.md) | decisões técnicas, preenchidas a cada fase |
| [docs/GUIA-DO-DESAFIO.md](docs/GUIA-DO-DESAFIO.md) | guia técnico dos conceitos e roteiro de fases |
| [docs/CONCEITOS-EXPLICADOS.md](docs/CONCEITOS-EXPLICADOS.md) | os mesmos conceitos sem jargão, com vídeos |

## Pré-requisitos

- Go 1.24+

## Comandos

```sh
go test ./...          # testes
go test -race ./...    # testes com detector de race condition
go vet ./...           # análise estática
gofmt -l .             # lista arquivos não formatados (deve sair vazio)

# fuzzing do parser de dinheiro (opcional)
go test -run='^$' -fuzz=FuzzParse -fuzztime=30s ./internal/domain/money
```

## Estrutura

```
internal/domain/
  money/       value object Money (centavos em int64, sem float)
  wallet/      carteira (raiz do agregado) e lançamento de ledger
  wagering/    transação de aposta, máquina de estados e regras dos 5 tipos
  events/      eventos de integração e envelope
  domainerr/   erros de validação compartilhados
docs/                    material de estudo
```
