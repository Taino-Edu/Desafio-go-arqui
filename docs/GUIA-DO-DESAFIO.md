# Guia de estudo do desafio: processamento distribuído de apostas em Go

> Desafio original: <https://github.com/junglegaming/backend-challenge-go>
>
> Este guia explica, do zero, cada conceito de pagamentos e sistemas transacionais
> que o desafio cobra, e propõe a arquitetura que vamos construir. Ele parte do
> princípio de que você já sabe Go, mas nunca trabalhou com dinheiro, ledger ou
> mensageria.
>
> Quer a versão sem jargão, com analogias e links de vídeos? Veja
> [CONCEITOS-EXPLICADOS.md](CONCEITOS-EXPLICADOS.md).

---

## Parte 0: o problema em uma frase

Um **provedor de jogos** (o estúdio que faz o caça-níquel "fortune-chimp") avisa a
gente: "o jogador X apostou R$ 25 na rodada 987". A gente precisa **tirar R$ 25 da
carteira dele exatamente uma vez**, mesmo que:

- o aviso chegue 2, 3 ou 50 vezes (por HTTP e também pela fila SQS);
- dois avisos da mesma carteira cheguem no mesmo milissegundo, em servidores diferentes;
- o servidor morra no meio do processamento;
- o banco fique fora do ar por alguns segundos.

Tudo no desafio gira em torno de **"exatamente uma vez" num mundo em que tudo
acontece "pelo menos uma vez" ou "às vezes nunca"**.

### O fluxo de uma rodada de jogo

```
Jogador aperta "girar"
        │
        ▼
Provedor envia BET (aposta) de 25.00  ───►  nós debitamos 25.00
        │
        ├── jogador ganhou ──► provedor envia WIN de 60.00   ───► creditamos 60.00
        └── jogador perdeu ──► provedor envia LOSS de 0.00   ───► não mexe no saldo,
                                                                 só registra o fim
Se algo deu errado do lado do provedor:
        ├── REFUND   (devolve a aposta inteira)              ───► creditamos 25.00
        └── ROLLBACK (desfaz uma BET, um WIN ou um REFUND)   ───► movimento contrário
```

---

## Parte 1: conceitos de dinheiro

### 1.1 Por que dinheiro nunca é `float`

`float64` é binário e não representa `0.1` exatamente:

```go
fmt.Println(0.1 + 0.2) // 0.30000000000000004
```

Em um milhão de apostas, esses erros somam centavos que "somem" ou "aparecem". Em
pagamentos isso é **eliminatório**: o desafio diz que o dinheiro não pode passar
por float **nem no parsing do JSON**. Por isso o valor chega como **string**
(`"25.00"`) e nunca como número (`25.00`).

**Nossa escolha:** `int64` em **unidades mínimas** (centavos). `"25.00"` vira
`2500`. No Postgres a coluna é `BIGINT`.

| Representação  | Vantagem                          | Desvantagem                      |
|----------------|-----------------------------------|----------------------------------|
| `int64` centavos | rápida, sem dependência, exata  | cuidado manual com overflow      |
| lib decimal    | escala flexível                   | mais lenta, mais dependência     |

Limite do `int64`: 9.223.372.036.854.775.807 centavos, ou seja, uns 92 quatrilhões
de reais. Sobra muito, mas o desafio exige **detectar overflow** em parse, soma,
subtração e negação (ex.: `-math.MinInt64` estoura).

### 1.2 Value object `Money`

"Value object" é um tipo **imutável** definido pelo seu valor, sem identidade
própria. Dois `Money{2500, BRL}` são iguais e ponto.

```go
type Currency string // ISO 4217: "BRL", "USD"...

type Money struct {
    amount   int64    // centavos; campo privado = ninguém altera de fora
    currency Currency
}

func ParseMoney(amount string, cur Currency) (Money, error) // "25.00" → 2500
func Zero(cur Currency) Money
func (m Money) Add(o Money) (Money, error)  // erro se moedas diferentes ou overflow
func (m Money) Sub(o Money) (Money, error)
func (m Money) Neg() (Money, error)
func (m Money) Cmp(o Money) (int, error)
func (m Money) String() string              // 2500 → "25.00"
```

Regras de parsing da entrada externa (todas com teste):

| Entrada        | Resultado | Motivo                                  |
|----------------|-----------|-----------------------------------------|
| `"25.00"`      | ok        | formato canônico                        |
| `"25"`, `"25.0"` | erro    | escala tem que ser exatamente 2 casas   |
| `"25.001"`     | erro      | escala excedente; **não arredondar**    |
| `"-1.00"`      | erro      | negativo na entrada externa             |
| `"1e3"`        | erro      | notação científica                      |
| `"NaN"`, `"Infinity"`, `""` | erro | inválidos                         |
| `"0.00"`       | ok        | permitido, mas cada tipo decide se aceita zero |

Regex simples que cobre tudo: `^(0|[1-9][0-9]*)\.[0-9]{2}$`. Depois disso a gente
converte os dígitos manualmente, checando overflow.

> **Por que aceitar só a forma canônica?** Porque isso simplifica a idempotência
> (Parte 3): se `"25.00"` e `"25.0"` fossem aceitos, os dois teriam que gerar o
> mesmo hash, e seria preciso documentar essa normalização.

---

## Parte 2: carteira e ledger (o coração financeiro)

