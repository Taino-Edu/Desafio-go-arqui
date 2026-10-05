## Antes: 1 instância, publicador sequencial

| Parâmetro | Valor |
|---|---|
| instâncias | 1 (http://localhost:8080) |
| clientes simultâneos | 32 |
| duração medida | 30s (após 5s de aquecimento) |
| carteiras | 200 (10% das operações numa carteira disputada) |
| mistura | 70% BET, 25% WIN, 5% LOSS; 5% de reenvios |
| máquina do gerador | linux/amd64, 4 CPUs, go1.27.1 |

| Resultado | Valor |
|---|---|
| requisições | 19692 |
| **throughput** | **656 req/s** |
| **latência p50 / p95 / p99** | **37.4 / 122.5 / 268.1 ms** |
| latência máxima | 734.4 ms |
| **erros** (503, 409, transporte, outros) | **0 (0.00%)** |
| **conflitos de lock** (`wallet_lock_conflicts_total`) | **0** |
| novas tentativas automáticas | 0 |
| **atraso da outbox** (`outbox_lag_seconds`) médio / máximo | **17.56 s / 35.23 s** |
| eventos pendentes na outbox (máximo) | 42134 |
| outbox vazia depois da carga em | não esvaziou em 2 min |
| reconciliação | 200 consistentes, 0 divergentes |

| Desfecho | Quantidade |
|---|---|
| created | 18729 |
| replay | 963 |

| Tipo | Requisições | p50 | p95 | p99 (ms) |
|---|---|---|---|---|
| BET | 13001 | 38.7 | 127.9 | 272.6 |
| WIN | 4723 | 38.5 | 126.2 | 259.3 |
| LOSS | 1005 | 31.5 | 115.9 | 316.5 |
| REPLAY | 963 | 24.2 | 39.8 | 50.4 |
