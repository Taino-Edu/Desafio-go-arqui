# Arquitetura

Registro das decisões técnicas do serviço de carteiras. As três últimas seções
reúnem o que o enunciado pede explicitamente:
[interpretações adotadas](#interpretações-adotadas),
[limitações](#limitações) e [trabalho não concluído](#trabalho-não-concluído).
Para executar, veja o [README](README.md).

## Visão geral

```
  provedor ── HTTP + Bearer JWT ──► API HTTP ─────┐
  provedor ── SQS (FIFO) ─────────► consumidor ───┤
                                                  ▼
                                   WagerService (o mesmo caso de uso)
                                                  │  UMA transação SQL
                                                  ▼
          PostgreSQL: operações (idempotência, estado) · carteiras (saldo,
          versão, FOR UPDATE) · ledger append-only · inbox · outbox
                    ▲                                   │
     worker de referências pendentes          publicador da outbox
     (conclui o que esperava referência)                │
                                                        ▼
                                              wallet-events (SQS FIFO)
```

- **Uma operação = uma transação SQL.** Estado da operação, saldo, ledger,
  inbox e eventos são gravados no mesmo `COMMIT`, ou nada é gravado.
- **O banco é a fonte de coordenação.** Idempotência, locks, pendências,
  inbox e outbox estão no Postgres. Qualquer número de instâncias pode rodar
  ao mesmo tempo, e qualquer uma pode cair a qualquer momento.
- **HTTP e SQS são duas portas para o mesmo caso de uso**, com as mesmas
  garantias.
- **Camadas:** `domain` (regras puras) ← `app` (casos de uso e portas) ←
  `adapters` (Postgres, HTTP, OIDC, SQS, Prometheus), compostos pelo Uber Fx.

Índice:

| Tema | Seção |
|---|---|
| pacotes e dependências | [Organização dos pacotes](#organização-dos-pacotes) |
| dinheiro | [Dinheiro (`Money`)](#dinheiro-money) |
| domínio, estados, reversões | [Carteira, ledger e transações](#carteira-ledger-e-transações) |
| schema e invariantes no banco | [Banco, migrations e invariantes no schema](#banco-migrations-e-invariantes-no-schema) |
| transações SQL | [Transação SQL entre repositórios](#transação-sql-entre-repositórios) |
| ciclo de vida e shutdown | [Uber Fx e shutdown](#uber-fx-ciclo-de-vida-e-shutdown) |
| API | [Contrato HTTP](#contrato-http) |
| idempotência | [Idempotência](#idempotência) |
| locks | [Concorrência e locks](#concorrência-e-locks) |
| referências pendentes | [Referências pendentes e worker](#referências-pendentes-e-worker) |
| autenticação e autorização | [Autenticação e autorização](#autenticação-e-autorização) |
| inbox, SQS, DLQ | [Inbox, SQS e DLQ](#inbox-sqs-e-dlq) |
| outbox | [Outbox](#outbox-publicação-de-eventos) |
| reconciliação | [Reconciliação](#reconciliação) |
| logs, métricas, health | [Observabilidade](#observabilidade-métricas-e-logs) |
| quedas e recuperação | [Caos, quedas e recuperação](#caos-quedas-e-recuperação) |
| o que foi interpretado, o que falta | [Interpretações](#interpretações-adotadas), [Limitações](#limitações), [Trabalho não concluído](#trabalho-não-concluído) |

---

## Organização dos pacotes

```
internal/domain/         regras de negócio puras: stdlib + uuid
internal/app/            casos de uso; define as portas (interfaces): persistência, publicação, métricas
internal/adapters/       implementações: postgres (pgx), httpapi (net/http), auth (OIDC),
                         sqsconsumer (SQS: consumidor e publicador), observability (Prometheus, slog)
internal/platform/       config (variáveis de ambiente) e fxapp (composição Uber Fx)
internal/worker/         loop de trabalho em segundo plano (worker de referências, publicador)
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
concorrência ficam no banco; o domínio é a primeira barreira,
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

Política (a imposição definitiva é um índice único parcial no banco):

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
| payload da outbox é snapshot | trigger `outbox_guard_update` (inclusive `seq`, migration 000002); `published_at` não volta a nulo |

Com o encadeamento e a checagem adiada, o saldo armazenado é sempre igual ao
último `balance_after` da cadeia de lançamentos, e a cadeia começa em zero.
A [reconciliação](#reconciliação) confere isso de fora, sob demanda, e
detecta o que escapar das barreiras.

### Decisões e limitações

- `wager_transactions.wallet_id` **não tem FK**: uma operação rejeitada com
  `WALLET_NOT_FOUND` precisa ser persistida para o replay idempotente, e
  aponta para uma carteira que não existe.
- `wallet_version` foi acrescentado ao lançamento (além dos campos pedidos):
  dá a ordem estável usada pela paginação do ledger e encadeia os lançamentos.
- Os triggers rodam antes das FKs; por isso uma transação inexistente no
  lançamento é barrada pelo trigger (`check_violation`), não pela FK.
- O dono do schema (e um superusuário) ainda pode desligar triggers com
  `ALTER TABLE ... DISABLE TRIGGER`. A proteção é contra a aplicação
  (`wallet_app` não é dona das tabelas) e contra erros em operação normal; o
  que escapar disso, a reconciliação detecta. O teste de divergência faz
  exatamente esse caminho.

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
e prazo do contexto. As disputas de escrita (`40001`, `40P01`, `55P03` e a
versão desatualizada no `UPDATE`) recebem também `app.ErrConcurrencyConflict`,
para serem contadas à parte (`wallet_lock_conflicts_total`). O HTTP responde
`503` com `Retry-After`. O pool define
`statement_timeout` (5s) e `lock_timeout` (3s) por conexão, menores que o prazo
da requisição (10s), para que esperar por uma carteira disputada nunca segure
uma requisição indefinidamente.

---

## Uber Fx: ciclo de vida e shutdown

Arquivo: [`internal/platform/fxapp/fxapp.go`](internal/platform/fxapp/fxapp.go).

| Módulo (`fx.Module`) | Fornece (`fx.Provide`) | Ciclo de vida (`fx.Lifecycle`) |
|---|---|---|
| `logging` | `*slog.Logger` JSON (com o `ContextHandler`) | — |
| `observability` | `*observability.Prometheus` e as portas `app.Metrics`, `sqsconsumer.Metrics`, `httpapi.Observability`; registra o coletor de pendências | — |
| `postgres` | `*pgxpool.Pool`, `app.Store` | OnStart: ping (sem banco, não sobe). OnStop: fecha o pool |
| `app` | `Clock`, `IDGenerator`, `WalletService`, `WagerService` (com `app.WithMetrics`: o Fx ignora parâmetros variádicos, então a opção é passada explicitamente) | — |
| `http` | `Health`, handler, `*httpapi.Server` | `fx.Invoke` registra OnStart (abre a porta) e OnStop (shutdown gracioso) |
| `aws` (se consumidor ou publicador ligado) | cliente SQS compartilhado | — |
| `outbox` (se `OUTBOX_PUBLISHER_ENABLED`) | — | OnStart resolve a fila de eventos (sem fila, não sobe) e inicia o loop; OnStop termina o lote em andamento (o que não for publicado volta quando o arrendamento vencer) |
| `sqs` (se `SQS_ENABLED`) | `Checker` de readiness | OnStart resolve as URLs das filas (sem filas, não sobe) e inicia os pollers; OnStop para de receber, conclui as mensagens em andamento e libera as não iniciadas |
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
aplicação não inicia. `TestFxApp_StopReleasesWorkers` sobe tudo ligado
(worker de referências, consumidor SQS com 2 pollers e publicador da outbox),
confere pelas pilhas das goroutines que elas existem e, depois do `Stop`, que
nenhuma sobrou (nem a do pool do banco). Experimento registrado: com um
`Stop` do worker que não para o loop, o teste falha apontando as duas
goroutines vivas.

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
| `POST /wallets/{walletId}/reconciliation` | `200` com o resultado, inclusive divergente (ver [Reconciliação](#reconciliação)) |
| `GET /health/live` | `200` sempre que o processo responde |
| `GET /health/ready` | `200` com Postgres (e SQS, se ligado) ok; `503` se indisponível ou desligando. Cada verificação roda em paralelo com o próprio prazo (2s) |
| `GET /metrics` | `200`, formato de exposição do Prometheus (público, ver [Observabilidade](#observabilidade-métricas-e-logs)) |

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
- SQS: `data.idempotencyKey`, mais a deduplicação da inbox.

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
vezes com espera crescente (20ms, 80ms). É seguro porque é idempotente: se uma
tentativa chegou a confirmar, a seguinte vira replay. Cada nova tentativa
conta em `transient_retries_total`, e cada disputa de escrita em
`wallet_lock_conflicts_total`.

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
| `TestLockContention_IsCountedAndAnswered503` | outra conexão segura a carteira além do `lock_timeout` | 3 tentativas, 3 conflitos e 2 retries contados; `503` + `Retry-After`; nada gravado; a repetição do cliente processa uma vez |

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

Mensageria (SQS): ver [Inbox, SQS e DLQ](#inbox-sqs-e-dlq), seção "Controle de
acesso".

---

## Inbox, SQS e DLQ

Arquivos: [`sqsconsumer/consumer.go`](internal/adapters/sqsconsumer/consumer.go),
[`sqsconsumer/envelope.go`](internal/adapters/sqsconsumer/envelope.go),
[`app/queue.go`](internal/app/queue.go),
[`deploy/localstack/init-sqs.sh`](deploy/localstack/init-sqs.sh).

### Filas

| Fila | Tipo | Configuração |
|---|---|---|
| `wager-transactions.fifo` | entrada | FIFO, `ContentBasedDeduplication=false`, visibilidade 30s, retenção 4 dias, redrive para a DLQ com `maxReceiveCount=5` |
| `wager-transactions-dlq.fifo` | DLQ da entrada | FIFO, retenção 14 dias |
| `wallet-events.fifo` / `-dlq.fifo` | saída (outbox) | idem |

**Contrato do produtor:**

- `MessageGroupId = walletId`: o SQS FIFO entrega em ordem dentro de um grupo
  e não entrega a próxima mensagem do grupo enquanto a anterior está em
  processamento; carteiras diferentes andam em paralelo.
- `MessageDeduplicationId = messageId` do envelope: o SQS descarta reenvios
  do produtor dentro de 5 minutos. **A correção não depende disso**: a inbox e
  a idempotência no banco cobrem reenvios fora da janela ou com outro id.

### Mesmo caso de uso do HTTP

O consumidor decodifica o envelope e chama `WagerService.HandleQueueMessage`,
que usa o mesmo núcleo do HTTP (`submitInTx`). Numa **única transação SQL**:

1. `INSERT` em `inbox_messages (consumer_name, message_id, payload_hash)` com
   `ON CONFLICT DO NOTHING` (outro processo com a mesma mensagem: espera);
2. se a inbox já concluiu essa mensagem: reentrega, nada é refeito
   (mesmo `messageId` com outro hash: `ErrInboxConflict`, DLQ);
3. processa a operação com a idempotência por `(providerId, data.idempotencyKey)`,
   exatamente como no HTTP (o hash de negócio é o mesmo);
4. marca a inbox como concluída;
5. `COMMIT`. **Só então** o consumidor apaga a mensagem da fila.

O hash da inbox é `sha256(hash de negócio | idempotencyKey)`. O
`correlationId` dos eventos é o `messageId`.

São duas camadas de deduplicação: a inbox (mesma mensagem) e a idempotência
(mesma operação, em qualquer mensagem ou porta). Por isso a mesma operação
enviada por HTTP e por SQS, em qualquer ordem ou ao mesmo tempo, gera um único
débito.

### Desfechos

| Situação | Ação |
|---|---|
| processada, rejeição de negócio confirmada, ou duplicata (inbox/replay) | apaga a mensagem |
| envelope inválido (JSON, campo desconhecido, valor como número, escala, UUID, `type` desconhecido, `occurredAt`) | DLQ imediata com `failure-reason`; nada é gravado |
| validação de domínio (`OPENING`, `LOSS` ≠ 0.00, referência obrigatória...) | DLQ imediata |
| provedor fora de `SQS_ALLOWED_PROVIDERS` | DLQ imediata (`forbidden`) |
| conflito de idempotência ou de inbox | DLQ imediata |
| falha transitória (banco, lock, timeout) ou erro desconhecido | `ChangeMessageVisibility` com backoff `min(1s × 2^(n−1), 60s)` (n = `ApproximateReceiveCount`); depois de 5 recebimentos, o **redrive** do SQS move para a DLQ |

Mensagens para a DLQ levam o corpo original e os atributos `failure-reason` e
`source-queue`; o envio usa o `MessageId` original como deduplicação. Se o
envio para a DLQ falhar, a mensagem não é apagada (o redrive resolve).

### Recebimento e parada

- `SQS_POLLERS` goroutines (padrão 2) fazem long polling
  (`WaitTimeSeconds` = `SQS_WAIT_TIME`, padrão 10s) com até 10 mensagens; cada
  lote é tratado em ordem.
- **SIGTERM:** o Fx chama `Stop`: os long polls são cancelados na hora, a
  mensagem em andamento termina (prazo `SHUTDOWN_TIMEOUT`), as do lote que
  ainda não começaram voltam com visibilidade 0. Se o prazo acabar, o
  tratamento é cancelado (a transação é desfeita) e a mensagem é liberada.
  O consumidor para antes do pool do banco fechar.
- Se o processo morrer sem aviso, a mensagem reaparece depois do
  `VisibilityTimeout` (30s) e é deduplicada pela inbox.

### Controle de acesso

- **Credenciais:** o consumidor usa a cadeia padrão da AWS (papel IAM da
  tarefa em produção). `SQS_ACCESS_KEY_ID`/`SQS_SECRET_ACCESS_KEY` existem só
  para o LocalStack.
- **Política da fila** ([`wager-transactions-policy.json`](deploy/localstack/wager-transactions-policy.json)):
  só os papéis dos provedores podem `SendMessage`; só o papel do serviço pode
  receber, apagar e alterar visibilidade. É aplicada pelo script de
  inicialização. **Limitação:** o LocalStack Community guarda a política mas
  não a aplica (a aplicação de IAM é recurso pago); na AWS ela vale.
- **Validações de domínio no consumidor**, independentes do broker: envelope
  estrito, regras do domínio, e `SQS_ALLOWED_PROVIDERS` (lista de provedores
  aceitos nesta fila). Uma mensagem não traz token: o provedor é o do corpo,
  e a confiança vem da política da fila.

### Testes (`test/integration/sqs_test.go`, LocalStack real)

| Teste | Comprova |
|---|---|
| `ProcessesAndDeletesAfterCommit` | processa, conclui a inbox, apaga; readiness inclui `sqs` |
| `DuplicatesAreDeduplicated` | mesmo `messageId` reenviado e outra mensagem com a mesma operação: 1 débito |
| `SameOperationViaHTTPAndSQS` | HTTP→SQS, SQS→HTTP e simultâneo: 1 débito por operação |
| `BusinessRejectionIsTerminal` | `INSUFFICIENT_FUNDS` persistido, mensagem apagada, DLQ vazia |
| `InvalidMessagesGoToDLQ` | JSON quebrado, `LOSS` 1.00, `OPENING`, provedor desconhecido: DLQ com motivo, nada gravado |
| `SameMessageIDWithDifferentContent` | conflito de inbox vai para a DLQ |
| `CrashAfterCommitBeforeDelete` | **teste obrigatório 5**: queda depois do commit e antes de apagar; a reentrega é reconhecida pela inbox; 1 débito |
| `TransientFailuresEndInDLQ` | backoff de visibilidade e redrive para a DLQ após `maxReceiveCount` |
| `GracefulShutdown` | mensagem em andamento conclui no Stop; long poll ocioso para em < 1s |

Cada teste cria filas próprias no LocalStack. A queda é simulada com um gancho
(`Hooks.AfterCommit`) que faz o consumidor parar sem apagar nem alterar a
visibilidade, como um processo derrubado nesse instante.

---

## Outbox: publicação de eventos

Arquivos: [`app/outbox_publisher.go`](internal/app/outbox_publisher.go),
[`postgres/outbox.go`](internal/adapters/postgres/outbox.go),
[`sqsconsumer/publisher.go`](internal/adapters/sqsconsumer/publisher.go),
[`migrations/000002_outbox_sequence.up.sql`](migrations/000002_outbox_sequence.up.sql).

### Por que outbox

Gravar no banco e publicar numa fila são duas operações em dois sistemas, sem
atomicidade entre eles ("dual write"). Publicar antes do commit pode anunciar
algo que não aconteceu; publicar depois pode perder o evento se o processo
cair no meio. Com a outbox, o evento é gravado **na mesma transação** do saldo,
do ledger e do estado da operação, e um worker separado publica depois. Como a
outbox só contém linhas de transações confirmadas, nada é publicado antes do
commit, e nada confirmado se perde.

### Ciclo de um evento

```
transação de negócio  ── INSERT outbox_events (seq, payload, next_attempt_at = occurred_at)  ── COMMIT
publicador            ── Claim (transação curta, confirmada): attempts+1, locked_by, locked_until = agora + lease
                      ── SendMessage para wallet-events.fifo   (fora de transação)
                      ── sucesso: published_at = agora, libera o arrendamento
                      ── falha:   next_attempt_at = agora + backoff, last_error, libera o arrendamento
```

### Reivindicação (`Claim`)

- **Ordem por agregado:** só o evento pendente de menor `seq` de cada agregado
  (a "cabeça") pode ser reivindicado; o seguinte espera a cabeça ser
  publicada. `seq` é uma coluna identidade atribuída no `INSERT` (migration
  000002); como os eventos de uma carteira são gravados com a carteira
  travada, `seq` segue a ordem das versões da carteira sem depender do relógio
  das instâncias.
- **Vários publicadores:** `FOR UPDATE SKIP LOCKED` faz cada instância pegar
  cabeças diferentes, sem esperar.
- **Arrendamento (lease, `OUTBOX_LEASE`, padrão 30s):** a reserva é gravada e
  confirmada antes da publicação. Enquanto vale, ninguém mais pega o evento;
  se a instância cair, o arrendamento vence e outra instância o retoma
  (trabalho abandonado).
- **Backoff:** `min(OUTBOX_RETRY_BASE_DELAY × 2^(tentativas−1), OUTBOX_RETRY_MAX_DELAY)`
  (1s até 5min). Não há limite de tentativas nem estado "morto": um evento
  confirmado nunca é descartado. Uma cabeça que falha segura os eventos
  seguintes do mesmo agregado (head-of-line), o que preserva a ordem; os
  outros agregados seguem.

### Garantia de entrega

**At-least-once, com `eventId` estável.** Se o processo cair entre publicar e
marcar `published_at`, outra instância republica o mesmo registro, com o
mesmo `eventId` e o mesmo payload (a linha é imutável). O SQS FIFO descarta a
cópia dentro de 5 minutos (`MessageDeduplicationId = eventId`); fora dessa
janela, o consumidor deduplica por `eventId`.

### Contrato de roteamento e consumo (`wallet-events.fifo`)

| Item | Valor |
|---|---|
| corpo | envelope JSON do evento (`eventId`, `eventType`, `aggregateId`, `correlationId`, `causationId`, `occurredAt`, `version`, `data`); a coluna é `JSONB`, que normaliza espaços e ordem das chaves |
| `MessageGroupId` | `aggregateId`: ordem por carteira (`WalletBalanceChanged`) e por transação |
| `MessageDeduplicationId` | `eventId` |
| atributos | `eventType`, `eventVersion`, `aggregateType`, `correlationId` (filtro sem abrir o corpo) |
| DLQ | `wallet-events-dlq.fifo` (`maxReceiveCount=5`) para consumidores que falharem |

Regras para quem consome: deduplicar por `eventId`; tratar `walletVersion` de
`WalletBalanceChanged` como sequência por carteira (lacuna = evento ainda por
vir ou perdido no consumidor); valores monetários são strings decimais.

### Testes (`test/integration/outbox_test.go`, LocalStack real)

| Teste | Comprova |
|---|---|
| `PublishesCommittedEvents` | abertura + aposta geram 4 eventos com o contrato de roteamento; linhas marcadas e liberadas |
| `UncommittedEventsAreNotPublished` | evento de transação aberta não é visto; depois do commit, é publicado |
| `CompetingPublishersKeepOrderPerAggregate` | 3 publicadores, 90 eventos de 5 carteiras: cada evento enviado uma vez e `walletVersion` entregue em ordem (1..9) por carteira |
| `RecoversAbandonedClaim` | **queda entre o commit e a publicação**: a reivindicação abandonada é retomada por outra instância após o arrendamento |
| `CrashBetweenPublishAndMarkRepublishesSameEventID` | **queda entre a publicação e a confirmação**: outra instância republica o mesmo `eventId`; a fila tem uma cópia |
| `FailureBacksOffAndKeepsOrder` | falha registra `last_error` e agenda backoff; o segundo evento do agregado só sai depois do primeiro |

Verificação adversarial registrada: removendo a regra da "cabeça" do
agregado, os eventos de uma carteira chegaram na ordem `[8 9 1 2 3 4 5 6 7]`
e os dois testes de ordem falharam.

---

## Reconciliação

Arquivos: [`app/reconcile.go`](internal/app/reconcile.go),
[`postgres/store.go`](internal/adapters/postgres/store.go) (`ReadSnapshot`),
[`httpapi/wallets.go`](internal/adapters/httpapi/wallets.go).

`POST /wallets/{walletId}/reconciliation`, só para `wallet-admin`:

```json
{
  "walletId": "0192f291-27dd-7d3f-8071-5f8685deef37",
  "storedBalance": { "amount": "975.00", "currency": "BRL" },
  "calculatedBalance": { "amount": "975.00", "currency": "BRL" },
  "difference": { "amount": "0.00", "currency": "BRL" },
  "consistent": true,
  "checkedEntries": 2
}
```

- **Reconstrução:** `calculatedBalance = Σ créditos − Σ débitos` de **todos**
  os lançamentos da carteira, abertura incluída (`SUM` com `FILTER` por
  direção, num único `SELECT`). `difference = storedBalance − calculatedBalance`:
  positiva quando a carteira tem dinheiro que o ledger não explica, negativa
  quando falta. A aritmética usa `Money`: um estouro vira erro, nunca um número
  errado.
- **Visão consistente:** saldo e soma são lidos na **mesma foto** do banco:
  transação `REPEATABLE READ` **somente leitura** (`Store.ReadSnapshot`). Em
  `READ COMMITTED`, cada `SELECT` vê os dados confirmados até ele, e uma
  aposta confirmada entre as duas leituras gera divergência falsa.
- **Nada é alterado.** A transação é somente leitura e termina em `ROLLBACK`.
  Divergência não é corrigida automaticamente: a correção é decisão humana,
  com um lançamento novo (o ledger nunca é editado).
- **Divergência é resultado, não erro:** a resposta é `200` com
  `consistent: false`, e ela também vai para um log `ERROR`
  (`reconciliation mismatch`, com `walletId`, `difference` e
  `checkedEntries`, sem os saldos) e para a métrica
  `reconciliation_mismatch_total`.
- Carteira inexistente: `404`. Provedor: `403`.

### Testes (`test/integration/observability_test.go`)

| Teste | Comprova |
|---|---|
| `ConsistentWallet` | o exemplo do enunciado (abertura 1000.00 + aposta 25.00 → 975.00, 2 lançamentos); `403` para provedor, `401` sem token, `404`, `400`; carteira aberta com zero tem 0 lançamentos |
| `ReportsDivergenceWithoutChangingAnything` | o dono da tabela desliga a checagem adiada e soma 10.00 ao saldo sem lançamento: resposta `difference: 10.00`, `consistent: false`; log `ERROR` com `walletId`, `difference` e `correlationId`; métrica `1`; saldo, versão, `updated_at` e ledger idênticos depois |
| `UnderLoadHasNoFalseAlarms` | 200 apostas concorrentes e ~1500 reconciliações ao mesmo tempo: nenhuma divergência falsa |

Experimento registrado: trocando `REPEATABLE READ` por `READ COMMITTED` na
leitura, 91 de 1530 reconciliações sob carga acusaram divergência falsa
(saldo 10000.00 × ledger 9999.00: uma aposta confirmou entre as duas
leituras).

---

## Observabilidade: métricas e logs

Arquivos: [`app/metrics.go`](internal/app/metrics.go) (porta),
[`adapters/observability`](internal/adapters/observability) (Prometheus e
slog), [`httpapi/observability.go`](internal/adapters/httpapi/observability.go).

### Portas e adaptador

Os casos de uso recebem a interface `app.Metrics` (com `NopMetrics` como
padrão) pela opção `app.WithMetrics`; o consumidor SQS recebe
`sqsconsumer.Metrics`; o HTTP recebe `httpapi.RequestObserver`. Nenhum deles
importa o Prometheus: só `internal/adapters/observability` conhece a
biblioteca, e um mesmo objeto implementa as três portas. O registry é
**próprio** (nada global), então cada instância, inclusive as dos testes, tem
as suas métricas.

### Métricas (`GET /metrics`)

| Métrica | Tipo | Rótulos | O que mede |
|---|---|---|---|
| `wager_transactions_total` | counter | `source`, `kind`, `status` | operações **novas** com desfecho gravado (`source`: `http`, `sqs`, `worker`) |
| `idempotent_replays_total` | counter | `source` | duplicatas respondidas com o resultado salvo |
| `idempotency_conflicts_total` | counter | `source`, `reason` | `key_reused` ou `duplicate_transaction` (`409`) |
| `wager_processing_duration_seconds` | histogram | `source` | latência do caso de uso, com esperas por lock e retries (1ms a 10s) |
| `transient_retries_total` | counter | `source` | novas tentativas automáticas |
| `wallet_lock_conflicts_total` | counter | `source` | disputas de escrita (lock timeout, deadlock, serialização, versão) |
| `sqs_messages_total` | counter | `outcome` | `processed`, `duplicate` (inbox), `retry`, `dlq`, `released` (desligamento) |
| `sqs_queue_messages` | gauge | `queue` | mensagens visíveis na fila de entrada e na DLQ, lidas do SQS na coleta (inclui o que a redrive policy move para a DLQ) |
| `outbox_events_total` | counter | `result` | publicações `published` e `failed` |
| `outbox_pending_events` | gauge | — | eventos ainda não publicados |
| `outbox_lag_seconds` | gauge | — | **atraso da outbox**: idade do evento não publicado mais antigo |
| `wager_pending_references` | gauge | — | operações em `PENDING_REFERENCE` |
| `reference_resolutions_total` | counter | `outcome` | rodadas do worker: `RESOLVED`, `RESCHEDULED`, `EXPIRED` |
| `reconciliations_total` | counter | `result` | `consistent`, `divergent` |
| `reconciliation_mismatch_total` | counter | — | divergências encontradas |
| `http_requests_total` | counter | `method`, `route`, `status` | requisições HTTP |
| `http_request_duration_seconds` | histogram | `method`, `route` | latência HTTP |
| `go_*`, `process_*` | — | — | runtime Go e processo (memória, goroutines, CPU, descritores) |

Decisões:

- **Cardinalidade controlada.** Nenhum id vira rótulo. A rota é o **padrão**
  do mux (`/wallets/{walletId}`), lido de `r.Pattern`; requisição sem rota é
  `unmatched`; método fora da lista vira `OTHER`. Um rótulo por carteira
  criaria uma série nova a cada carteira.
- **Gauges lidos na hora da coleta.** Pendências da outbox, atraso e
  referências pendentes são consultados no banco a cada coleta (índices
  parciais, prazo de 2s). O valor é o do banco, igual em qualquer instância,
  e não um contador local que cada instância teria diferente. No máximo duas
  coletas simultâneas.
- **Coleta que falha não derruba a página:** com o banco fora, `/metrics`
  continua servindo as demais métricas (`ContinueOnError`) e o erro vai para
  o log.
- **Séries de rótulos fixos começam em zero** (`reconciliation_mismatch_total`,
  `outbox_events_total`...), para que `rate()` e alertas funcionem desde a
  partida.
- **Contadores são por instância** (o Prometheus soma as instâncias com
  `sum by`); os gauges do banco são globais.
- **`/metrics` é público**, como os health checks: não expõe dados de negócio
  nem ids. Em produção, fica restrito à rede interna (ou a uma porta de
  administração) por configuração de rede, não pela aplicação.

O middleware de métricas fica **colado no mux**: o mux grava o padrão casado
no `*http.Request` que recebe, e um `r.WithContext` no meio (como o do
timeout) cria uma cópia sem o padrão. Experimento registrado: com o
middleware fora do lugar, todas as rotas viraram `unmatched` e o teste de
métricas falhou. O `withRecover` fica dentro dele, para que um panic seja
medido como `500`.

### Logs

- JSON (`slog.NewJSONHandler`) em stdout, com `service` e `instance` em toda
  linha.
- O `ContextHandler` acrescenta o `correlationId` do contexto a todo log
  feito com ele (`InfoContext`, `ErrorContext`...), sem depender de cada
  chamada lembrar de incluí-lo, e sem chave duplicada.
- Identificadores por fluxo: HTTP com `correlationId` (do
  `X-Correlation-Id` ou gerado), `transactionId`, `walletId` e `providerId`;
  SQS com `messageId`, `sqsMessageId`, `providerId`, `walletId` e
  `transactionId` (o `correlationId` da mensagem é o `messageId`); worker com
  `transactionId`, `walletId` e `providerId`.
- O worker conclui operações sem requisição de origem: os eventos dessa
  conclusão levam o `transactionId` como `correlationId`, o que os liga ao
  evento de pendência (que traz o mesmo `transactionId` nos dados).
  Limitação: o `correlationId` da requisição original não é gravado na
  transação.
- **Nunca** vão para o log: token, cabeçalho `Authorization`, corpo da
  requisição ou da mensagem, valores e saldos (a divergência da
  reconciliação registra só a diferença).
- Health checks e coletas de métricas são logados em `DEBUG` (chegam a cada
  poucos segundos e afogariam os logs).

### Testes

| Teste | Comprova |
|---|---|
| `observability/prometheus_test.go` | cada método da porta vira a série esperada; séries fixas em zero; método normalizado; gauges lidos na coleta; coletor com erro não derruba a página |
| `observability/logging_test.go` | `correlationId` do contexto em todo log, inclusive em loggers derivados (`With`), sem duplicar |
| `httpapi/observability_test.go` | pela pilha completa de middlewares: rota = padrão, `unmatched` em 404/405, panic medido como 500 |
| `app/metrics_test.go` | retries e conflitos contados por tentativa (conflito ≠ banco fora); aritmética da reconciliação |
| `TestMetrics_ExposeOutcomesAndBacklog` | pela aplicação real: desfechos por status e porta, replay, conflito, latência, pendência resolvida pelo worker, atraso da outbox, rotas HTTP; nenhum id em `/metrics` |
| `TestLogs_CarryIdentifiersAndNoSecrets` | `correlationId`, `transactionId`, `walletId`, `providerId` no log; token, `Bearer` e valores ausentes |
| testes de SQS e outbox | `sqs_messages_total` por desfecho (processada, duplicata, DLQ), profundidade das filas, publicação da outbox com atraso zero no fim |

---

## Caos, quedas e recuperação

Arquivos: [`test/integration/chaos_test.go`](test/integration/chaos_test.go),
[`test/integration/crash_test.go`](test/integration/crash_test.go),
[`internal/testsupport/chaostest`](internal/testsupport/chaostest).

O enunciado pede que nenhuma destas situações gere movimentação duplicada,
saldo negativo ou perda de evento confirmado: entrega repetida (HTTP e SQS),
reversão antes da referência, concorrência na mesma carteira, **encerramento
abrupto antes ou depois do commit**, publicação repetida e
**indisponibilidade temporária do PostgreSQL ou do SQS**.

### Ferramentas

- **Processos reais:** `launchInstance` compila `cmd/server` e sobe
  processos independentes; `kill` envia `SIGKILL` (sem shutdown gracioso,
  como queda de máquina ou OOM). As transações abertas são desfeitas pelo
  Postgres, as mensagens em mãos voltam à fila quando a visibilidade vence e
  as reivindicações da outbox voltam quando o arrendamento vence.
- **Dependência fora do ar:** `chaostest.Proxy` é um encaminhador TCP entre a
  aplicação e o Postgres (ou o LocalStack). `Cut` derruba as conexões abertas
  e recusa as novas; `Restore` religa. A dependência real continua de pé para
  o teste conferir o estado.
- **Cliente realista:** o `cluster` dos testes repete em outra instância,
  com a **mesma** chave de idempotência, quando a conexão cai ou volta `503`,
  como um cliente atrás de um balanceador.

### Cenários

| Teste | Falha provocada | O que precisa sobreviver |
|---|---|---|
| `TestChaos_PostgresOutage` | conexões com o banco cortadas | `503` + `Retry-After` sem nada gravado; readiness `503` e liveness `200`; `/metrics` responde; com o banco de volta, a mesma requisição é processada uma vez e o replay devolve `200` |
| `TestChaos_SQSOutage` | conexões com o SQS cortadas (consumidor e publicador) | HTTP continua; eventos esperam na outbox (falhas contadas em `outbox_events_total{result="failed"}`) e a mensagem espera na fila; na volta, a mensagem é processada e **todo** evento confirmado chega à fila de eventos |
| `TestRestart_KillDashNinePreservesEverything` | `kill -9` da instância; outra sobe no mesmo banco | replay devolve o resultado original (mesmo `transactionId` e saldo); a pendência (`REFUND` antes da aposta) continua e é concluída pelo worker da nova instância; reenvio da mensagem reconhecido pela inbox; eventos não publicados antes da queda são publicados depois |
| `TestChaos_KillInstancesUnderLoad` | 3 instâncias completas (HTTP, consumidor, publicador, worker) sob carga HTTP + SQS; `kill -9` em uma a 25% da carga, uma substituta sobe, `kill -9` em outra a 60% | 108 operações aplicadas exatamente uma vez; saldos exatos (977.00 em cada carteira); débitos e créditos contados um a um; reconciliação consistente; inbox completa; DLQ vazia; todo evento confirmado publicado |

Os quatro rodaram 4 vezes seguidas com `-race` sem falha. Nas execuções do
teste sob carga, de 2 a 8 requisições por execução foram interrompidas pela
queda e repetidas pelo cliente.

### Mapa dos testes obrigatórios do enunciado

| Exigência | Testes |
|---|---|
| 1. mesma aposta 50 vezes, um débito | `TestSameBet50TimesInParallel`; 51 vezes entre 3 processos em `TestThreeIndependentInstances` |
| 2. duas apostas de 80.00 sobre 100.00 | `TestTwoBetsOf80On100Concurrently`; também entre 3 processos |
| 3. carteiras distintas em paralelo | `TestDifferentWalletsAreNotBlocked`, `TestManyWalletsManyBetsInParallel` |
| 4. três instâncias independentes | `TestThreeIndependentInstances`, `TestChaos_KillInstancesUnderLoad` |
| 5. consumidor cai entre o commit e a remoção | `TestSQS_CrashAfterCommitBeforeDelete` (ponto exato, por gancho); `TestChaos_KillInstancesUnderLoad` (aleatório, por `kill -9`) |
| 6. dois publishers na mesma outbox, recuperação | `TestOutbox_CompetingPublishersKeepOrderPerAggregate`, `TestOutbox_RecoversAbandonedClaim`, `TestOutbox_CrashBetweenPublishAndMarkRepublishesSameEventID` |
| 7. reversão antes da referência | `TestPendingReference_ResolvedWhenReferenceArrives`, `TestPendingReference_ExpiresAsReferenceNotFound` |
| 8. reinício preserva idempotência, pendências e consistência | `TestRestart_KillDashNinePreservesEverything`, `TestPendingReference_ResumedAfterRestart` |
| HTTP e SQS na mesma operação | `TestSQS_SameOperationViaHTTPAndSQS` (inclusive ao mesmo tempo) |
| indisponibilidade de PostgreSQL e SQS | `TestChaos_PostgresOutage`, `TestChaos_SQSOutage`, `TestStore_LockTimeoutIsTransient`, `TestSQS_TransientFailuresEndInDLQ` |
| composição Fx, início e encerramento, liberação dos workers | `TestFxApp_*` (inclusive `StopReleasesWorkers`), `TestSQS_GracefulShutdown`, parada por `SIGTERM` de todos os processos dos testes |

Todos terminam conferindo saldo contra créditos menos débitos do ledger.

### Bug encontrado pelo caos

`TestChaos_SQSOutage` mostrou o readiness acusando o **Postgres** como fora do
ar quando só o SQS estava. As verificações rodavam em sequência com um prazo
único de 2s, e o SQS (o SDK retenta com backoff) consumia o prazo inteiro. Um
balanceador tiraria do ar instâncias com banco saudável por causa da fila.
Correção: cada verificação roda em paralelo, com o próprio prazo. O teste
unitário `TestHealth` cobre o caso e falha sem a correção.

### Limites desses testes

São probabilísticos: o `kill -9` cai onde a carga estiver naquele instante.
Um experimento registrado mostra isso: com um cliente que troca a chave de
idempotência ao repetir, o teste continuou passando, porque nas execuções as
quedas pegaram requisições ainda não confirmadas (conexão recusada), e
repetir com outra chave era inofensivo. A janela "confirmou, mas a resposta
não chegou" é estreita. Por isso as quedas em **pontos exatos** ficam nos
testes determinísticos com ganchos (`TestSQS_CrashAfterCommitBeforeDelete`,
`TestOutbox_RecoversAbandonedClaim`,
`TestOutbox_CrashBetweenPublishAndMarkRepublishesSameEventID`), e o caos
confirma que o sistema inteiro converge sob falhas reais.

---

## Interpretações adotadas

Onde o enunciado deixa espaço, a escolha foi esta:

| Tema | Interpretação |
|---|---|
| **formato do valor** | string decimal com exatamente 2 casas (`"25.00"`); `"25"`, `"25.0"`, `"25.000"`, número JSON e negativos são `400`. Assim o texto recebido é único e o hash de idempotência não varia por formatação |
| **moedas** | `BRL`, `USD` e `EUR` (todas com 2 casas). A carteira tem uma moeda; o jogador pode ter uma carteira por moeda |
| **abertura** | saldo inicial positivo cria uma transação interna `OPENING` (`PROCESSED`, sem metadados externos) e o crédito no ledger; saldo zero cria só a carteira |
| **`LOSS`** | valor exatamente `0.00`; não movimenta saldo, não gera lançamento nem muda a versão; só registra a operação e o evento |
| **`WIN`** | referência opcional; se vier, precisa ser uma `BET` do mesmo provedor, jogador, carteira, moeda e rodada |
| **`REFUND`** | estorno **total** de uma `BET` (valor igual); estorno parcial não é aceito |
| **`ROLLBACK`** | desfaz por inteiro uma `BET`, `WIN` ou `REFUND`; `ROLLBACK` de `ROLLBACK` é recusado |
| **combinações** | cada operação recebe no máximo uma reversão bem-sucedida (`REFUND` **ou** `ROLLBACK` numa `BET`); a segunda é `REJECTED` com `ALREADY_REVERSED` |
| **validação × rejeição** | erro de formato ou regra estática é `400` e **nada** é gravado; condição que depende do estado (saldo, referência, carteira) é `REJECTED` com `failureCode`, **persistida** e devolvida igual no replay (`422`) |
| **mesmo id externo, outra chave** | `409 DUPLICATE_TRANSACTION`: a operação financeira não é reaplicada nem respondida como replay |
| **chave de idempotência** | 1 a 255 caracteres ASCII visíveis, escopo por provedor; o servidor nunca a substitui por uma calculada |
| **aceite assíncrono** | o HTTP é síncrono (`201`/`200`/`422`), exceto quando a referência ainda não existe: `202 PENDING_REFERENCE`. É esse o "aceite assíncrono" do teste obrigatório 8, coberto pela queda com pendência aberta |
| **espera pela referência** | backoff `1s × 2^n` até 5 min, 12 tentativas (cerca de 24 min); depois, `REJECTED` com `REFERENCE_NOT_FOUND`. A chegada da referência antecipa a tentativa |
| **operações de carteira** | abrir, ler, extrato e reconciliar são do serviço interno (`wallet-admin`); provedores só operam e consultam as próprias transações |
| **transação de outro provedor** | por id: `404` (não revela que existe); pelo caminho de outro provedor: `403` |
| **mensagem SQS** | não traz token: a confiança vem da política da fila (quem pode enviar) e da lista `SQS_ALLOWED_PROVIDERS`; a chave vem em `data.idempotencyKey` |
| **reconciliação** | `200` mesmo quando diverge (`consistent: false`): divergência é resultado, não erro; nunca corrige sozinha |
| **eventos** | um tipo por evento, versão `1`, publicados at-least-once com `eventId` estável; ordem garantida por agregado (carteira ou transação), não global |
| **instantes** | UTC, truncados em microssegundos (a precisão do Postgres) |
| **`/metrics`** | público como os health checks (sem ids nem valores); o isolamento fica para a rede |

---

## Limitações

- **Política da fila não aplicada no LocalStack.** O LocalStack Community
  guarda a política de acesso da fila, mas não a aplica (IAM é recurso
  pago). Na AWS ela vale; localmente, a proteção efetiva no consumidor é a
  validação de domínio e `SQS_ALLOWED_PROVIDERS`.
- **Triggers podem ser desligados pelo dono do schema.** O ledger imutável e
  a checagem saldo = ledger valem contra a aplicação (`wallet_app`) e em
  operação normal; o dono das tabelas ou um superusuário pode desligá-los.
  A reconciliação detecta o resultado.
- **Inbox e outbox crescem sem limite.** Não há rotina de retenção para
  mensagens concluídas e eventos publicados.
- **Head-of-line na outbox.** Um evento que nunca consegue ser publicado
  segura os seguintes do mesmo agregado (é o preço da ordem por carteira).
  Não há estado "morto": o sinal é `outbox_lag_seconds` subindo.
- **`correlationId` do worker.** Operações concluídas pelo worker publicam
  eventos com o `transactionId` como `correlationId`: o id da requisição
  original não é gravado na transação.
- **Carteira muito disputada.** O lock é por carteira: operações da mesma
  carteira são serializadas (por desenho). Sob disputa além do
  `lock_timeout`, a resposta é `503` com `Retry-After`, e o cliente repete com
  a mesma chave.
- **Tokens sem revogação imediata.** O JWT é validado localmente (assinatura,
  emissor, audiência, validade); um token revogado no Keycloak continua
  aceito até expirar (5 minutos).
- **Keycloak em modo de desenvolvimento** (`start-dev`, banco embutido) e
  segredos de teste no realm versionado: servem só para o ambiente local.
- **Sem limite de taxa (rate limiting)** por provedor; o corpo da requisição
  é limitado a 64 KiB.
- **Moedas com 2 casas decimais apenas.**
- **Testes de caos são probabilísticos.** O `kill -9` cai onde a carga estiver
  naquele instante; as quedas em pontos exatos ficam nos testes com ganchos
  (ver [Caos](#limites-desses-testes)).
- **Testes de integração dependem do compose** (não usam testcontainers): é
  preciso subir Postgres, Keycloak e LocalStack antes (`README`).

---

## Trabalho não concluído

Itens fora do escopo entregue, em ordem de prioridade para produção:

1. **Retenção da inbox e da outbox:** job que apaga mensagens concluídas e
   eventos publicados mais antigos que a janela de deduplicação.
2. **Gravar o `correlationId` original** na transação, para que a conclusão
   pelo worker continue o mesmo rastro.
3. **Alertas** sobre as métricas existentes (`outbox_lag_seconds`,
   `sqs_queue_messages{queue="dlq"}`, `reconciliation_mismatch_total`,
   `wallet_lock_conflicts_total`) e ferramenta para reprocessar a DLQ.
4. **Tracing com OpenTelemetry** (diferencial opcional do enunciado).
5. **Teste de carga** com throughput e p50/p95/p99 (diferencial opcional).
6. **Partidas dobradas** (diferencial opcional): hoje o ledger é de entrada
   simples por carteira.
7. **Rate limiting** por provedor e Keycloak em modo de produção.