### 2.1 O que é um ledger

**Ledger** é o livro-razão: a lista de **todos** os lançamentos de dinheiro, em
ordem, que **nunca é apagada nem editada**. Funciona como um extrato bancário.

```
wallet 7f3.. | tx OPENING | CREDIT | 1000.00 | antes    0.00 | depois 1000.00
wallet 7f3.. | tx BET-123 | DEBIT  |   25.00 | antes 1000.00 | depois  975.00
wallet 7f3.. | tx WIN-124 | CREDIT |   60.00 | antes  975.00 | depois 1035.00
```

Duas regras de ouro:

1. **Append-only:** errou? Não se edita a linha. Cria-se um **novo** lançamento
   contrário (é exatamente isso que `ROLLBACK` faz).
2. **O saldo da carteira tem que bater com o ledger:**
   `saldo = Σ créditos − Σ débitos`. O endpoint de **reconciliação** prova isso.

Por que guardar o saldo na tabela `wallets` se dá para calcular pelo ledger?
Porque somar milhões de linhas a cada aposta é lento. O saldo armazenado é um
"cache" que **precisa** ser atualizado na **mesma transação SQL** que o lançamento.

#### Como o banco garante a imutabilidade

O desafio exige que isso seja imposto **pelo banco**, não só pelo código Go:

```sql
CREATE FUNCTION forbid_ledger_mutation() RETURNS trigger AS $$
BEGIN
  RAISE EXCEPTION 'wallet_ledger_entries is append-only';
END $$ LANGUAGE plpgsql;

CREATE TRIGGER ledger_no_update_delete
  BEFORE UPDATE OR DELETE ON wallet_ledger_entries
  FOR EACH ROW EXECUTE FUNCTION forbid_ledger_mutation();

-- e também: o usuário da aplicação nem tem permissão
REVOKE UPDATE, DELETE, TRUNCATE ON wallet_ledger_entries FROM app_user;
```

Mais as constraints:

```sql
UNIQUE (wallet_id, transaction_id),                      -- 1 lançamento por transação
CHECK (amount > 0),
CHECK (balance_before >= 0 AND balance_after >= 0),
CHECK ( (direction = 'CREDIT' AND balance_after = balance_before + amount)
     OR (direction = 'DEBIT'  AND balance_after = balance_before - amount) )
```

### 2.2 Carteira (Wallet) como raiz do agregado

"Agregado" é um termo de DDD: um grupo de objetos que só muda por uma porta de
entrada, a **raiz**, que protege as regras. Aqui a raiz é a `Wallet`. Ninguém mexe
no saldo diretamente, só via `wallet.Debit(money)` ou `wallet.Credit(money)`.

```go
type Wallet struct {
    id, playerID uuid.UUID
    currency     Currency
    balance      Money
    version      int64
    createdAt, updatedAt time.Time
}

// Criação: valida tudo, version = 1
func NewWallet(id, playerID uuid.UUID, initial Money, now time.Time) (*Wallet, error)

// Reidratação: reconstrói a partir do banco SEM revalidar movimentos nem gerar eventos
func RehydrateWallet(id, playerID uuid.UUID, balance Money, version int64, ...) (*Wallet, error)

// Retorna o lançamento de ledger que corresponde à mudança
func (w *Wallet) Debit(txID uuid.UUID, m Money, now time.Time) (LedgerEntry, error) // ErrInsufficientFunds
func (w *Wallet) Credit(txID uuid.UUID, m Money, now time.Time) (LedgerEntry, error)
```

Regras: `(playerId, currency)` é único, o saldo nunca fica negativo, a moeda do
movimento é igual à da carteira, e a versão só sobe quando o saldo muda (por isso
`LOSS` não incrementa a versão).

**Criação vs. reidratação:** quando você lê uma carteira do banco, ela já existe.
Chamar `NewWallet` geraria de novo o crédito de abertura e os eventos. Por isso há
dois construtores separados.

---

## Parte 3: idempotência

### 3.1 O que é

Uma operação é **idempotente** quando executá-la 1 ou 50 vezes produz o mesmo
efeito. `PUT saldo = 100` é idempotente; `saldo = saldo - 25` **não é**. O nosso
trabalho é tornar o débito idempotente.

### 3.2 Por que as mensagens chegam repetidas

O desafio manda assumir **entrega at-least-once** (pelo menos uma vez):

- o provedor mandou o HTTP, a gente processou, mas a resposta se perdeu na rede.
  O provedor não sabe se deu certo e **reenvia**;
- o SQS entregou a mensagem, processamos, mas o processo morreu antes de apagá-la
  da fila. O SQS **reentrega**;
- o mesmo provedor manda por HTTP **e** por SQS.

### 3.3 A chave e o hash

Cada operação externa vem com:

- **`Idempotency-Key`** (header HTTP ou `data.idempotencyKey` no SQS): "esta é a
  operação X";
- **payload**: o conteúdo de negócio.

Guardamos no banco a chave **e** um **hash SHA-256 do JSON canônico** do conteúdo.
JSON canônico significa chaves ordenadas, sem espaços e com valores normalizados,
para que o mesmo conteúdo gere sempre o mesmo hash, venha de HTTP ou de SQS.

