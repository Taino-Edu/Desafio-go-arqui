# Desafio Go: processamento distribuído de apostas

Implementação do [desafio backend em Go](https://github.com/junglegaming/backend-challenge-go):
um serviço de carteiras que processa apostas (`BET`, `WIN`, `LOSS`, `REFUND`,
`ROLLBACK`) com garantias financeiras em ambiente distribuído.

> 🚧 Em construção. Fase atual: **3 — infraestrutura local e schema do banco**.

## Documentação

| Documento | Para quê |
|---|---|
| [ARCHITECTURE.md](ARCHITECTURE.md) | decisões técnicas, preenchidas a cada fase |
| [docs/GUIA-DO-DESAFIO.md](docs/GUIA-DO-DESAFIO.md) | guia técnico dos conceitos e roteiro de fases |
| [docs/CONCEITOS-EXPLICADOS.md](docs/CONCEITOS-EXPLICADOS.md) | os mesmos conceitos sem jargão, com vídeos |

## Pré-requisitos

- Go 1.27+
- Docker com Docker Compose v2

## Ambiente local

```sh
cp .env.example .env        # opcional: só para mudar portas
docker compose up -d        # Postgres + migrations + LocalStack (SQS) + Keycloak
docker compose ps           # aguarde postgres, localstack e keycloak "healthy"
docker compose down -v      # derruba e apaga os dados
```

| Serviço | Endereço | Credenciais (só local) |
|---|---|---|
| PostgreSQL | `localhost:5432/wallet` | app `wallet_app/wallet_app`, migrations `wallet_owner/wallet_owner` |
| LocalStack (SQS) | `http://localhost:4566` | `test/test` |
| Keycloak | `http://localhost:8081` | admin `admin/admin`; realm `wallet` |

Obter um token de teste (fluxo `client_credentials`):

```sh
curl -s -d grant_type=client_credentials \
     -d client_id=provider-a -d client_secret=provider-a-secret \
     http://localhost:8081/realms/wallet/protocol/openid-connect/token
```

Listar as filas:

```sh
docker compose exec localstack awslocal sqs list-queues
```

## Migrations

O serviço `migrate` do compose aplica tudo ao subir. Manualmente:

```sh
export MIGRATE_DATABASE_URL='postgres://wallet_owner:wallet_owner@localhost:5432/wallet?sslmode=disable'
go run ./cmd/migrate up        # aplica as pendentes
go run ./cmd/migrate down 1    # reverte a última
go run ./cmd/migrate down all  # reverte tudo (apaga os dados)
go run ./cmd/migrate version
```

## Testes

```sh
go test ./...                       # unitários
go test -race ./...                 # unitários com detector de race condition
go vet ./...

# integração: Postgres real, um banco descartável por teste
docker compose up -d postgres
go test -tags=integration -race ./...

# fuzzing do parser de dinheiro (opcional)
go test -run='^$' -fuzz=FuzzParse -fuzztime=30s ./internal/domain/money
```

## Estrutura

```
cmd/migrate/                 aplica/reverte migrations
internal/domain/
  money/                     value object Money (centavos em int64, sem float)
  wallet/                    carteira (raiz do agregado) e lançamento de ledger
  wagering/                  transação de aposta, máquina de estados e regras dos 5 tipos
  events/                    eventos de integração e envelope
  domainerr/                 erros de validação compartilhados
internal/adapters/postgres/  migrations (pgx + golang-migrate) e testes do schema
internal/testsupport/pgtest/ banco descartável para testes de integração
migrations/                  SQL versionado (up/down)
deploy/                      scripts do Postgres, LocalStack e realm do Keycloak
docs/                        material de estudo
```
