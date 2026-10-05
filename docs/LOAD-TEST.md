# Teste de carga

Gerador: [`cmd/loadtest`](../cmd/loadtest/main.go) (só biblioteca padrão).
Relatórios completos de cada execução em [`docs/load/`](load/).

## Comando reproduzível

```sh
docker compose up -d --build                     # ambiente completo
go run ./cmd/loadtest -duration 60s -concurrency 32 -wallets 200            # capacidade
go run ./cmd/loadtest -duration 60s -concurrency 32 -wallets 200 -rate 300  # sustentável

# 3 instâncias independentes (processos com HTTP, consumidor, publicador e worker)
docker compose stop app
go build -o /tmp/server ./cmd/server
for i in 1 2 3; do
  (set -a; . ./.env.example; set +a; HTTP_ADDR=:909$i INSTANCE_ID=load-$i DB_MAX_CONNS=15 /tmp/server) &
done
go run ./cmd/loadtest -api http://localhost:9091,http://localhost:9092,http://localhost:9093 \
  -duration 60s -concurrency 32 -wallets 200 -rate 300
```

Parâmetros: `-api` (uma ou mais instâncias, a carga é espalhada entre elas),
`-duration`, `-warmup` (5s, fora dos números), `-concurrency`, `-wallets`,
`-hot` (fração numa carteira disputada), `-replay` (fração de reenvios),
`-rate` (req/s; 0 = sem limite) e `-out` (relatório em Markdown).

## Ambiente

| Item | Valor |
|---|---|
| máquina | 1 VM, 4 vCPUs Intel Xeon 2.80GHz, 15 GB |
| tudo na mesma máquina | gerador de carga, API, PostgreSQL 16.15, LocalStack 4.4 (SQS) e Keycloak |
| API | Go 1.27.1; pool de 20 conexões (15 por instância no cenário de 3) |
| outbox | lote de 100 agregados, até 20 eventos por agregado, 8 envios em paralelo |

Como tudo divide as mesmas 4 CPUs, os números são **relativos**: servem
para comparar versões e cenários, não como capacidade de produção. Com 3
instâncias na mesma máquina, a CPU é dividida por mais processos: o cenário
mostra correção e comportamento com publicadores concorrentes, não ganho de
vazão.

## Metodologia

- **Autenticação real:** tokens `client_credentials` do Keycloak, renovados
  durante a execução. Operações alternam `provider-a` e `provider-b`.
- **Preparação:** 200 carteiras abertas com 1.000.000,00 (saldo suficiente
  para não haver rejeição por saldo).
- **Mistura:** 70% `BET` 1.00, 25% `WIN` 1.50, 5% `LOSS` 0.00; 5% das
  requisições são **reenvios** de operações já feitas (mesma chave, mesmo
  corpo); **10%** das operações vão para **uma única carteira** (disputa de
  lock entre clientes e instâncias).
- **Dois modos:**
  - **capacidade** (sem `-rate`): 32 clientes, cada um envia assim que recebe
    a resposta. Mede o máximo que a máquina aguenta.
  - **sustentável** (`-rate 300`): carga aberta em ritmo fixo, como tráfego
    real. Mostra latência e atraso da outbox abaixo do limite.
- **Medição:** 5s de aquecimento descartados; 60s medidos. Latência do
  cliente (inclui fila de conexão do gerador). Contadores do servidor
  (`/metrics`) lidos antes e depois, somando as instâncias; o atraso da
  outbox é amostrado a cada 0,5s durante a medição.
- **Conferência no fim:** espera a outbox esvaziar (até 2 min) e
  **reconcilia todas as 200 carteiras** pela API. Carga que deixa saldo
  errado não conta como sucesso.

## Resultados

| Cenário | Vazão | p50 / p95 / p99 | Erros | Conflitos de lock | Atraso da outbox médio / máx. | Outbox vazia após | Reconciliação |
|---|---|---|---|---|---|---|---|
| Capacidade, 1 instância | **648 req/s** | 38.6 / 108.4 / 265.1 ms | 0 | 0 | 26.0 s / 52.1 s | 49 s | 200/200 ✅ |
| Sustentável, 1 instância (300 req/s) | 300 req/s | **6.4 / 34.3 / 107.5 ms** | 0 | 0 | **0.21 s / 0.89 s** | imediato | 200/200 ✅ |
| Sustentável, 3 instâncias (300 req/s) | 300 req/s | **9.8 / 23.8 / 44.6 ms** | 0 | 0 | **0.47 s / 4.50 s** | imediato | 200/200 ✅ |
| Capacidade, 3 instâncias | 422 req/s | 33.2 / 270.4 / 1097.5 ms | 0 | 0 | 16.4 s / 30.5 s | 8 s | 200/200 ✅ |

Em todos: **nenhum erro** (nenhum `503`, `409` ou falha de transporte),
replays respondidos como replay, e **todas as carteiras consistentes**.

