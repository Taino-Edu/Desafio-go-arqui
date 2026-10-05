## A: 300 req/s sem partidas dobradas

| Parâmetro | Valor |
|---|---|
| instâncias | 1 (http://localhost:9091) |
| clientes simultâneos | 32 |
| taxa pedida | 300 req/s (carga aberta, ritmo fixo) |
| duração medida | 1m0s (após 5s de aquecimento) |
| carteiras | 200 (10% das operações numa carteira disputada) |
| mistura | 70% BET, 25% WIN, 5% LOSS; 5% de reenvios |
| máquina do gerador | linux/amd64, 4 CPUs, go1.27.1 |

| Resultado | Valor |
|---|---|
| requisições | 18005 |
| **throughput** | **300 req/s** |
| **latência p50 / p95 / p99** | **8.4 / 16.5 / 24.1 ms** |
| latência máxima | 58.9 ms |
| **erros** (503, 409, transporte, outros) | **0 (0.00%)** |
| **conflitos de lock** (`wallet_lock_conflicts_total`) | **0** |
| novas tentativas automáticas | 0 |
| **atraso da outbox** (`outbox_lag_seconds`) médio / máximo | **0.12 s / 0.66 s** |
| eventos pendentes na outbox (máximo) | 358 |
| outbox vazia depois da carga em | 0s |
| reconciliação | 200 consistentes, 0 divergentes |

| Desfecho | Quantidade |
|---|---|
| created | 17138 |
| replay | 867 |

| Tipo | Requisições | p50 | p95 | p99 (ms) |
|---|---|---|---|---|
| BET | 12001 | 8.7 | 16.9 | 24.9 |
| WIN | 4281 | 8.7 | 16.7 | 23.7 |
| LOSS | 856 | 5.6 | 11.2 | 16.8 |
| REPLAY | 867 | 3.3 | 7.2 | 10.5 |