```
Campos do hash: providerId, externalTransactionId, playerId, walletId, roundId,
                gameId, kind, money.amount, money.currency, referenceExternalTransactionId
Fora do hash:   idempotencyKey, messageId, occurredAt, headers, correlationId
```

| Situação                                         | Resposta                                  |
|--------------------------------------------------|-------------------------------------------|
| chave nova                                       | processa normalmente                      |
| mesma chave, mesmo hash                          | devolve o resultado **salvo**, `idempotentReplay: true` |
| mesma chave, hash diferente                      | `409 Conflict` (`IDEMPOTENCY_KEY_REUSED`) |
| outra chave, mesmo `(providerId, externalTransactionId)` | `409 Conflict`, não reaplica      |

Detalhe importante: o replay devolve o **saldo da época** (o `balance_after`
guardado na transação), não o saldo atual. Por isso salvamos o resultado
financeiro dentro da própria `wager_transactions`.

### 3.4 Idempotência persistente

Um `map[string]bool` em memória é **eliminatório**: some no restart e não é
compartilhado entre 3 instâncias. A idempotência fica em **constraints UNIQUE do
Postgres**:

```sql
UNIQUE (provider_id, idempotency_key),
UNIQUE (provider_id, external_transaction_id)
```

E o truque para 50 requisições paralelas iguais: todas fazem `INSERT ... ON
CONFLICT DO NOTHING`. O Postgres garante que **só uma** insere. As outras
**esperam** a primeira commitar (o índice único bloqueia), veem o conflito, leem a
linha existente e devolvem o replay.

---

## Parte 4: concorrência

### 4.1 O problema do lost update

Duas apostas de 80 numa carteira com 100, em dois servidores ao mesmo tempo:

```
Servidor A: lê saldo = 100        Servidor B: lê saldo = 100
Servidor A: 100 ≥ 80 ok           Servidor B: 100 ≥ 80 ok
Servidor A: grava saldo = 20      Servidor B: grava saldo = 20   ← perdeu um débito!
```

Resultado: o jogador apostou 160 com 100 de saldo. Esse é o **lost update**, e é
eliminatório. Um `sync.Mutex` não resolve, porque são **processos diferentes**,
com memórias diferentes.

### 4.2 As três estratégias

| Estratégia | Como funciona | Prós | Contras |
|---|---|---|---|
| **Pessimista** `SELECT ... FOR UPDATE` | trava a linha da carteira até o commit; o outro espera | simples, sem retry, já dá o `balanceBefore` | segura o lock durante a transação |
| **Otimista** (`version`) | `UPDATE ... WHERE id=$1 AND version=$2`; se afetou 0 linhas, alguém chegou antes, então tenta de novo | sem espera | precisa de retry limitado; muito conflito = muito retry |
| **Atômica condicional** | `UPDATE wallets SET balance = balance - $1 WHERE id=$2 AND balance >= $1` | uma query só | difícil obter `balanceBefore` e versão de forma limpa |

**Nossa escolha:** **pessimista por carteira** (`SELECT ... FOR UPDATE` só na
linha daquela carteira) mais o `version` incrementado e um `CHECK (balance >= 0)`
como **última linha de defesa** no banco. Carteiras diferentes travam linhas
diferentes, então rodam em paralelo (o desafio proíbe lock global).

Com isso, a disputa vira:

```
A: SELECT ... FOR UPDATE (pega o lock)     B: SELECT ... FOR UPDATE (espera...)
A: debita 80, saldo 20, COMMIT (solta)     B: agora lê saldo = 20
                                           B: 20 < 80, REJECTED (INSUFFICIENT_FUNDS)
```

O teste obrigatório espera exatamente isso: 1 processada, 1 rejeitada, saldo
20.00 e 1 débito no ledger.

### 4.3 A transação SQL de uma aposta (o "caminho feliz")

```sql
BEGIN;
  -- 1. idempotência: tenta registrar a operação
  INSERT INTO wager_transactions (...) VALUES (...) ON CONFLICT DO NOTHING RETURNING id;
  --    se não inseriu: lê a existente, compara o hash e devolve replay ou 409

  -- 2. trava só esta carteira
  SELECT balance, version FROM wallets WHERE id = $wallet FOR UPDATE;

  -- 3. regra de domínio em Go: wallet.Debit(...) → LedgerEntry ou ErrInsufficientFunds

  -- 4. grava tudo
  UPDATE wallets SET balance = $new, version = version + 1, updated_at = now() WHERE id = $wallet;
  INSERT INTO wallet_ledger_entries (...);
  UPDATE wager_transactions SET status = 'PROCESSED', balance_after = $new, ... WHERE id = $tx;
  INSERT INTO outbox_events (...);   -- WagerTransactionProcessed + WalletBalanceChanged
  -- (se veio do SQS) UPDATE inbox_messages SET completed_at = now() ...
COMMIT;
```

**Tudo ou nada.** Se o processo morrer em qualquer linha antes do `COMMIT`, o
Postgres desfaz tudo e a reentrega processa do zero. Se morrer **depois** do
`COMMIT`, a reentrega cai no replay. Nos dois casos, o resultado é **exatamente
uma vez**.

> Ordem dos locks: sempre insira a transação **antes** de travar a carteira, ou
> sempre na mesma ordem, para evitar deadlock. Defina um `lock_timeout` /
> `statement_timeout` e trate o erro como transitório (503 com retry).