Leitura:

- **Até ~300 req/s** (cerca de 600 eventos/s) o sistema fica folgado: p99
  abaixo de 110 ms e eventos publicados em menos de 1 s, com 1 ou 3
  instâncias.
- **Na capacidade máxima desta máquina** (~650 req/s), o PostgreSQL satura
  (mais de 200% de CPU das 400% disponíveis) e o publicador, que disputa o
  mesmo banco, fica atrás da escrita: o atraso cresce durante o pico e a
  outbox esvazia logo depois. Nada se perde nem trava; é o limite do
  hardware compartilhado.
- **Conflitos de lock = 0:** a carteira disputada recebeu 10% de todo o
  tráfego, vindo de 32 clientes e 3 instâncias. As operações dela se
  enfileiram no `SELECT ... FOR UPDATE` e esperam a vez, sempre dentro do
  `lock_timeout` (3s). O contador mede só as disputas que **falham** (lock
  timeout, deadlock, versão desatualizada), e nenhuma falhou.
- **3 instâncias na capacidade** fazem menos vazão que 1: são 3 processos
  dividindo as mesmas 4 CPUs com o banco. Em máquinas separadas, a
  coordenação continua no banco e não exige nada a mais.

## O que o teste de carga encontrou (e foi corrigido)

A primeira execução (relatório [antes](load/antes-capacidade-1-instancia.md))
mostrou a API saudável, mas a **outbox parando**: atraso de 35 s, 42 mil
eventos pendentes, e a fila não esvaziou nem 2 minutos depois da carga.
A investigação, medindo em vez de supor, achou três problemas em sequência:

1. **A reivindicação estourava o `statement_timeout` e a publicação
   parava.** Logo depois de um pico, as estatísticas do PostgreSQL ainda
   dizem que quase nada está pendente; com essa crença, o planejador
   verificava "há evento anterior do mesmo agregado?" varrendo **todos** os
   pendentes para cada linha (2 milhões de comparações para um lote de 50,
   com 40 mil pendentes). A consulta foi reescrita para ter custo limitado
   pelo tamanho do lote, **qualquer que seja o plano escolhido**: a cabeça é
   conferida por uma subconsulta `ORDER BY seq LIMIT 1` e os eventos
   seguintes de cada agregado vêm de uma janela limitada dos pendentes mais
   antigos, numerada por agregado. Migration `000003` troca o índice de
   `next_attempt_at` (só servia à consulta antiga) por um índice parcial por
   `seq`. Teste de regressão: `TestOutbox_ClaimScalesWithBacklog` (40 mil
   pendentes com estatísticas congeladas: a versão anterior leva ~600 ms por
   rodada; a atual, ~20 ms).
2. **Um evento e um `UPDATE` por vez.** O publicador passou a mandar em
   **ondas**: a onda *n* leva o *n*-ésimo evento de cada agregado, em lotes
   de até 10 por chamada (`SendMessageBatch`) e até 8 chamadas em paralelo,
   e marca a onda inteira num único `UPDATE`. Num mesmo lote nunca há dois
   eventos do mesmo agregado, então uma falha parcial não fura a ordem; a
   onda *n+1* só sai depois da *n*.
3. **Lote grande demais custa caro.** Com 500 cabeças por rodada, partes da
   consulta voltavam a ser caras com estatísticas velhas (custo cresce com o
   quadrado do lote: 12 ms com 50, 35 ms com 100, 111 ms com 200, 464 ms com
   500) e as 3 instâncias tiveram 19 rodadas abortadas por timeout. O padrão
   ficou em **100** (`OUTBOX_BATCH_SIZE`): zero rodadas falhas nos cenários
   finais.

Antes × depois, mesma carga de capacidade (32 clientes, 1 instância):

| | Antes | Depois |
|---|---|---|
| rodadas da outbox abortadas por timeout | sim (a publicação parava) | **nenhuma** |
| outbox vazia depois da carga | não esvaziou em 2 min | **49 s** (com 50% mais eventos, porque a medição foi de 60 s em vez de 30 s) |
| atraso máximo, 3 instâncias a 300 req/s | 17 s (com 19 rodadas abortadas) | **4,5 s** (nenhuma) |

Cada mudança foi verificada com mutação: com os eventos de um agregado
publicados em paralelo, ou tudo numa onda só, os testes de ordem falham
(`[1 3 2 4 ...]`); com a consulta antiga, o teste de regressão falha.

## Limitações do teste

- Uma única máquina para tudo (ver Ambiente): números relativos.
- LocalStack não é o SQS real: latência e limites de vazão diferentes.
- O gerador mede a latência no cliente, incluindo a disputa de CPU com o
  servidor.
- Não houve teste de longa duração (horas): crescimento das tabelas de
  inbox e outbox sem rotina de retenção não foi medido.
