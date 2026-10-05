## B: 300 req/s com partidas dobradas

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
| **latência p50 / p95 / p99** | **13.4 / 30.5 / 46.0 ms** |
| latência máxima | 132.0 ms |
| **erros** (503, 409, transporte, outros) | **0 (0.00%)** |
| **conflitos de lock** (`wallet_lock_conflicts_total`) | **0** |
| novas tentativas automáticas | 0 |
| **atraso da outbox** (`outbox_lag_seconds`) médio / máximo | **0.17 s / 0.99 s** |
| eventos pendentes na outbox (máximo) | 469 |
| outbox vazia depois da carga em | 0s |
| reconciliação | 200 consistentes, 0 divergentes |

| Desfecho | Quantidade |
|---|---|
| created | 17094 |
| replay | 911 |

| Tipo | Requisições | p50 | p95 | p99 (ms) |
|---|---|---|---|---|
| BET | 12083 | 14.0 | 31.3 | 46.8 |
| WIN | 4173 | 13.9 | 31.6 | 47.8 |
| LOSS | 838 | 7.3 | 18.3 | 28.3 |
| REPLAY | 911 | 4.0 | 9.9 | 14.6 |
