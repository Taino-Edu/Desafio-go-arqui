# Arquitetura

Registro das decisões técnicas. Cada seção é preenchida conforme a fase
correspondente é implementada (ver o roteiro em
[docs/GUIA-DO-DESAFIO.md](docs/GUIA-DO-DESAFIO.md#parte-13-roteiro-de-construção-ordem-sugerida)).

| Seção | Status |
|---|---|
| [Organização dos pacotes](#organização-dos-pacotes) | ✅ |
| [Dinheiro (`Money`)](#dinheiro-money) | ✅ |
| [Carteira, ledger e transações](#carteira-ledger-e-transações) | ✅ |
| [Banco, migrations e invariantes no schema](#banco-migrations-e-invariantes-no-schema) | ✅ |
| [Transação SQL entre repositórios](#transação-sql-entre-repositórios) | ✅ |
| [Contrato HTTP](#contrato-http) | ✅ (reconciliação na fase 10) |
| [Idempotência](#idempotência) | ✅ HTTP; SQS reutiliza o mesmo caso de uso na fase 8 |
| [Concorrência e locks](#concorrência-e-locks) | ✅ |
| [Referências pendentes e worker](#referências-pendentes-e-worker) | ✅ |
| [Autenticação e autorização](#autenticação-e-autorização) | ✅ HTTP; SQS (credenciais e políticas do broker) na fase 8 |
| Inbox, SQS e DLQ | ⏳ fase 8 |
| Outbox | ⏳ fase 9 |
| [Uber Fx e shutdown](#uber-fx-ciclo-de-vida-e-shutdown) | ✅ HTTP, banco e worker de referências; SQS e outbox nas fases 8 e 9 |
| Observabilidade | ⏳ fase 10 |

---

## Organização dos pacotes

```
internal/domain/         regras de negócio puras: stdlib + uuid
internal/app/            casos de uso; define as portas (interfaces) de persistência
internal/adapters/       implementações: postgres (pgx), httpapi (net/http); sqs e auth virão
internal/platform/       config (variáveis de ambiente) e fxapp (composição Uber Fx)
cmd/server, cmd/migrate  executáveis
```

Dependências apontam para dentro: `adapters → app → domain`. O domínio não
importa infraestrutura; `app` não importa pgx, HTTP nem Fx; **só `fxapp`
importa Fx**. Isso permite testar o domínio sem banco e trocar adaptadores
sem tocar nas regras.

---

## Dinheiro (`Money`)

Pacote: [`internal/domain/money`](internal/domain/money).

### Representação

`int64` em **unidades mínimas** (centavos), com escala fixa de **2 casas**.
`"25.00"` vira `2500`. Nenhum caminho (parsing, cálculo, serialização ou
persistência) usa `float32`/`float64`.

| Aspecto | Decisão |
|---|---|
| Tipo | `Money{minor int64, currency Currency}`, campos privados, imutável |
| Moeda | `Currency` validada; aceitas `BRL`, `USD`, `EUR` (todas com 2 casas) |
| Persistência | `BIGINT` (centavos) + `CHAR(3)` (moeda) |
| Limites | de `-92233720368547758.08` a `92233720368547758.07` |
| Overflow | parsing, `Add`, `Sub` e `Neg` devolvem erro em vez de estourar |
| Valor zero | `Money{}` e `Currency{}` são inválidos; operações devolvem `ErrUninitialized` |

Por que `int64` e não uma biblioteca decimal: a escala é fixa, a aritmética
inteira é exata e rápida, mapeia diretamente para `BIGINT`, e não traz
dependência. O custo é tratar overflow manualmente, o que está coberto por testes.

### Contrato externo e parsing

Formato: `{"amount":"25.00","currency":"BRL"}`. O valor é **string** JSON; um
número JSON (`25.00`) é rejeitado sem ser convertido para float.

`money.Parse` aceita **somente a forma canônica**: `^(0|[1-9][0-9]*)\.[0-9]{2}$`.

| Rejeitado | Erro |
|---|---|
| vazio, `NaN`, `Infinity`, `1e3`, `+1.00`, `25,00`, espaços, `025.00`, `.50` | `ErrInvalidFormat` |
| `25`, `25.0`, `25.001`, `25.000` | `ErrInvalidScale` |
| `-1.00`, `-0.00` | `ErrNegativeAmount` |
| acima de `92233720368547758.07` | `ErrAmountTooLarge` |
| moeda desconhecida ou minúscula (`brl`) | `ErrInvalidCurrency` |

Todos os erros de valor casam com `errors.Is(err, money.ErrInvalidAmount)`,
o que permite ao adaptador HTTP mapear a família inteira para `400`.

**Não há normalização nem arredondamento.** Como só a forma canônica é aceita,
o texto recebido já é o texto que entra no hash de idempotência: `"25.00"` e
`"25.0"` nunca geram hashes diferentes para a mesma operação, porque o segundo
é rejeitado.

### Valores negativos

- **Entrada externa** (`Parse`): negativos são rejeitados.
- **Cálculos internos** (`Sub`, `Neg`, `FromMinorUnits`): negativos são
  permitidos, por exemplo a `difference` da reconciliação.
- **Saldo da carteira**: a proibição de saldo negativo é responsabilidade da
  `Wallet` e de uma `CHECK` no banco, não do `Money`.

`UnmarshalJSON` aceita negativos para permitir o round-trip de valores internos.
Entradas financeiras externas devem passar por `Parse`.

### Compatibilidade de moedas

`Add`, `Sub` e `Cmp` entre moedas diferentes devolvem `ErrCurrencyMismatch`.
`Equal` entre moedas diferentes devolve `false`.

---

## Carteira, ledger e transações

Pacotes: [`wallet`](internal/domain/wallet), [`wagering`](internal/domain/wagering),
[`events`](internal/domain/events), [`domainerr`](internal/domain/domainerr).
Todos usam só a biblioteca padrão e `github.com/google/uuid`.

### Princípios comuns

- **Estado encapsulado:** campos privados, sem setters. Só construtores e
  métodos de transição alteram o estado.
- **Criação vs. reidratação:** `Open`/`NewExternal`/`NewOpening` criam e
  podem emitir eventos; `Rehydrate` reconstrói a partir do banco, valida a
  coerência do registro e **não** reaplica movimentos, não muda a versão nem
  emite eventos.
- **Eventos pendentes:** as entidades acumulam eventos; a camada de aplicação
  os retira com `PullEvents()` e os grava na outbox na mesma transação SQL.
- **Valor zero rejeitado:** `Wallet{}`, IDs nulos, `Money{}` e instantes
  zerados devolvem erro.
- **Erros classificáveis:** validação → `domainerr.ErrValidation` (`FieldError`
  traz o campo); saldo → `wallet.ErrInsufficientFunds`; estado →
  `wagering.ErrInvalidTransition` (`TransitionError` traz origem e destino).
  Nenhuma rejeição de negócio usa `panic`.
- **Tempo:** todo instante é recebido por parâmetro (`now`) e normalizado para
  UTC, o que torna o domínio determinístico nos testes.

### Wallet

| Regra | Onde |
|---|---|
| versão inicial `1` | `Open` |
| versão sobe só quando o saldo muda | `move`: `LOSS` e rejeições não alteram a versão |
| saldo nunca negativo | `Debit` devolve `ErrInsufficientFunds` sem alterar nada |
| moeda do movimento = moeda da carteira | `move` devolve `money.ErrCurrencyMismatch` |
| todo movimento gera `LedgerEntry` + `WalletBalanceChanged` | `move` / `record` |
| saldo inicial positivo gera crédito de abertura; zero não gera | `Open` |

A unicidade `(playerId, currency)`, o `CHECK (balance >= 0)` e o bloqueio de
concorrência ficam no banco (fases 3 e 5); o domínio é a primeira barreira,
não a única.

### LedgerEntry

Imutável. O construtor valida `balanceAfter = balanceBefore ± amount` conforme
a direção, valor positivo, saldos não negativos e moedas iguais. Como não há
transições, o mesmo construtor serve para criação e reidratação.

### WagerTransaction: máquina de estados

```
PENDING ───────────► PROCESSED | REJECTED | FAILED | PENDING_REFERENCE
PENDING_REFERENCE ─► PROCESSED | REJECTED | FAILED
PROCESSED, REJECTED, FAILED: terminais
```

- `PENDING_REFERENCE` vai direto para o desfecho quando a referência é
  resolvida; não volta para `PENDING`.
- `RescheduleReference` só conta tentativas e reagenda; não é uma transição.
- `OPENING` nasce interna e já `PROCESSED`, sem metadados externos.
- `MarkFailed` registra falha permanente (`PERMANENT_PROCESSING_ERROR`) para
  auditoria, sem evento de negócio.
- **Transitória vs. permanente:** falha transitória (banco fora, timeout,
  deadlock) não muda o estado: a operação é tentada de novo. Só falhas que
  nunca vão se resolver levam a `FAILED`.

### Regras por tipo (`wagering.Apply`)

| Tipo | Valor | Referência | Movimento |
|---|---|---|---|
| `BET` | > 0 | proibida | débito |
| `WIN` | > 0 | opcional; precisa ser `BET` | crédito |
| `LOSS` | exatamente `0.00` | proibida | nenhum; só `WagerTransactionProcessed` |
| `REFUND` | > 0 e = valor da `BET` | obrigatória; `BET` | crédito |
| `ROLLBACK` | > 0 e = valor original | obrigatória; `BET`, `WIN` ou `REFUND` | contrário do original |

Erros de formato e de regra de valor/referência acima são **validação**
(`ErrValidation`, HTTP 400, nada é persistido). As condições que dependem do
estado são **rejeições** persistidas (`REJECTED` + `failureCode`):

| `failureCode` | Tipo | Quando |
|---|---|---|
| `INSUFFICIENT_FUNDS` | definitivo | débito de `BET` sem saldo |
| `REVERSAL_INSUFFICIENT_FUNDS` | definitivo | débito de `ROLLBACK` sem saldo |
| `REFERENCE_NOT_FOUND` | definitivo | referência não chegou até esgotar as tentativas |
| `REFERENCE_NOT_PROCESSED` | definitivo | referência terminou `REJECTED`/`FAILED` |
| `ALREADY_REVERSED` | definitivo | referência já revertida (detectado pelo índice único no banco) |
| `WALLET_NOT_FOUND` | corrigível | carteira inexistente |
| `WALLET_PLAYER_MISMATCH` | corrigível | `playerId` não é o dono de `walletId` |
| `CURRENCY_MISMATCH` | corrigível | moeda diferente da carteira |
| `REFERENCE_MISMATCH` | corrigível | provedor, jogador, carteira, moeda ou rodada diferentes |
| `REFERENCE_KIND_INVALID` | corrigível | tipo referenciado não permitido (ex.: `ROLLBACK` de `ROLLBACK`) |
| `AMOUNT_MISMATCH` | corrigível | valor da reversão diferente do original |

`FailureCode.IsCorrectable()` expõe essa classificação.

### Referências

`Apply` confere primeiro as divergências estáticas (tipo, provedor, jogador,
carteira, moeda, rodada, valor), que não mudam com o tempo, e só depois o
estado da referência:

| Estado da referência | Resultado |
|---|---|
| não encontrada | `AWAITING_REFERENCE` (transação não muda) |
| `PENDING` / `PENDING_REFERENCE` | `AWAITING_REFERENCE` (espera mais um ciclo) |
| `PROCESSED` | aplica |
| `REJECTED` / `FAILED` | rejeita com `REFERENCE_NOT_PROCESSED` |

Diante de `AWAITING_REFERENCE`, a aplicação chama `MarkPendingReference`
(primeira vez), `RescheduleReference` (tentativas seguintes) ou
`MarkRejected(REFERENCE_NOT_FOUND)` quando `ReferenceRetryPolicy.Exhausted`.

Backoff: `min(1s × 2^n, 5min)`, até 12 tentativas (cerca de 24 minutos). O
jitter é somado pelo worker, para manter a política determinística.

### Combinações de REFUND e ROLLBACK

Política (a imposição definitiva é um índice único parcial no banco, fase 6):

1. Uma `BET` recebe **no máximo uma** compensação direta: `REFUND` **ou**
   `ROLLBACK`.
2. Um `WIN` ou `REFUND` recebe no máximo um `ROLLBACK`.
3. `ROLLBACK` de `ROLLBACK` é recusado (`REFERENCE_KIND_INVALID`).

Assim o mesmo débito nunca é devolvido duas vezes: `BET → REFUND → ROLLBACK
do REFUND` termina com o débito original de volta, e um novo `REFUND` da mesma
`BET` é bloqueado.

### Eventos

Um tipo concreto por evento; o construtor fixa `eventType` e `version`. O
envelope (`eventId`, `eventType`, `aggregateId`, `correlationId`,
`causationId`, `occurredAt`, `version`, `data`) é montado por
`events.NewEnvelope`, que copia tipo, versão, agregado e instante do evento.
Instantes em UTC (RFC 3339), dinheiro como string decimal, e o payload é um
snapshot (os metadados externos são copiados).

---

## Banco, migrations e invariantes no schema

Arquivos: [`migrations/`](migrations), [`deploy/postgres/01-roles.sql`](deploy/postgres/01-roles.sql),
[`internal/adapters/postgres/migrate.go`](internal/adapters/postgres/migrate.go).

### Biblioteca e migrations

- **Acesso:** `pgx/v5` com SQL explícito (sem ORM). Transações, locks e
  constraints ficam visíveis no código e no schema.
- **Migrations:** formato `golang-migrate` (`NNNNNN_nome.up.sql` /
  `.down.sql`), embutidas no binário (`migrations.FS`). Aplicadas pelo serviço
  `migrate` do compose ou por `go run ./cmd/migrate up|down <n|all>|version`.
- **Dinheiro:** `BIGINT` em centavos + `CHAR(3)` (`^[A-Z]{3}$`).

### Papéis

| Papel | Uso | Pode |
|---|---|---|
| `postgres` | administração e testes | tudo |
| `wallet_owner` | dono das tabelas; roda migrations | DDL |
| `wallet_app` | aplicação | `SELECT/INSERT/UPDATE`; no ledger só `SELECT/INSERT`; nunca `DELETE`, `TRUNCATE` ou DDL |

A aplicação não é dona das tabelas, então não consegue desligar triggers
(`ALTER TABLE ... DISABLE TRIGGER` exige ser dono). Isso é testado.

### Invariantes impostas pelo banco

Valem mesmo que o código Go tenha um bug ou que alguém acesse o banco direto.

| Invariante | Mecanismo |
|---|---|
| saldo nunca negativo | `CHECK (balance_minor >= 0)` |
| uma carteira por `(player_id, currency)` | `UNIQUE` |
| identidade da carteira imutável | trigger `wallets_guard_update` |
| versão sobe exatamente 1 quando o saldo muda, e só então | trigger `wallets_guard_update` |
| **saldo = ledger** | constraint trigger **adiado** `wallets_check_ledger`: no `COMMIT`, a versão atual da carteira precisa ter o lançamento com `balance_after` igual ao saldo |
| lançamento fecha a conta | `CHECK` `balance_after = balance_before ± amount` |
| lançamentos encadeados | trigger `ledger_guard_insert`: `balance_before` = `balance_after` do lançamento anterior (ou 0) |
| moeda do lançamento = moeda da carteira | FK composta `(wallet_id, currency)` |
| um lançamento por transação e por versão | `UNIQUE (wallet_id, transaction_id)`, `UNIQUE (wallet_id, wallet_version)` |
| ledger append-only | triggers contra `UPDATE`/`DELETE`/`TRUNCATE` (inclusive do dono) + sem permissão para a aplicação |
| idempotência | `UNIQUE (provider_id, external_transaction_id)` e `UNIQUE (provider_id, idempotency_key)` |
| interno vs. externo | `CHECK wager_tx_origin_shape` |
| um `OPENING` por carteira | índice único parcial |
| uma reversão bem-sucedida por referência | índice único parcial `WHERE kind IN ('REFUND','ROLLBACK') AND status = 'PROCESSED'` |
| política de valor zero e de referência | `CHECK` por tipo |
| campos exigidos por estado | `CHECK wager_tx_status_shape` |
| máquina de estados | trigger `wager_tx_guard_update`: terminal não muda; só transições válidas; campos de negócio imutáveis |
| registros financeiros não são apagados | triggers em `wallets` e `wager_transactions` |
| inbox deduplica | `PRIMARY KEY (consumer_name, message_id)` |
| payload da outbox é snapshot | trigger `outbox_guard_update`; `published_at` não volta a nulo |

Com o encadeamento e a checagem adiada, o saldo armazenado é sempre igual ao
último `balance_after` da cadeia de lançamentos, e a cadeia começa em zero.
A reconciliação (fase 10) passa a ser uma prova, não só um alerta.

### Decisões e limitações

- `wager_transactions.wallet_id` **não tem FK**: uma operação rejeitada com
  `WALLET_NOT_FOUND` precisa ser persistida para o replay idempotente, e
  aponta para uma carteira que não existe.
- `wallet_version` foi acrescentado ao lançamento (além dos campos pedidos):
  dá a ordem estável usada pela paginação do ledger e encadeia os lançamentos.
- Os triggers rodam antes das FKs; por isso uma transação inexistente no
  lançamento é barrada pelo trigger (`check_violation`), não pela FK.
- Um superusuário ainda pode desligar triggers. A proteção é contra a
  aplicação e contra o dono do schema em operação normal.

### Ambiente local

`docker compose up -d` sobe:

| Serviço | Porta | O que faz |
|---|---|---|
| `postgres` | 5432 | cria `wallet_owner`, `wallet_app` e o banco `wallet` |
| `migrate` | — | aplica as migrations e termina |
| `localstack` | 4566 | SQS: `wager-transactions.fifo` e `wallet-events.fifo`, cada uma com DLQ (`maxReceiveCount=5`, visibilidade 30s) |
| `keycloak` | 8081 | realm `wallet` com `provider-a`, `provider-b` (papel `provider`, claim `provider_id`) e `wallet-service` (papel `wallet-admin`); audiência `wallet-api` |

O emissor do Keycloak é fixo (`KC_HOSTNAME`), então tokens obtidos de fora
(`localhost:8081`) ou de dentro da rede do compose (`keycloak:8080`) têm o mesmo
`iss`.

### Testes de integração

`//go:build integration`. Rodam contra o Postgres real do compose; cada teste
cria um banco descartável (`pgtest.New`), aplica as migrations como
`wallet_owner` e usa `wallet_app`, como a aplicação. Cada teste tenta violar
uma invariante e confere o `SQLSTATE` devolvido.

---

## Transação SQL entre repositórios

Arquivos: [`internal/app/ports.go`](internal/app/ports.go),
[`internal/adapters/postgres/store.go`](internal/adapters/postgres/store.go).

A camada de aplicação define a porta `Store`:

```go
store.WithinTx(ctx, func(ctx context.Context, r app.Repositories) error {
    r.Wallets().Insert(...)       // todos estes repositórios
    r.Transactions().Insert(...)  // compartilham a MESMA pgx.Tx
    r.Ledger().Insert(...)
    return r.Outbox().Append(...) // erro em qualquer passo => ROLLBACK de tudo
})
```

- A implementação abre `BEGIN` (READ COMMITTED), entrega repositórios
  construídos sobre a mesma `pgx.Tx` e faz `COMMIT` no fim. Se `fn` falhar ou o
  `COMMIT` falhar (por exemplo, a checagem adiada saldo = ledger), tudo é
  desfeito. O rollback usa um contexto próprio, porque o da requisição pode já
  ter expirado.
- `Reader()` devolve repositórios sobre o pool, para consultas.
- Os repositórios recebem uma interface `querier` (o que `pgxpool.Pool` e
  `pgx.Tx` têm em comum), então o mesmo código serve dentro e fora de
  transação.

### Proteção contra lost update (duas camadas no Go, uma no banco)

1. `GetForUpdate`: `SELECT ... FOR UPDATE` trava só a linha da carteira.
2. `Update`: `UPDATE ... WHERE id = $1 AND version = $nova - 1`. Se outro
   escritor confirmou antes, nenhuma linha muda e o caso de uso recebe
   `ErrTransient` em vez de sobrescrever.
3. No banco, o trigger de versão e a checagem adiada saldo = ledger.

Testado em `TestStore_UpdateRejectsStaleVersion`: com a condição de versão
removida do Go, o trigger do banco ainda barra a escrita.

### Falhas transitórias

`postgres.classify` embrulha com `app.ErrTransient`: SQLSTATE `40001`
(serialização), `40P01` (deadlock), `55P03` (`lock_timeout`), `57014`
(`statement_timeout`), `57P0x`, `53300`, classe `08` (conexão), erros de rede
e prazo do contexto. O HTTP responde `503` com `Retry-After`. O pool define
`statement_timeout` (5s) e `lock_timeout` (3s) por conexão, menores que o prazo
da requisição (10s), para que esperar por uma carteira disputada nunca segure
uma requisição indefinidamente.

---

## Uber Fx: ciclo de vida e shutdown

Arquivo: [`internal/platform/fxapp/fxapp.go`](internal/platform/fxapp/fxapp.go).

| Módulo (`fx.Module`) | Fornece (`fx.Provide`) | Ciclo de vida (`fx.Lifecycle`) |
|---|---|---|
| `logging` | `*slog.Logger` JSON | — |
| `postgres` | `*pgxpool.Pool`, `app.Store` | OnStart: ping (sem banco, não sobe). OnStop: fecha o pool |
| `app` | `Clock`, `IDGenerator`, `WalletService` | — |
| `http` | `Health`, handler, `*httpapi.Server` | `fx.Invoke` registra OnStart (abre a porta) e OnStop (shutdown gracioso) |
| `workers` | — | `fx.Invoke` registra o loop do worker de referências: OnStart inicia a goroutine; OnStop para de buscar trabalho, espera o item em andamento e, se o prazo acabar, cancela o item (a transação é desfeita) |

- **Partida:** `cmd/server` valida a configuração antes do Fx (inválida → sai
  com código 2). O Fx executa os OnStart em ordem: banco, depois HTTP; cada um
  dentro de `START_TIMEOUT`.
- **Parada (SIGTERM):** o Fx executa os OnStop em ordem **inversa**, dentro de
  `SHUTDOWN_TIMEOUT`:
  1. HTTP: o readiness passa a `503 draining`, o servidor para de aceitar
     conexões e espera as requisições em andamento (`http.Server.Shutdown`);
     se o prazo estourar, fecha as conexões restantes.
  2. Worker: para de pegar pendências e conclui (ou desfaz) a atual.
  3. Banco: o pool fecha **depois** que ninguém mais o usa (HTTP e worker
     dependem do `Store`, então o Fx os para antes).
- `HTTP_REQUEST_TIMEOUT < SHUTDOWN_TIMEOUT` é validado na configuração, para
  que uma requisição em andamento sempre caiba no prazo de desligamento.
- `docker-compose` usa `stop_grace_period: 30s` (maior que `SHUTDOWN_TIMEOUT`).

Testes (`fxapp_integration_test.go`): `fx.ValidateApp` confere o grafo;
`fxtest` sobe a aplicação com banco real, usa a API, desliga e confirma que a
porta foi liberada e o pool fechado; outro teste confirma que sem banco a
aplicação não inicia.

---

## Contrato HTTP

Respostas de erro têm sempre o formato:

```json
{"error": {"code": "INVALID_REQUEST", "message": "...", "field": "initialBalance.amount"}}
```

| Situação | Status | `error.code` |
|---|---|---|
| JSON malformado, campo desconhecido, tipo errado, valor fora do formato, UUID inválido, cursor inválido | `400` | `INVALID_REQUEST` |
| carteira inexistente | `404` | `NOT_FOUND` |
| carteira já existe para jogador e moeda | `409` | `WALLET_ALREADY_EXISTS` |
| indisponibilidade transitória (banco, lock, timeout) | `503` + `Retry-After: 1` | `TEMPORARILY_UNAVAILABLE` |
| erro inesperado (detalhes só no log) | `500` | `INTERNAL_ERROR` |

| chave de idempotência reutilizada com outro conteúdo | `409` | `IDEMPOTENCY_KEY_REUSED` |
| `(providerId, externalTransactionId)` já registrado com outra chave | `409` | `DUPLICATE_TRANSACTION` |
| transação inexistente | `404` | `NOT_FOUND` |

Desfechos de `POST /wagering/transactions` (corpo `submitResponse`, sempre com
`transactionId`, `status` e `idempotentReplay`):

| Desfecho | Status | Corpo |
|---|---|---|
| processada (nova) | `201` | `balance` = saldo após a operação |
| processada (replay) | `200` | o mesmo `balance` do processamento original |
| aguardando referência | `202` | `status: PENDING_REFERENCE`, `nextAttemptAt` |
| rejeição de negócio (nova ou replay) | `422` | `status: REJECTED`, `failureCode`, `failureCorrectable` |
| falha permanente registrada | `422` | `status: FAILED`, `failureCode: PERMANENT_PROCESSING_ERROR` |

Assim as situações são distinguíveis pelo contrato: entrada inválida (`400`),
conflito (`409`), rejeição de negócio (`422`), pendente (`202`) e
indisponibilidade transitória (`503`).

| Endpoint | Sucesso |
|---|---|
| `POST /wallets` | `201` + `Location`; corpo com `id`, `playerId`, `balance`, `version` |
| `GET /wallets/{walletId}` | `200` |
| `GET /wallets/{walletId}/ledger?cursor=&limit=` | `200` `{walletId, items[], nextCursor}`; `limit` de 1 a 200 (padrão 50) |
| `POST /wagering/transactions` (header `Idempotency-Key` obrigatório) | ver tabela de desfechos |
| `GET /wagering/transactions/{transactionId}` | `200` com estado, resultado, tentativas e referência |
| `GET /providers/{providerId}/wagering/transactions/{externalTransactionId}` | `200` (mesma visão) |
| `GET /health/live` | `200` sempre que o processo responde |
| `GET /health/ready` | `200` com Postgres ok; `503` se indisponível ou desligando |

- **Paginação:** ordem crescente de `walletVersion` (estável: lançamentos novos
  só entram no fim). O cursor é opaco (`base64url("v1:<versão>")`).
- **Correlação:** `X-Correlation-Id` recebido (validado) ou gerado; volta no
  cabeçalho da resposta, vai para os logs e para o `correlationId` dos eventos.
- **Instantes:** UTC, truncados em microssegundos (a precisão do Postgres), para
  que o valor devolvido na criação seja igual ao lido depois.

Rotas de negócio exigem `Authorization: Bearer <token>`; veja
[Autenticação e autorização](#autenticação-e-autorização).

| Situação | Status | `error.code` |
|---|---|---|
| token ausente, inválido, expirado, de outro emissor ou de outra audiência | `401` + `WWW-Authenticate: Bearer` | `UNAUTHENTICATED` |
| identidade válida sem permissão (ex.: provedor agindo por outro) | `403` | `FORBIDDEN` |

---

## Idempotência

Arquivos: [`wagering/payload.go`](internal/domain/wagering/payload.go),
[`app/wagering.go`](internal/app/wagering.go),
[`postgres/repositories.go`](internal/adapters/postgres/repositories.go).

### Chave

- HTTP: header `Idempotency-Key` obrigatório (1 a 255 caracteres ASCII
  visíveis). O cliente pode usar `{providerId}:{externalTransactionId}`, mas o
  servidor **nunca** troca a chave recebida por uma calculada.
- SQS (fase 8): `data.idempotencyKey`, mais a deduplicação da inbox.

### Hash do conteúdo

`sha256-canonical-json-v1`: SHA-256 (hex) do JSON canônico dos campos de negócio.

| Entra no hash | Fica de fora |
|---|---|
| `providerId`, `externalTransactionId`, `playerId`, `walletId`, `roundId`, `gameId`, `kind`, `money.amount`, `money.currency`, `referenceExternalTransactionId` (se houver) | `Idempotency-Key`, headers, `messageId`, `occurredAt` e `type` do envelope SQS, `correlationId` |

Canonicalização: chaves em ordem alfabética (inclusive em `money`), sem
espaços, UUIDs em minúsculas, valor monetário na forma canônica validada
(`"25.00"`), referência omitida quando vazia. Não há outra normalização: como
só a forma canônica do valor é aceita na entrada, `"25.0"` é recusado com `400`
em vez de gerar um hash diferente. O mesmo cálculo é usado por HTTP e SQS,
então a mesma operação tem o mesmo hash pelas duas portas.

### Persistência e decisão

Índices únicos `(provider_id, idempotency_key)` e
`(provider_id, external_transaction_id)`. O caso de uso faz, numa única
transação:

1. `INSERT ... ON CONFLICT DO NOTHING` da operação em `PENDING`. Se outra
   requisição com a mesma chave está em andamento, o Postgres **faz esta
   esperar** o desfecho da outra, em vez de deixar as duas passarem.
2. Se não inseriu, lê a ocupante e decide:

| Situação | Resposta |
|---|---|
| mesma chave, mesmo id externo, mesmo hash | replay do resultado persistido (`idempotentReplay: true`) |
| mesma chave, conteúdo ou id externo diferente | `409 IDEMPOTENCY_KEY_REUSED` |
| mesmo `(providerId, externalTransactionId)` com outra chave | `409 DUPLICATE_TRANSACTION` (não reaplica) |

3. Se inseriu, segue o processamento e confirma estado, saldo, ledger e outbox
   no mesmo `COMMIT`. Operações sem dependência vão de `PENDING` ao desfecho
   sem commit intermediário; o `PENDING` nunca é visível isoladamente.

O replay devolve o **saldo observado no processamento original**
(`balance_after_minor` gravado na transação), mesmo que a carteira já tenha
mudado. Rejeições também são persistidas e reproduzidas.

Como tudo está no banco, a idempotência sobrevive a reinícios de todos os
processos e vale entre instâncias diferentes.

### Repetição automática

Diante de `ErrTransient` (lock, deadlock, conexão), o caso de uso tenta até 3
vezes com espera crescente. É seguro porque é idempotente: se uma tentativa
chegou a confirmar, a seguinte vira replay.

---

## Concorrência e locks

**Estratégia:** lock pessimista **por carteira** + controle otimista de versão +
invariantes no banco. Nenhum lock global; nada depende de memória local.

| Camada | Mecanismo | O que protege |
|---|---|---|
| 1 | `SELECT ... FOR UPDATE` só na linha da carteira | serializa operações da mesma carteira entre processos |
| 2 | `UPDATE ... WHERE version = nova - 1` | se a camada 1 falhar, o escritor atrasado não sobrescreve (vira `ErrTransient` e repete) |
| 3 | trigger de versão, checagem adiada saldo = ledger, `CHECK (balance >= 0)`, `UNIQUE (wallet_id, wallet_version)` | mesmo com o código Go errado, o banco recusa |
| idempotência | `INSERT ... ON CONFLICT` sobre índices únicos | duplicatas simultâneas esperam e viram replay |
| reversões | consulta sob o lock da carteira + índice único parcial | a mesma referência não é revertida duas vezes |

Ordem dos locks numa operação: primeiro a linha da própria transação (índice
de idempotência), depois a carteira. Como toda operação segue essa ordem e só
trava uma carteira, não há ciclo de espera. `lock_timeout` (3s) limita a
espera por uma carteira disputada.

Experimento registrado: sem a camada 1, os testes continuam passando (a 2
detecta e repete); sem as camadas 1 e 2, o banco ainda impede débito duplo e
saldo negativo, mas o perdedor recebe `500` em vez de uma rejeição limpa.

### Testes de concorrência (`test/integration`)

| Teste | Cenário | Resultado exigido |
|---|---|---|
| `TestSameBet50TimesInParallel` | a mesma aposta 50 vezes ao mesmo tempo | 1 criada, 49 replays, 1 débito |
| `TestTwoBetsOf80On100Concurrently` | 2 apostas de 80 sobre 100, em 15 carteiras ao mesmo tempo, e depois reenviadas | 1 processada, 1 `INSUFFICIENT_FUNDS`, saldo 20.00, 1 débito; reenvios iguais |
| `TestDifferentWalletsAreNotBlocked` | carteira A travada por outra conexão | aposta na carteira B conclui na hora |
| `TestManyWalletsManyBetsInParallel` | 200 apostas em 20 carteiras | nenhum lost update |
| `TestThreeIndependentInstances` | os cenários acima com **3 processos** do servidor (binário compilado, pools e memória próprios), requisições espalhadas entre eles | mesmos resultados; shutdown gracioso por SIGTERM |

Todos terminam com a reconciliação de todas as carteiras (saldo = créditos −
débitos do ledger) e rodam com `-race`. Foram repetidos 5 vezes seguidas sem
falha.

---

## Referências pendentes e worker

Arquivos: [`app/references.go`](internal/app/references.go),
[`worker/loop.go`](internal/worker/loop.go),
[`postgres/repositories.go`](internal/adapters/postgres/repositories.go).

### Fluxo

1. Uma operação com referência (`REFUND`, `ROLLBACK`, `WIN` com referência)
   chega antes da referência: é gravada como `PENDING_REFERENCE` com
   `next_attempt_at = agora + 1s`, o evento `WagerTransactionPendingReference`
   vai para a outbox, e o HTTP responde `202`. A pendência é **durável**: está
   no banco, não na memória de nenhuma instância.
2. Quando a referência é concluída (`BET`, `WIN` ou `REFUND` processado), a
   mesma transação SQL antecipa para "agora" o `next_attempt_at` das
   pendências que esperam por ela (`NudgePendingReferences`, com
   `SKIP LOCKED`, sem esperar por linhas travadas). Resultado medido nos
   testes: a pendência é resolvida cerca de 50 ms depois da chegada da
   referência, em vez de esperar o próximo ciclo do backoff.
3. O worker de cada instância roda `ResolveNextPending` em loop. Cada rodada é
   uma transação:
   - `SELECT ... WHERE status = 'PENDING_REFERENCE' AND next_attempt_at <= agora
     ORDER BY next_attempt_at LIMIT 1 FOR UPDATE SKIP LOCKED`;
   - trava a carteira, busca a referência e aplica **as mesmas funções do
     envio** (`evaluate` e `persist`): mesmas regras, mesma gravação de saldo,
     ledger, estado e outbox;
   - referência ainda indisponível: `attempts + 1` e reagenda com
     `min(1s × 2^n, 5min)` + até 20% de jitter; ao chegar em
     `REFERENCE_RETRY_MAX_ATTEMPTS` (padrão 12, cerca de 24 min), rejeita com
     `REFERENCE_NOT_FOUND` e emite `WagerTransactionRejected`.

### Comportamento por estado da referência

| Referência | Desfecho da pendência |
|---|---|
| não existe ainda | reagenda (ou `REFERENCE_NOT_FOUND` ao esgotar) |
| existe, `PENDING_REFERENCE` (ela também espera algo) | reagenda (ou `REFERENCE_NOT_FOUND` ao esgotar) |
| `PROCESSED` | aplica a operação |
| `REJECTED` ou `FAILED` | `REJECTED` com `REFERENCE_NOT_PROCESSED` (sem esperar a expiração) |
| já revertida por outra operação | `REJECTED` com `ALREADY_REVERSED` |
| divergente (provedor, jogador, carteira, moeda, rodada, valor, tipo) | `REJECTED` com o código correspondente |

### Concorrência e recuperação

- **Várias instâncias:** `FOR UPDATE SKIP LOCKED` faz cada worker pegar uma
  pendência diferente, sem esperar pelas que outro já está processando.
- **Exatamente uma vez:** garantido pelo banco, não pelo lock de seleção.
  Experimento registrado: removendo o `FOR UPDATE SKIP LOCKED`, o teste de 3
  workers continua passando, porque o segundo worker a tentar gravar uma
  pendência já concluída é barrado pelo trigger da máquina de estados (estado
  terminal não muda) e a transação dele é desfeita. O `SKIP LOCKED` evita esse
  trabalho desperdiçado.
- **Ordem dos locks sem ciclo:** o worker trava a pendência e depois a
  carteira; o envio trava a própria operação e a carteira e só "cutuca"
  pendências com `SKIP LOCKED` (não espera). Nenhum caminho espera por uma
  pendência segurando uma carteira.
- **Queda no meio:** a transação é desfeita e a pendência continua vencida;
  outra instância (ou a mesma, ao voltar) a pega.
- **Aceite assíncrono:** operações sem dependência não têm commit intermediário
  de `PENDING` (vão direto ao desfecho). O único estado intermediário
  confirmado é `PENDING_REFERENCE`, retomável por qualquer instância.

### Loop de worker (`internal/worker`)

Genérico (será reutilizado pelo consumidor SQS e pelo publicador da outbox):
chama o trabalho de novo na hora enquanto houver itens, espera
`REFERENCE_WORKER_INTERVAL` (com jitter) quando não há, e aplica backoff
exponencial (até 30s) em erros seguidos, para não martelar um banco fora do
ar. O contexto do item não deriva do contexto de partida do Fx; só é
cancelado se o prazo de parada acabar. `Done()` permite observar o término.

### Testes (`test/integration/references_test.go`)

| Teste | Comprova |
|---|---|
| `ResolvedWhenReferenceArrives` | REFUND antes da BET; ao chegar a BET, o REFUND é concluído, saldo restaurado, replay devolve o resultado final |
| `ExpiresAsReferenceNotFound` | sem referência, após 3 tentativas: `REJECTED`/`REFERENCE_NOT_FOUND`, evento de rejeição |
| `ReferenceRejected` | referência rejeitada: `REFERENCE_NOT_PROCESSED` |
| `ResumedAfterRestart` | instância sem worker registra a pendência e cai; uma instância nova retoma do banco |
| `CompetingWorkers` | 30 pendências e 3 instâncias com worker ao mesmo tempo: cada REFUND creditado exatamente uma vez |

---

## Autenticação e autorização

Arquivos: [`adapters/auth/oidc.go`](internal/adapters/auth/oidc.go),
[`app/authz.go`](internal/app/authz.go),
[`httpapi/auth.go`](internal/adapters/httpapi/auth.go),
[`deploy/keycloak/realm-wallet.json`](deploy/keycloak/realm-wallet.json).

### Escolha do IdP

**Keycloak** (OAuth 2.0/OIDC), provisionado automaticamente pelo docker compose
com o realm `wallet` importado. Motivos: é o recomendado pelo desafio, roda
localmente sem conta externa, suporta `client_credentials` (comunicação entre
serviços, sem usuário humano), papéis de realm e claims fixas por cliente. O
serviço **não** emite tokens nem guarda senhas.

### Fluxo

1. O provedor (ou o serviço interno) obtém um access token no Keycloak com
   `client_credentials` (`client_id` + `client_secret`).
2. Envia `Authorization: Bearer <token>` em toda rota de negócio.
3. O middleware `withAuth` valida o token **localmente** (sem chamar o IdP a
   cada requisição) e coloca a identidade (`app.Principal`) no contexto.
   Falhou: `401`, e o handler nem é executado.
4. Cada handler aplica a regra de autorização da rota antes de qualquer
   leitura ou gravação.

### Validação do token (`go-oidc`)

| Verificação | Por quê |
|---|---|
| assinatura RS256 com as chaves públicas do IdP (JWKS, em cache, renovado quando surge um `kid` novo) | o token foi emitido pelo Keycloak e não foi alterado |
| `iss` = `OIDC_ISSUER` | emitido pelo realm certo |
| `aud` contém `OIDC_AUDIENCE` (`wallet-api`) | o token foi emitido **para este serviço**, não para outro sistema |
| `exp` | não expirou (tokens de 5 minutos) |
| `typ` = `Bearer` | recusa ID tokens usados como access token |

Se as chaves não puderem ser buscadas (IdP fora do ar e sem cache), a resposta
é `503` (`ErrTransient`), não `401`. O token nunca é registrado em log.

`OIDC_JWKS_URL` permite buscar as chaves por um endereço diferente do emissor:
no compose, o emissor é `http://localhost:8081/realms/wallet` (o mesmo `iss`
dos tokens obtidos de fora) e as chaves vêm de `http://keycloak:8080/...` pela
rede interna. O Keycloak usa `KC_HOSTNAME` fixo para que o `iss` não dependa
do endereço usado para pedir o token.

### Modelo de permissões

| Cliente (Keycloak) | Papel | Claim | Pode |
|---|---|---|---|
| `provider-a`, `provider-b` | `provider` | `provider_id` (mapper fixo por cliente) | enviar operações **em nome próprio**; consultar as próprias |
| `wallet-service` | `wallet-admin` | — | abrir/consultar carteiras e ledger; consultar qualquer transação |

| Rota | Regra (`app.Principal`) | Recusa |
|---|---|---|
| `POST /wallets`, `GET /wallets/{id}`, `GET /wallets/{id}/ledger` | `CanManageWallets`: papel `wallet-admin` | `403` |
| `POST /wagering/transactions` | `CanSubmitFor`: papel `provider` **e** `providerId` do corpo = `provider_id` do token | `403`, antes de qualquer gravação |
| `GET /wagering/transactions/{id}` | `CanReadTransaction`: `wallet-admin`, ou o provedor dono | `404` para outro provedor (não revela que existe) |
| `GET /providers/{p}/wagering/transactions/{ext}` | `CanQueryProvider`: `wallet-admin`, ou `p` = `provider_id` | `403` |
| `/health/*` | pública | — |

- **O provedor vem da identidade.** O corpo ainda traz `providerId` (contrato
  do desafio), mas ele só é aceito se for igual ao do token; o servidor nunca
  "corrige" um pelo outro.
- **Replays isolados.** A idempotência é por `(providerId, chave)`. Como o
  `providerId` precisa ser o do token, o provedor B não consegue reenviar a
  chave do provedor A para ler o resultado de A: recebe `403` sem corpo
  financeiro.
- **O serviço interno não envia operações de provedor:** não tem
  `provider_id`, então não pode decidir em nome de um provedor.

### Testes (Keycloak real)

`test/integration/auth_test.go` obtém tokens reais por `client_credentials`
(`internal/testsupport/idptest`) e cobre:

| Cenário | Esperado |
|---|---|
| sem token, lixo, payload adulterado (`provider_id` trocado), `alg: none`, assinado por chave de terceiro, expirado (cliente de teste com token de 1 s), de outra audiência (cliente de teste sem o mapper `wallet-api`) | `401` em todas as rotas de negócio, nenhum dado exposto e nenhum efeito financeiro (contagem de carteiras, transações, ledger, outbox e soma de saldos inalterada) |
| B envia o corpo e a chave de A; A envia em nome de B; interno envia operação | `403`, sem efeitos |
| B lê transação de A por id / pelo namespace de A | `404` / `403` |
| provedor tenta abrir/ler carteira ou ledger | `403` |
| mesmo id externo no namespace de B | operação nova de B, sem receber o resultado de A |

Verificação adversarial registrada: desligando a checagem de audiência, um
token emitido para outro sistema conseguiu fazer uma aposta (`201`) e o teste
falhou; removendo a comparação `providerId` do corpo × token, o teste de
isolamento falhou. Os dois clientes `test-*` do realm existem só para esses
testes.

Mensageria (SQS): credenciais e políticas do broker, com as validações de
domínio mantidas no consumidor, na fase 8.