---

## Parte 5: a máquina de estados da transação

```
                 ┌──────────────► PROCESSED   (terminal)
                 │
   PENDING ──────┼──────────────► REJECTED    (terminal, regra de negócio)
      │          │
      │          └──────────────► FAILED      (terminal, infra permanente, auditoria)
      ▼
 PENDING_REFERENCE ──(referência chegou)──► PROCESSED | REJECTED | FAILED
      │
      └──(esgotou tentativas/TTL)──► REJECTED (REFERENCE_NOT_FOUND)
```

- **Terminal** quer dizer que não sai mais desse estado. O domínio recusa qualquer
  transição (`ErrInvalidTransition`).
- **Transitória vs. permanente:** banco fora do ar, timeout ou deadlock são
  **transitórios** (tenta de novo, não muda o estado). Payload impossível de
  desserializar ou erro irrecuperável depois de N tentativas é **permanente** (`FAILED`).
- **Operações sem dependência** (BET, WIN sem referência, LOSS) podem ir de
  `PENDING` a `PROCESSED` **no mesmo commit**: o `PENDING` nem chega a ser visto
  por ninguém. Só fica `PENDING` persistido quando algo vai ser retomado depois.

### Códigos de falha (`failureCode`), estáveis e documentados

| Código | Tipo | Quando |
|---|---|---|
| `INSUFFICIENT_FUNDS` | definitivo | BET sem saldo |
| `REVERSAL_INSUFFICIENT_FUNDS` | definitivo | ROLLBACK de WIN/REFUND sem saldo (o desafio exige código **diferente** da BET) |
| `REFERENCE_NOT_FOUND` | definitivo | referência não chegou dentro do TTL |
| `REFERENCE_NOT_PROCESSED` | definitivo | referência existe, mas terminou REJECTED/FAILED |
| `REFERENCE_MISMATCH` | corrigível | provedor/jogador/carteira/moeda/rodada não batem |
| `AMOUNT_MISMATCH` | corrigível | valor da reversão diferente do original |
| `ALREADY_REVERSED` | definitivo | a referência já recebeu essa reversão |
| `CURRENCY_MISMATCH` | corrigível | moeda diferente da carteira |
| `INVALID_AMOUNT` | corrigível | zero onde não pode, ou LOSS diferente de 0.00 |
| `WALLET_NOT_FOUND` | corrigível | carteira inexistente |

---

## Parte 6: os cinco tipos de operação e as reversões

| Tipo | Efeito | Valor | Referência |
|---|---|---|---|
| `BET` | débito | > 0, precisa de saldo | não |
| `WIN` | crédito | > 0 | opcional (uma BET da mesma rodada) |
| `LOSS` | nada (sem ledger, sem versão) | **exatamente 0.00** | não |
| `REFUND` | crédito | = valor da BET | **obrigatória**, precisa ser uma BET |
| `ROLLBACK` | contrário do original | = valor do original | **obrigatória**: BET, WIN ou REFUND |

- `ROLLBACK` de `BET`: crédito (devolve a aposta).
- `ROLLBACK` de `WIN`: **débito** (tira o prêmio; pode faltar saldo, e aí vem `REVERSAL_INSUFFICIENT_FUNDS`).
- `ROLLBACK` de `REFUND`: **débito** (desfaz a devolução).

A referência é buscada por `(providerId, referenceExternalTransactionId)` e precisa
bater em provedor, jogador, carteira, moeda e rodada.

### Política de combinações (decisão nossa, que precisa estar no ARCHITECTURE.md)

O perigo é **devolver a mesma aposta duas vezes** (um REFUND e depois um ROLLBACK
da mesma BET, por exemplo). Regra proposta:

1. Uma `BET` pode receber **no máximo uma compensação**: **ou** um REFUND **ou**
   um ROLLBACK, nunca os dois.
2. Um `WIN` ou um `REFUND` pode receber no máximo **um** ROLLBACK.
3. `ROLLBACK` de `ROLLBACK` não é permitido.

Isso é imposto no banco com um **índice único parcial**:

```sql
-- no máximo uma reversão bem-sucedida apontando para cada transação original
CREATE UNIQUE INDEX one_reversal_per_reference
  ON wager_transactions (reference_transaction_id)
  WHERE kind IN ('REFUND','ROLLBACK') AND status = 'PROCESSED';
```

Exemplo: BET 25, depois REFUND 25 (ok), depois ROLLBACK do REFUND (débito de 25,
ok). Um novo REFUND da BET é bloqueado pelo índice. Financeiramente o jogador
terminou com o débito original, o que é coerente.

### Referência que ainda não chegou (`PENDING_REFERENCE`)

Mensagens fora de ordem acontecem: o `ROLLBACK` chega pelo SQS antes da `BET` chegar
pelo HTTP. Em vez de rejeitar:

1. salva o ROLLBACK como `PENDING_REFERENCE`, com `attempts = 0` e
   `next_attempt_at = now() + backoff`;
2. emite o evento `WagerTransactionPendingReference`;
3. responde `202 Accepted` (HTTP) ou apaga a mensagem do SQS (a pendência já está
   **durável** no banco, e o worker assume daqui);
