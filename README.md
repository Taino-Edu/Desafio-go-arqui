# Desafio Go: processamento distribuído de apostas

Implementação do [desafio backend em Go](https://github.com/junglegaming/backend-challenge-go):
um serviço de carteiras que processa operações de provedores de jogos (`BET`,
`WIN`, `LOSS`, `REFUND`, `ROLLBACK`) por **HTTP e SQS**, com garantias
financeiras em ambiente distribuído: sem dinheiro em ponto flutuante, sem
movimentação duplicada, sem saldo negativo, sem evento perdido, e funcionando
com várias instâncias ao mesmo tempo.

| Garantia | Como |
|---|---|
| dinheiro exato | `int64` em centavos, `Money` imutável, texto decimal canônico (`"25.00"`) |
| extrato auditável | ledger append-only encadeado; o banco recusa edição e confere saldo = ledger no `COMMIT` |
| sem duplicidade | idempotência persistida por `(provedor, chave)` e `(provedor, id externo)` + inbox para a fila |
| sem lost update | `SELECT ... FOR UPDATE` por carteira + versão no `UPDATE` + triggers; nenhum lock em memória |
| eventos confiáveis | transactional outbox: gravados no mesmo commit, publicados depois, at-least-once |
| quem pode o quê | OAuth 2.0/OIDC (Keycloak), `client_credentials`; o provedor vem do token |
| rastreável | OpenTelemetry: um trace vai da requisição (ou mensagem) ao SQL e à publicação do evento, que leva o `traceparent` adiante |
| prova | testes com Postgres, Keycloak e LocalStack reais, 3 processos, `kill -9`, dependências fora do ar |

## Documentação

| Documento | Para quê |
|---|---|
| [ARCHITECTURE.md](ARCHITECTURE.md) | decisões técnicas, interpretações, limitações e trabalho não concluído |
| [docs/GUIA-DO-DESAFIO.md](docs/GUIA-DO-DESAFIO.md) | guia técnico dos conceitos e roteiro de construção |
| [docs/CONCEITOS-EXPLICADOS.md](docs/CONCEITOS-EXPLICADOS.md) | os mesmos conceitos sem jargão, com analogias e vídeos |
| [docs/LOAD-TEST.md](docs/LOAD-TEST.md) | teste de carga: comando, ambiente, metodologia, vazão, p50/p95/p99, erros, conflitos, atraso da outbox |

## Pré-requisitos

- Docker com Docker Compose v2 (para tudo).
- Go 1.27+ (para rodar testes ou a API fora do Docker).
- `curl` e `jq` (para os exemplos e o [`scripts/demo.sh`](scripts/demo.sh)).

## Início rápido (checkout limpo)

```sh
docker compose up --build         # Postgres + migrations + LocalStack (filas) + Keycloak + API
# em outro terminal, quando a API estiver "healthy":
./scripts/demo.sh                 # fluxo completo autenticado, conferindo cada resposta
docker compose down -v            # derruba e apaga os dados
```

O compose sobe tudo na ordem certa, sem passos manuais:

1. **Postgres** cria os papéis `wallet_owner` (dono do schema) e `wallet_app`
   (aplicação, sem DDL e sem `UPDATE`/`DELETE` no ledger) e o banco `wallet`.
2. **migrate** aplica as migrations como `wallet_owner` e termina.
3. **LocalStack** cria as filas FIFO e suas DLQs
   ([`deploy/localstack/init-sqs.sh`](deploy/localstack/init-sqs.sh)).
4. **Keycloak** importa o realm `wallet` com os clientes de teste
   ([`deploy/keycloak/realm-wallet.json`](deploy/keycloak/realm-wallet.json)).
5. **API** sobe depois que as migrations terminaram e o Keycloak e o
   LocalStack estão saudáveis.

O `scripts/demo.sh` passa por: `401` sem token, abertura de carteira, aposta
(`201`), replay (`200`, mesmo saldo), chave reutilizada (`409`), provedor
agindo por outro (`403`), saldo insuficiente (`422`), `REFUND` antes da aposta
(`202`) concluído pelo worker, a mesma operação pela fila SQS, ledger,
reconciliação e métricas.

> Atrás de um proxy que inspeciona TLS, o `go mod download` do build pode
> falhar com `x509: certificate signed by unknown authority`. Informe o bundle
> de CAs do proxy: `EXTRA_CA_CERT=/caminho/ca.crt docker compose up --build`.

| Serviço | Endereço | Credenciais (só local) |
|---|---|---|
| API | `http://localhost:8080` | token do Keycloak (`client_credentials`), ver abaixo |
| PostgreSQL | `localhost:5432/wallet` | app `wallet_app/wallet_app`, migrations `wallet_owner/wallet_owner`, admin `postgres/postgres` |
| LocalStack (SQS) | `http://localhost:4566` | `test/test` |
| Keycloak | `http://localhost:8081` | admin `admin/admin`; realm `wallet` |
| Jaeger (traces) | `http://localhost:16686` | — |

As portas podem ser trocadas copiando [`.env.example`](.env.example) para `.env`.

### Filas

| Fila | Papel |
|---|---|
| `wager-transactions.fifo` | entrada: operações dos provedores (`MessageGroupId` = `walletId`) |
| `wager-transactions-dlq.fifo` | mensagens inválidas ou que esgotaram as tentativas (`maxReceiveCount=5`), com o motivo |
| `wallet-events.fifo` | saída: eventos publicados pela outbox (`MessageGroupId` = agregado) |
| `wallet-events-dlq.fifo` | DLQ dos consumidores de eventos |

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

# reconciliação: saldo armazenado x saldo reconstruído pelo ledger (não altera nada)
curl -s -X POST localhost:8080/wallets/<walletId>/reconciliation -H "Authorization: Bearer $ADMIN"

curl -s localhost:8080/health/ready   # público
curl -s localhost:8080/metrics        # público, formato Prometheus
```

Métricas mais úteis (lista completa e decisões em
[ARCHITECTURE.md](ARCHITECTURE.md#observabilidade-métricas-logs-e-traces)):

```sh
curl -s localhost:8080/metrics | grep -E '^(wager_transactions_total|idempotent_replays_total|wallet_lock_conflicts_total|sqs_messages_total|sqs_queue_messages|outbox_lag_seconds|outbox_pending_events|reconciliation_mismatch_total)'
```

Traces: abra o Jaeger em `http://localhost:16686`, serviço `wallet`. Uma
operação aparece como `POST /wagering/transactions` com um span por comando
SQL e, logo depois, os spans `publish <evento>` da outbox, no mesmo trace.
Mande um `traceparent` (W3C) para pendurar o trace no do seu cliente; os
logs de cada requisição trazem `traceId` para ir do log ao trace:

```sh
curl -s -X POST localhost:8080/wagering/transactions -H "Authorization: Bearer $PROVIDER_A" \
  -H 'traceparent: 00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01' ...
# Jaeger: http://localhost:16686/trace/4bf92f3577b34da6a3ce929d0e0e4736
```

Enviar uma operação pela fila (mesmo efeito e mesma idempotência do HTTP):

```sh
BODY='{"messageId":"msg-1","type":"WagerTransactionRequested","occurredAt":"2026-09-08T12:00:00Z",
 "data":{"providerId":"provider-a","externalTransactionId":"transaction-124","idempotencyKey":"provider-a:transaction-124",
 "playerId":"0192f28f-5dc0-7d58-bdb2-814ad6a0f4a1","walletId":"<walletId>","roundId":"round-987",
 "gameId":"fortune-chimp","kind":"BET","money":{"amount":"25.00","currency":"BRL"}}}'
docker compose exec localstack awslocal sqs send-message \
  --queue-url http://localhost:4566/000000000000/wager-transactions.fifo \
  --message-group-id '<walletId>' --message-deduplication-id msg-1 --message-body "$BODY"

# mensagens que foram para a DLQ (com o motivo)
docker compose exec localstack awslocal sqs receive-message --message-attribute-names All \
  --queue-url http://localhost:4566/000000000000/wager-transactions-dlq.fifo
```

Eventos publicados pela outbox (`WagerTransactionProcessed`, `WalletBalanceChanged`...):

```sh
docker compose exec localstack awslocal sqs receive-message --max-number-of-messages 10 \
  --attribute-names MessageGroupId MessageDeduplicationId --message-attribute-names All \
  --queue-url http://localhost:4566/000000000000/wallet-events.fifo

# o que ainda falta publicar
docker compose exec postgres psql -U wallet_owner -d wallet -c \
  "SELECT event_type, attempts, last_error, next_attempt_at FROM outbox_events WHERE published_at IS NULL"
```

### Rodar a API fora do Docker

```sh
docker compose up -d postgres migrate keycloak localstack
cp .env.example .env
set -a; . ./.env; set +a
go run ./cmd/server
```

Variáveis de ambiente (a configuração inválida é recusada na partida, com
código de saída 2):

| Variável | Padrão | Uso |
|---|---|---|
| `DATABASE_URL` | — (obrigatória) | conexão com o papel `wallet_app` |
| `OIDC_ISSUER` | — (obrigatória) | emissor esperado nos tokens; não existe modo sem autenticação |
| `OIDC_AUDIENCE` | `wallet-api` | audiência exigida |
| `OIDC_JWKS_URL` | `<issuer>/protocol/openid-connect/certs` | onde buscar as chaves públicas |
| `HTTP_ADDR` | `:8080` | endereço do servidor |
| `INSTANCE_ID` | hostname | identifica a instância nos logs |
| `LOG_LEVEL` | `info` | `debug`, `info`, `warn`, `error` (health checks e `/metrics` são logados em `debug`) |
| `DB_MAX_CONNS` | `20` | conexões do pool |
| `DB_STATEMENT_TIMEOUT` / `DB_LOCK_TIMEOUT` | `5s` / `3s` | limites por comando e por espera de lock |
| `HTTP_REQUEST_TIMEOUT` | `10s` | prazo de cada requisição |
| `START_TIMEOUT` / `SHUTDOWN_TIMEOUT` | `30s` / `25s` | prazos de partida e de desligamento gracioso |
| `SQS_ENABLED` | `true` | liga o consumidor da fila de entrada |
| `AWS_REGION` / `SQS_ENDPOINT` | `us-east-1` / — | região; endpoint só para LocalStack |
| `SQS_ACCESS_KEY_ID` / `SQS_SECRET_ACCESS_KEY` | — | só LocalStack; na AWS use o papel IAM |
| `SQS_INPUT_QUEUE` / `SQS_INPUT_DLQ` | `wager-transactions.fifo` / `wager-transactions-dlq.fifo` | nome ou URL das filas |
| `SQS_CONSUMER_NAME` | `wager-transactions-consumer` | escopo da inbox |
| `SQS_POLLERS` / `SQS_MAX_MESSAGES` / `SQS_WAIT_TIME` | `2` / `10` / `10s` | recebimento (long polling) |
| `SQS_RETRY_BASE_DELAY` / `SQS_RETRY_MAX_DELAY` | `1s` / `1m` | backoff de visibilidade em falha transitória |
| `SQS_ALLOWED_PROVIDERS` | — (qualquer) | provedores aceitos na fila, separados por vírgula |
| `OUTBOX_PUBLISHER_ENABLED` | `true` | liga o publicador da outbox nesta instância |
| `OUTBOX_QUEUE` | `wallet-events.fifo` | fila de eventos (nome ou URL); usa a conexão SQS acima |
| `OUTBOX_BATCH_SIZE` / `OUTBOX_POLL_INTERVAL` | `100` / `500ms` | agregados por rodada; espera quando não há eventos |
| `OUTBOX_PER_AGGREGATE` / `OUTBOX_PARALLELISM` | `20` / `8` | eventos seguidos de um agregado por rodada; envios ao SQS em paralelo |
| `OUTBOX_LEASE` | `30s` | reserva de um evento reivindicado (recuperação de trabalho abandonado) |
| `OUTBOX_RETRY_BASE_DELAY` / `OUTBOX_RETRY_MAX_DELAY` | `1s` / `5m` | backoff de falha de publicação |
| `TRACING_ENABLED` | `false` (`true` no compose) | liga o OpenTelemetry; desligado, os spans são no-op |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | `http://localhost:4318` | coletor OTLP/HTTP (Jaeger, Tempo, Collector...); as demais `OTEL_*` padrão também valem, ex.: `OTEL_TRACES_SAMPLER=parentbased_traceidratio`, `OTEL_TRACES_SAMPLER_ARG=0.1` |
| `REFERENCE_WORKER_ENABLED` | `true` | liga o worker de referências pendentes nesta instância |
| `REFERENCE_WORKER_INTERVAL` | `1s` | espera do worker quando não há pendência vencida |
| `REFERENCE_RETRY_BASE_DELAY` / `REFERENCE_RETRY_MAX_DELAY` | `1s` / `5m` | backoff exponencial entre tentativas |
| `REFERENCE_RETRY_MAX_ATTEMPTS` | `12` | depois disso: `REJECTED` com `REFERENCE_NOT_FOUND` |

Listar as filas:

```sh
docker compose exec localstack awslocal sqs list-queues
```

### Várias instâncias

Nada fica em memória entre requisições: idempotência, locks, pendências,
inbox e outbox estão no Postgres. Para subir mais instâncias, basta mais
processos com o mesmo `DATABASE_URL` (e `INSTANCE_ID` diferentes). Cada uma
pode ligar ou não o consumidor (`SQS_ENABLED`), o publicador
(`OUTBOX_PUBLISHER_ENABLED`) e o worker (`REFERENCE_WORKER_ENABLED`); com
vários ligados, eles disputam o trabalho com segurança (`SKIP LOCKED`,
arrendamento, inbox).

```sh
docker compose up -d postgres migrate keycloak localstack
for i in 1 2 3; do
  (set -a; . ./.env; set +a; HTTP_ADDR=:909$i INSTANCE_ID=local-$i go run ./cmd/server) &
done
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

### Unitários (sem dependências)

```sh
go test ./...
go test -race ./...
go vet ./... && go vet -tags=integration ./...
gofmt -l .                          # vazio = tudo formatado
```

### Integração (containers reais)

Os testes de integração usam a build tag `integration` e rodam contra o
Postgres, o Keycloak e o LocalStack do compose. **Nada é mockado.** Cada teste
cria o próprio banco descartável (migrado como `wallet_owner`, usado como
`wallet_app`) e as próprias filas, então podem rodar em qualquer ordem e não
tocam nos dados do ambiente local.

```sh
# 1. dependências (a API do compose não é necessária: os testes sobem a aplicação)
docker compose up -d postgres migrate keycloak localstack
docker compose ps                   # postgres, keycloak e localstack "healthy"

# 2. tudo (cerca de 70s)
go test -tags=integration -race -count=1 ./...
```

| Variável | Padrão | Uso |
|---|---|---|
| `TEST_PG_ADMIN_URL` | `postgres://postgres:postgres@localhost:5432/postgres?sslmode=disable` | superusuário que cria e apaga os bancos de teste |
| `TEST_SQS_ENDPOINT` | `http://localhost:4566` | LocalStack |

Os tokens vêm do Keycloak real em `http://localhost:8081`.

Por categoria:

```sh
# autenticação e isolamento entre provedores (Keycloak real)
go test -tags=integration -race -run 'TestAuth' ./test/integration/

# SQS: inbox, duplicatas, HTTP x SQS, DLQ, queda após o commit, shutdown (LocalStack real)
go test -tags=integration -race -run 'TestSQS' ./test/integration/

# outbox: só após o commit, publicadores concorrentes, ordem, quedas, backoff
go test -tags=integration -race -run 'TestOutbox' ./test/integration/

# concorrência e 3 instâncias independentes (compila cmd/server e sobe 3 processos)
go test -tags=integration -race -run 'Parallel|Concurrently|NotBlocked|ThreeIndependentInstances' ./test/integration/

# referências pendentes: chegada fora de ordem, expiração, reinício, workers concorrentes
go test -tags=integration -race -run 'PendingReference' ./test/integration/

# simulações de falha: Postgres e SQS fora do ar (proxy cortável), kill -9 sob carga
# em 3 processos completos, reinício preservando idempotência, pendências e saldo
go test -tags=integration -race -run 'TestChaos|TestRestart' ./test/integration/

# quedas em pontos exatos (ganchos): consumidor entre commit e remoção, publicador
# entre reivindicar e publicar e entre publicar e confirmar
go test -tags=integration -race -run 'CrashAfterCommit|RecoversAbandonedClaim|CrashBetweenPublish' ./test/integration/

# schema, migrations (up/down/up), imutabilidade do ledger e composição Fx
go test -tags=integration -race ./internal/adapters/postgres/ ./internal/platform/fxapp/

# reconciliação (consistente, divergência simulada, sob carga), métricas, logs e disputa de lock
go test -tags=integration -race -run 'TestReconciliation|TestMetrics|TestLogs|TestLockContention' ./test/integration/

# teste de carga (ambiente do compose no ar; ver docs/LOAD-TEST.md)
go run ./cmd/loadtest -duration 60s -concurrency 32 -wallets 200 -rate 300

# fuzzing do parser de dinheiro (opcional)
go test -run='^$' -fuzz=FuzzParse -fuzztime=30s ./internal/domain/money
```

O mapa entre cada teste obrigatório do enunciado e o teste que o cobre está em
[ARCHITECTURE.md](ARCHITECTURE.md#mapa-dos-testes-obrigatórios-do-enunciado).
O CI ([`.github/workflows/ci.yml`](.github/workflows/ci.yml)) roda formatação,
vet, unitários, build da imagem e a integração completa com `-race`.

## Estrutura

```
cmd/server/                  API HTTP (Uber Fx)
cmd/migrate/                 aplica/reverte migrations
cmd/loadtest/                gerador de carga (vazão, p50/p95/p99, conflitos, atraso da outbox)
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
internal/adapters/sqsconsumer/ consumidor SQS (envelope, inbox via app, DLQ, backoff, shutdown) e publicador
internal/adapters/observability/ métricas Prometheus (/metrics), log com correlationId/traceId do contexto, TracerProvider
internal/platform/config/    configuração por variáveis de ambiente
internal/platform/fxapp/     composição Uber Fx (único pacote que importa Fx)
internal/worker/             loop genérico de trabalho em segundo plano (parada observável)
internal/testsupport/pgtest/ banco descartável para testes de integração
internal/testsupport/apptest/ aplicação completa + cliente HTTP (com tokens reais) para testes
internal/testsupport/idptest/ obtém tokens do Keycloak (client_credentials) para os testes
internal/testsupport/sqstest/ filas FIFO descartáveis no LocalStack para os testes
internal/testsupport/chaostest/ proxy TCP cortável: simula Postgres ou SQS fora do ar
test/integration/            ponta a ponta: regras, idempotência, concorrência, 3 instâncias, SQS,
                             outbox, reconciliação, métricas, logs, caos (kill -9, dependências fora)
migrations/                  SQL versionado (up/down)
deploy/                      scripts do Postgres, LocalStack e realm do Keycloak
scripts/demo.sh              demonstração ponta a ponta contra o compose
docs/                        material de estudo
```
