# Arquitetura

Registro das decisões técnicas. Cada seção é preenchida conforme a fase
correspondente é implementada (ver o roteiro em
[docs/GUIA-DO-DESAFIO.md](docs/GUIA-DO-DESAFIO.md#parte-13-roteiro-de-construção-ordem-sugerida)).

| Seção | Status |
|---|---|
| [Organização dos pacotes](#organização-dos-pacotes) | ✅ |
| [Dinheiro (`Money`)](#dinheiro-money) | ✅ |
| [Carteira, ledger e transações](#carteira-ledger-e-transações) | ✅ |
| [Banco, migrations e invariantes no schema](#banco-migrations-e-invariantes-no-schema) | ✅ (transação SQL entre repositórios: fase 4) |
| Idempotência | ⏳ fase 5 |
| Concorrência e locks | ⏳ fase 5 |
| Reversões e referências pendentes | 🟡 regras de domínio na fase 2; worker e banco na fase 6 |
| Autenticação e autorização | 🟡 Keycloak provisionado na fase 3; validação na fase 7 |
| Inbox, SQS e DLQ | ⏳ fase 8 |
| Outbox | ⏳ fase 9 |
| Uber Fx e shutdown | ⏳ fase 4+ |
| Observabilidade | ⏳ fase 10 |

---

## Organização dos pacotes

```
internal/domain/   regras de negócio puras: só stdlib, sem Fx, HTTP, SQS ou pgx
internal/app/      casos de uso (orquestram domínio + interfaces de persistência)
internal/adapters/ postgres, http, sqs, auth, observabilidade
cmd/server/        composição com Uber Fx
```

O domínio não importa nenhuma biblioteca de infraestrutura. Isso permite
testá-lo sem banco e trocar adaptadores sem tocar nas regras.

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