4. um **worker de referências** busca as pendências vencidas com
   `SELECT ... FOR UPDATE SKIP LOCKED` (várias instâncias, cada uma pega linhas
   diferentes, sem brigar) e tenta resolver:
   - referência `PROCESSED`: aplica a reversão;
   - referência ainda `PENDING`/`PENDING_REFERENCE`: espera mais um ciclo;
   - referência `REJECTED`/`FAILED`: `REJECTED` com `REFERENCE_NOT_PROCESSED`;
   - não achou e `attempts >= max` (ex.: 10 tentativas, cerca de 30 min): `REJECTED` com `REFERENCE_NOT_FOUND`.

Backoff exponencial: `delay = min(base * 2^attempts, max) + jitter` (ex.:
1s, 2s, 4s, 8s... até 5min). O jitter (aleatoriedade) evita que todas as instâncias
tentem no mesmo instante.

---

## Parte 7: mensageria (SQS, inbox e outbox)

### 7.1 SQS em 1 minuto

O AWS SQS é uma fila gerenciada. Rodamos localmente com **LocalStack**.

- O consumidor faz `ReceiveMessage`. A mensagem fica **invisível** por
  `VisibilityTimeout` segundos (ex.: 30s).
- Se processar, chama `DeleteMessage`. Se não apagar (crash, erro), a mensagem
  **reaparece** depois do timeout. É daí que vem o at-least-once.
- Cada reentrega incrementa o `ApproximateReceiveCount`. Ao passar de
  `maxReceiveCount` (ex.: 5), o SQS move a mensagem para a **DLQ** (Dead Letter
  Queue), a "fila de mensagens problemáticas" para análise humana.
- **FIFO** (`.fifo`): garante ordem **dentro de um `MessageGroupId`**.
  - `MessageGroupId = walletId`: mensagens da mesma carteira em ordem; carteiras
    diferentes em paralelo. É a escolha natural aqui.
  - `MessageDeduplicationId = idempotencyKey`: o SQS descarta duplicatas enviadas
    numa janela de **5 minutos**. Ajuda, mas o desafio diz explicitamente que
    **não podemos depender disso**: a garantia real é o banco.

### 7.2 Inbox: deduplicação na entrada

Tabela `inbox_messages (consumer_name, message_id, payload_hash, received_at, completed_at)`,
com `UNIQUE (consumer_name, message_id)`.

Fluxo do consumidor para cada mensagem:

```
BEGIN
  INSERT INTO inbox_messages ... ON CONFLICT DO NOTHING
  se já existia e completed_at não é nulo: COMMIT, DeleteMessage (duplicata)
  se o hash é diferente: mensagem inválida, vai para a DLQ e não toca no domínio
  executa o MESMO caso de uso do HTTP (mesma transação SQL!)
  UPDATE inbox_messages SET completed_at = now()
COMMIT
DeleteMessage   ← só depois do commit
```

Se morrer entre o `COMMIT` e o `DeleteMessage`, a mensagem volta, a inbox diz "já
concluída" e a gente só apaga. Esse é o teste 5 da seção 13.

Rejeição de negócio (sem saldo) **também é sucesso** do ponto de vista da fila:
commita o `REJECTED` e apaga. Só erro **transitório** deixa a mensagem voltar.

### 7.3 Outbox: publicar eventos sem mentir

Problema clássico do **dual write**: gravar no banco e publicar no SQS são duas
operações em dois sistemas. Qualquer ordem dá errado:

- publica e depois commita: o commit falha, mas o mundo já recebeu um evento de
  algo que **não aconteceu** (eliminatório: "publicação anterior ao commit");
- commita e depois publica: o processo morre no meio e o evento **se perde**.

**Transactional outbox:** o evento é gravado numa tabela `outbox_events` **na
mesma transação** do saldo. Um **worker separado** lê essa tabela e publica.

```
outbox_events: event_id (UUID estável), aggregate_id, event_type, payload (JSONB),
               occurred_at, attempts, next_attempt_at, locked_until, published_at
```

Worker publicador (pode rodar em 3 instâncias):

```sql
-- 1. reivindica um lote (lease): ninguém mais pega essas linhas por 30s
UPDATE outbox_events SET locked_until = now() + interval '30 seconds', attempts = attempts + 1
WHERE event_id IN (
  SELECT event_id FROM outbox_events
  WHERE published_at IS NULL AND next_attempt_at <= now()
    AND (locked_until IS NULL OR locked_until < now())
  ORDER BY occurred_at
  LIMIT 50
  FOR UPDATE SKIP LOCKED
) RETURNING *;
-- 2. publica cada um no SQS/SNS (fora da transação)
-- 3. sucesso: UPDATE ... SET published_at = now()
--    falha:   UPDATE ... SET next_attempt_at = now() + backoff, locked_until = NULL
```

- Se o worker morrer **depois de reivindicar**, o `locked_until` expira e outra
  instância pega o evento ("recuperação de trabalho abandonado").
- Se morrer **entre publicar e marcar `published_at`**, o evento é publicado de
  novo **com o mesmo `eventId`**. Isso é aceitável: o consumidor do evento
  deduplica pelo `eventId`. Outbox dá at-least-once na saída, e é por isso que o
  `eventId` precisa ser estável.

Destino dos eventos: uma fila `wallet-events.fifo` (ou um tópico SNS), com
`MessageGroupId = aggregateId` e `MessageDeduplicationId = eventId`.

### 7.4 Envelope dos eventos

