# Desafio Go: processamento distribuído de apostas

Implementação do [desafio backend em Go](https://github.com/junglegaming/backend-challenge-go):
um serviço de carteiras que processa apostas (`BET`, `WIN`, `LOSS`, `REFUND`,
`ROLLBACK`) com garantias financeiras em ambiente distribuído.

> 🚧 Em construção. Fase atual: **7 — autenticação e autorização (Keycloak)**.

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
cp .env.example .env              # opcional: só para mudar portas
docker compose up -d --build      # Postgres + migrations + LocalStack + Keycloak + API
docker compose ps                 # aguarde os serviços "healthy"
docker compose down -v            # derruba e apaga os dados
```

> Atrás de um proxy que inspeciona TLS, o `go mod download` do build pode
> falhar com `x509: certificate signed by unknown authority`. Informe o bundle
> de CAs do proxy: `EXTRA_CA_CERT=/caminho/ca.crt docker compose up -d --build`.

| Serviço | Endereço | Credenciais (só local) |
|---|---|---|
| PostgreSQL | `localhost:5432/wallet` | app `wallet_app/wallet_app`, migrations `wallet_owner/wallet_owner` |
| LocalStack (SQS) | `http://localhost:4566` | `test/test` |
| Keycloak | `http://localhost:8081` | admin `admin/admin`; realm `wallet` |
| API | `http://localhost:8080` | token do Keycloak (`client_credentials`), ver abaixo |

### Exemplos (fluxo autenticado)

Clientes de teste do realm `wallet` (provisionados automaticamente):

| `client_id` | `client_secret` | Papel | Uso |
|---|---|---|---|
| `wallet-service` | `wallet-service-secret` | `wallet-admin` | carteiras, ledger, consulta de qualquer transação |
| `provider-a` | `provider-a-secret` | `provider` (`provider_id=provider-a`) | operações do provedor A |
| `provider-b` | `provider-b-secret` | `provider` (`provider_id=provider-b`) | operações do provedor B |

```sh
token() {
  curl -s -d grant_type=client_credentials -d client_id="$1" -d client_secret="$2" \
    http://localhost:8081/realms/wallet/protocol/openid-connect/token | jq -r .access_token
}
ADMIN=$(token wallet-service wallet-service-secret)
PROVIDER_A=$(token provider-a provider-a-secret)

# abrir carteira (serviço interno)
curl -s -X POST localhost:8080/wallets -H "Authorization: Bearer $ADMIN" \
  -H 'Content-Type: application/json' -d '{
  "playerId": "0192f28f-5dc0-7d58-bdb2-814ad6a0f4a1",
  "initialBalance": {"amount": "1000.00", "currency": "BRL"}
}'

# aposta (provedor A; Idempotency-Key obrigatório; providerId = o do token)
curl -s -X POST localhost:8080/wagering/transactions -H "Authorization: Bearer $PROVIDER_A" \
  -H 'Content-Type: application/json' -H 'Idempotency-Key: provider-a:transaction-123' -d '{
  "providerId": "provider-a", "externalTransactionId": "transaction-123",
  "playerId": "0192f28f-5dc0-7d58-bdb2-814ad6a0f4a1", "walletId": "<walletId>",
  "roundId": "round-987", "gameId": "fortune-chimp", "kind": "BET",
  "money": {"amount": "25.00", "currency": "BRL"}
}'
# repita o mesmo comando: 200 com "idempotentReplay": true e o mesmo saldo

curl -s localhost:8080/providers/provider-a/wagering/transactions/transaction-123 -H "Authorization: Bearer $PROVIDER_A"
curl -s localhost:8080/wagering/transactions/<transactionId> -H "Authorization: Bearer $ADMIN"
curl -s localhost:8080/wallets/<walletId> -H "Authorization: Bearer $ADMIN"
curl -s 'localhost:8080/wallets/<walletId>/ledger?limit=50' -H "Authorization: Bearer $ADMIN"
curl -s localhost:8080/health/ready   # público
```

### Rodar a API fora do Docker

```sh
docker compose up -d postgres migrate keycloak
DATABASE_URL='postgres://wallet_app:wallet_app@localhost:5432/wallet?sslmode=disable' \
  OIDC_ISSUER=http://localhost:8081/realms/wallet \
  HTTP_ADDR=:8080 go run ./cmd/server
```

| Variável | Padrão | Uso |
|---|---|---|
| `DATABASE_URL` | — (obrigatória) | conexão com o papel `wallet_app` |
| `OIDC_ISSUER` | — (obrigatória) | emissor esperado nos tokens; não existe modo sem autenticação |
| `OIDC_AUDIENCE` | `wallet-api` | audiência exigida |
| `OIDC_JWKS_URL` | `<issuer>/protocol/openid-connect/certs` | onde buscar as chaves públicas |
| `HTTP_ADDR` | `:8080` | endereço do servidor |
| `INSTANCE_ID` | hostname | identifica a instância nos logs |
| `LOG_LEVEL` | `info` | `debug`, `info`, `warn`, `error` |
| `DB_MAX_CONNS` | `20` | conexões do pool |
| `DB_STATEMENT_TIMEOUT` / `DB_LOCK_TIMEOUT` | `5s` / `3s` | limites por comando e por espera de lock |
| `HTTP_REQUEST_TIMEOUT` | `10s` | prazo de cada requisição |
| `START_TIMEOUT` / `SHUTDOWN_TIMEOUT` | `30s` / `25s` | prazos de partida e de desligamento gracioso |
| `REFERENCE_WORKER_ENABLED` | `true` | liga o worker de referências pendentes nesta instância |
| `REFERENCE_WORKER_INTERVAL` | `1s` | espera do worker quando não há pendência vencida |
| `REFERENCE_RETRY_BASE_DELAY` / `REFERENCE_RETRY_MAX_DELAY` | `1s` / `5m` | backoff exponencial entre tentativas |
| `REFERENCE_RETRY_MAX_ATTEMPTS` | `12` | depois disso: `REJECTED` com `REFERENCE_NOT_FOUND` |

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

# integração: Postgres e Keycloak reais, um banco descartável por teste
docker compose up -d postgres keycloak
go test -tags=integration -race ./...

# autenticação e isolamento entre provedores (Keycloak real)
go test -tags=integration -race -run 'TestAuth' ./test/integration/

# só os testes de concorrência e de 3 instâncias (compila cmd/server e sobe 3 processos)
go test -tags=integration -race -run 'Parallel|Concurrently|NotBlocked|ThreeIndependentInstances' ./test/integration/

# referências pendentes: chegada fora de ordem, expiração, reinício, workers concorrentes
go test -tags=integration -race -run 'PendingReference' ./test/integration/

# fuzzing do parser de dinheiro (opcional)
go test -run='^$' -fuzz=FuzzParse -fuzztime=30s ./internal/domain/money
```

## Estrutura

```
cmd/server/                  API HTTP (Uber Fx)
cmd/migrate/                 aplica/reverte migrations
internal/domain/
  money/                     value object Money (centavos em int64, sem float)
  wallet/                    carteira (raiz do agregado) e lançamento de ledger
  wagering/                  transação de aposta, máquina de estados e regras dos 5 tipos
  events/                    eventos de integração e envelope
  domainerr/                 erros de validação compartilhados
internal/app/                casos de uso e portas (interfaces)
internal/adapters/postgres/  repositórios pgx, transação (Store), migrations
internal/adapters/httpapi/   rotas, middlewares (inclusive autenticação), erros, health
internal/adapters/auth/      validação de JWT do Keycloak (OIDC, JWKS)
internal/platform/config/    configuração por variáveis de ambiente
internal/platform/fxapp/     composição Uber Fx (único pacote que importa Fx)
internal/worker/             loop genérico de trabalho em segundo plano (parada observável)
internal/testsupport/pgtest/ banco descartável para testes de integração
internal/testsupport/apptest/ aplicação completa + cliente HTTP (com tokens reais) para testes
internal/testsupport/idptest/ obtém tokens do Keycloak (client_credentials) para os testes
test/integration/            ponta a ponta: regras, idempotência, concorrência, 3 instâncias
migrations/                  SQL versionado (up/down)
deploy/                      scripts do Postgres, LocalStack e realm do Keycloak
docs/                        material de estudo
```