```json
{
  "eventId": "0192f2a0-...",
  "eventType": "WalletBalanceChanged",
  "aggregateId": "<walletId>",
  "correlationId": "<id da requisição/mensagem original>",
  "causationId": "<transactionId que causou>",
  "occurredAt": "2026-09-08T12:00:00Z",
  "version": 1,
  "data": {
    "walletId": "...", "transactionId": "...", "direction": "DEBIT",
    "money":         { "amount": "25.00",  "currency": "BRL" },
    "balanceBefore": { "amount": "1000.00", "currency": "BRL" },
    "balanceAfter":  { "amount": "975.00",  "currency": "BRL" },
    "walletVersion": 2
  }
}
```

Em Go, um struct por evento, com construtor que fixa `eventType` e `version` (o
chamador não escolhe). O payload é serializado **no momento** da gravação na
outbox (snapshot imutável).

---

## Parte 8: autenticação e autorização (OAuth 2.0 / OIDC)

### Conceitos

- **IdP** (Identity Provider): o sistema que emite tokens. Usaremos o **Keycloak**
  no Docker Compose. Nós **não** emitimos tokens nem guardamos senhas.
- **`client_credentials`**: fluxo OAuth de máquina para máquina. O provedor tem um
  `client_id` e um `client_secret`, pede um token ao Keycloak e manda
  `Authorization: Bearer <JWT>` para a nossa API.
- **JWT**: token assinado. A API valida **assinatura** (chaves públicas do
  endpoint JWKS do Keycloak), `iss` (emissor), `aud` (audiência = nossa API) e
  `exp` (expiração). Não precisa chamar o Keycloak a cada requisição.

### Modelo de permissões proposto

| Cliente Keycloak | Claims no token | Pode |
|---|---|---|
| `provider-a` | `provider_id=provider-a`, role `wagering:write` | enviar e consultar **as próprias** transações |
| `provider-b` | `provider_id=provider-b`, role `wagering:write` | idem, isolado do A |
| `wallet-service` (interno) | role `wallet:admin` | abrir carteira, ler carteira/ledger, reconciliar |

Regras:

- O `providerId` **vem do token**. Se o body disser `provider-b` e o token for do
  `provider-a`, a resposta é `403`, e **nada** é gravado.
- `GET /providers/provider-b/...` com token do A dá `403`/`404` (não vaza
  existência). O mesmo vale para replay: o A não pode "reenviar" a chave do B para
  ler o resultado.
- Endpoints de carteira só para o cliente interno.
- `/health/*` é público.
- SQS: credenciais IAM e uma policy da fila (no LocalStack) restringindo quem
  envia e consome. O consumidor **ainda** valida o domínio (providerId válido etc.).

O Keycloak sobe com um **realm importado** (`keycloak/realm.json`) contendo esses
clients e um *hardcoded claim mapper* para `provider_id`. Assim tudo funciona com
`docker compose up`.

---

## Parte 9: Uber Fx (composição e ciclo de vida)

Fx é um container de **injeção de dependência**. Você declara construtores e o Fx
monta o grafo:

```go
func main() {
    fx.New(
        config.Module,     // fx.Provide(LoadConfig)
        postgres.Module,   // fx.Provide(NewPool) + OnStop: pool.Close()
        sqsx.Module,       // fx.Provide(NewSQSClient)
        auth.Module,       // fx.Provide(NewJWTVerifier)
        wagering.Module,   // repos + casos de uso
        httpapi.Module,    // fx.Provide(NewServer) + fx.Invoke(registra no lifecycle)
        workers.Module,    // consumer SQS, outbox publisher, reference worker
    ).Run()               // bloqueia até SIGTERM e roda os OnStop
}
```

`fx.Lifecycle` com `OnStart`/`OnStop`:

```go
lc.Append(fx.Hook{
    OnStart: func(ctx context.Context) error {
        go srv.Serve(ln) // não bloquear o OnStart!
        return nil
    },
    OnStop: func(ctx context.Context) error {
        return srv.Shutdown(ctx) // para de aceitar e espera as requisições em curso
    },
})
```

Ordem de shutdown (o Fx roda os `OnStop` na **ordem inversa** do start, o que dá
naturalmente o que o desafio pede):

1. HTTP para de aceitar conexões; consumidor para de chamar `ReceiveMessage`.
2. Workers terminam o item atual (com `context` com prazo), ou devolvem a
   visibilidade da mensagem (`ChangeMessageVisibility` para 0).
3. Fecha o pool do Postgres e o cliente SQS **por último**.

**O domínio não importa Fx, HTTP, SQS nem pgx.** O Fx só aparece em `cmd/` e nos
arquivos `module.go` das camadas de fora.

---

## Parte 10: arquitetura proposta

### 10.1 Estrutura de pacotes (hexagonal / ports & adapters)

```
.
├── cmd/
│   └── server/main.go              # fx.New(...).Run()
├── internal/
│   ├── domain/                     # PURO: só stdlib + uuid
│   │   ├── money/                  # Money, Currency, parse, aritmética
│   │   ├── wallet/                 # Wallet, LedgerEntry, erros
│   │   ├── wagering/               # WagerTransaction, Kind, Status, máquina de estados, FailureCode
│   │   └── events/                 # structs dos eventos + envelope
│   ├── app/                        # casos de uso (orquestram domínio + portas)
│   │   ├── ports.go                # interfaces: WalletRepo, TxRepo, Outbox, Inbox, UnitOfWork
│   │   ├── open_wallet.go
│   │   ├── process_wager.go        # usado por HTTP E por SQS
│   │   ├── resolve_references.go
│   │   └── reconcile.go
│   ├── adapters/
│   │   ├── postgres/               # pgx, SQL explícito, UnitOfWork (BEGIN/COMMIT)
│   │   ├── httpapi/                # handlers, DTOs, middleware de auth, mapeamento de erros
│   │   ├── sqsconsumer/            # loop de recebimento, inbox, delete
│   │   ├── outboxpublisher/
│   │   ├── auth/                   # validação JWT/JWKS, Principal
│   │   └── observability/          # slog JSON, métricas Prometheus
│   └── platform/
│       ├── config/
│       └── fxmodules/              # fx.Module de cada camada
├── migrations/                     # golang-migrate: 0001_init.up.sql / .down.sql
├── deploy/
│   ├── keycloak/realm.json
│   └── localstack/init-sqs.sh      # cria filas + DLQ + redrive
├── test/
│   ├── integration/                # //go:build integration (testcontainers)
│   └── chaos/                      # 3 instâncias, kill -9 etc.
├── docker-compose.yml
├── Dockerfile
├── .env.example
├── README.md
└── ARCHITECTURE.md
```

**UnitOfWork** é a peça que resolve "a delimitação da transação SQL entre os
repositórios": o caso de uso pede `uow.Do(ctx, func(tx Repos) error {...})` e todos
os repositórios dentro do callback usam a **mesma** `pgx.Tx`.

### 10.2 Schema (resumo)

```sql
wallets (
  id UUID PK, player_id UUID, currency CHAR(3),
  balance_minor BIGINT NOT NULL CHECK (balance_minor >= 0),
  version BIGINT NOT NULL CHECK (version >= 1),
  created_at, updated_at,
  UNIQUE (player_id, currency)
)

wager_transactions (
  id UUID PK,
  origin TEXT CHECK (origin IN ('INTERNAL','EXTERNAL')),
  kind TEXT, status TEXT,
  wallet_id UUID FK, player_id UUID,
  amount_minor BIGINT CHECK (amount_minor >= 0), currency CHAR(3),
  -- só externos:
  provider_id TEXT, external_transaction_id TEXT, idempotency_key TEXT, payload_hash BYTEA,
  round_id TEXT, game_id TEXT,
  reference_external_transaction_id TEXT, reference_transaction_id UUID,
  -- resultado:
  failure_code TEXT, balance_after_minor BIGINT,
  attempts INT DEFAULT 0, next_attempt_at TIMESTAMPTZ,
  created_at, updated_at,
  -- integridade:
  CHECK ( (origin='INTERNAL' AND kind='OPENING' AND provider_id IS NULL ...)
       OR (origin='EXTERNAL' AND kind<>'OPENING' AND provider_id IS NOT NULL ...) ),
  UNIQUE (provider_id, external_transaction_id),
  UNIQUE (provider_id, idempotency_key)
)
-- uma abertura por carteira:
CREATE UNIQUE INDEX one_opening_per_wallet ON wager_transactions (wallet_id) WHERE kind='OPENING';
-- uma reversão por referência:
CREATE UNIQUE INDEX one_reversal_per_reference ON wager_transactions (reference_transaction_id)
  WHERE kind IN ('REFUND','ROLLBACK') AND status='PROCESSED';
-- worker de pendências:
CREATE INDEX pending_due ON wager_transactions (next_attempt_at) WHERE status IN ('PENDING','PENDING_REFERENCE');

wallet_ledger_entries (append-only, trigger + REVOKE; ver Parte 2)
inbox_messages (UNIQUE (consumer_name, message_id))
outbox_events  (event_id PK; índice em next_attempt_at WHERE published_at IS NULL)
```

### 10.3 Contrato HTTP (proposta de status codes)

| Situação | HTTP | Corpo |
|---|---|---|
| processada (nova) | `201` | `{transactionId, status: PROCESSED, balance, idempotentReplay: false}` |
| replay | `200` | mesmo corpo salvo, `idempotentReplay: true` |
| aguardando referência | `202` | `{transactionId, status: PENDING_REFERENCE}` |
| rejeição de negócio | `422` | `{transactionId, status: REJECTED, failureCode}` (persistida, replay devolve o mesmo) |
| JSON/valor inválido, sem header | `400` | `{error: {code: INVALID_REQUEST, details}}` (nada persistido) |
| chave reutilizada / carteira duplicada | `409` | `{error: {code: IDEMPOTENCY_CONFLICT}}` |
| sem token / token inválido | `401` | |
| provedor errado / sem role | `403` | |
| banco ou SQS fora | `503` + `Retry-After` | `{error: {code: TEMPORARILY_UNAVAILABLE}}` |

---

## Parte 11: observabilidade

- **Logs JSON** com `log/slog` (`slog.NewJSONHandler`), sempre com `correlationId`,
  `transactionId`, `walletId`, `providerId`, `messageId`. **Nunca** logar token
  nem o payload inteiro.
- **Métricas Prometheus** em `/metrics`:
  `wager_transactions_total{source,kind,status}`, `idempotent_replays_total`,
  `sqs_messages_total{outcome}` (processada, duplicata, retry, DLQ),
  `sqs_queue_messages{queue}`, `wallet_lock_conflicts_total`,
  `outbox_lag_seconds` (idade do evento não publicado mais antigo),
  `wager_processing_duration_seconds` (histograma), `reconciliation_mismatch_total`.
  Lista completa e decisões no
  [ARCHITECTURE.md](../ARCHITECTURE.md#observabilidade-métricas-e-logs).
- **Health:** `/health/live` sempre 200 se o processo está de pé;
  `/health/ready` faz `SELECT 1` no Postgres e `GetQueueAttributes` no SQS.

---

## Parte 12: testes

| Nível | Ferramenta | O que prova |
|---|---|---|
| Unitário | `testing`, table-driven | Money, Wallet, máquina de estados, regras dos 5 tipos, hash canônico |
| Integração | Postgres, LocalStack e Keycloak reais do docker compose (o plano previa `testcontainers-go`; a solução usa o compose, com banco e filas descartáveis por teste) e `//go:build integration` | constraints, trigger do ledger, inbox, outbox, DLQ, auth real |
| Concorrência | goroutines + `-race` | 50 apostas iguais = 1 débito; 2 × 80 sobre 100 = 20 de saldo |
| Multi-instância | 3 containers da app no compose (ou 3 `exec.Command`) | mesmos cenários com processos separados |
| Falha | `kill -9` em processos reais, proxy TCP cortável (Postgres/SQS fora do ar) e ganchos de falha injetados nos testes | crash entre commit e delete/publish, dependência fora, reinício |
| Fx | `fxtest.New(t, ...)` com `RequireStart`/`RequireStop` | composição sobe e desce e libera recursos |

No fim de cada teste: **reconciliação** (saldo = Σ ledger).

---

## Parte 13: roteiro de construção (ordem sugerida)

Cada fase entrega algo testável. Não pule para a próxima com a anterior vermelha.

1. **Fundação:** `go.mod`, layout de pastas, `Money` com testes completos.
2. **Domínio:** `Wallet`, `LedgerEntry`, `WagerTransaction` + máquina de estados +
   erros (`errors.Is`). Só testes unitários, sem banco.
3. **Infra local:** `docker-compose.yml` com Postgres, LocalStack e Keycloak;
   migrations com constraints e trigger; teste de integração das constraints.
4. **Abertura de carteira + leitura:** UnitOfWork com pgx, `POST /wallets`,
   `GET /wallets/:id`, ledger paginado, outbox gravada (ainda sem publisher).
5. **Processar BET/WIN/LOSS via HTTP** com idempotência e `FOR UPDATE`. Aqui entram
   os testes de concorrência (50x e 80+80).
6. **Reversões** (REFUND/ROLLBACK), política de combinações e `PENDING_REFERENCE`
   + worker com `SKIP LOCKED`.
7. **Auth:** middleware JWT, claims, isolamento entre provedores e testes com
   o Keycloak real.
8. **Consumidor SQS** + inbox + DLQ + shutdown gracioso.
9. **Outbox publisher** com lease, backoff e múltiplas instâncias.
10. **Reconciliação, observabilidade e health.**
11. **Testes de caos e multi-instância.**
12. **README.md + ARCHITECTURE.md** (o ARCHITECTURE pode ser escrito aos poucos,
    a cada fase, registrando as decisões deste guia).

---

## Glossário rápido

| Termo | Em uma linha |
|---|---|
| **At-least-once** | a mensagem chega 1 ou mais vezes, nunca 0 (se ninguém apagar) |
| **Exactly-once (efeito)** | at-least-once + idempotência = efeito de uma vez só |
| **Idempotência** | repetir não muda o resultado |
| **Ledger** | livro de lançamentos imutável; o "extrato" |
| **Append-only** | só insere; corrige com lançamento novo |
| **Reconciliação** | conferir se saldo armazenado = soma do ledger |
| **Lost update** | duas escritas concorrentes, uma apaga a outra |
| **Lock pessimista** | trava antes de ler (`FOR UPDATE`) |
| **Lock otimista** | não trava; detecta conflito pela `version` e tenta de novo |
| **SKIP LOCKED** | "pula as linhas que outro já travou", usado para filas no Postgres |
| **Inbox** | tabela que lembra quais mensagens já foram tratadas |
| **Outbox** | tabela de eventos a publicar, gravada junto com o dado |
| **Dual write** | gravar em 2 sistemas sem atomicidade; o problema que a outbox resolve |
| **DLQ** | fila para mensagens que falharam demais |
| **Visibility timeout** | tempo que a mensagem fica escondida enquanto alguém processa |
| **MessageGroupId** | chave de ordenação FIFO no SQS |
| **Backoff exponencial** | esperar 1s, 2s, 4s, 8s... entre tentativas |
| **Jitter** | aleatoriedade no backoff para não sincronizar as instâncias |
| **Value object** | tipo imutável comparado por valor (Money) |
| **Agregado / raiz** | grupo de objetos alterado só por uma entidade (Wallet) |
| **Reidratação** | reconstruir o objeto a partir do banco sem reexecutar regras/eventos |
| **client_credentials** | login OAuth de máquina (client_id + secret) |
| **JWKS** | chaves públicas do IdP para validar a assinatura do JWT |
