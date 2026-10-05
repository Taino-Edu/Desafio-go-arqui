## Sustentável: 1 instância, 300 req/s

| Parâmetro | Valor |
|---|---|
| instâncias | 1 (http://localhost:8080) |
| clientes simultâneos | 32 |
| taxa pedida | 300 req/s (carga aberta, ritmo fixo) |
| duração medida | 1m0s (após 5s de aquecimento) |
| carteiras | 200 (10% das operações numa carteira disputada) |
| mistura | 70% BET, 25% WIN, 5% LOSS; 5% de reenvios |
| máquina do gerador | linux/amd64, 4 CPUs, go1.27.1 |

| Resultado | Valor |
|---|---|
| requisições | 18000 |
| **throughput** | **300 req/s** |
| **latência p50 / p95 / p99** | **6.4 / 34.3 / 107.5 ms** |
| latência máxima | 573.0 ms |
| **erros** (503, 409, transporte, outros) | **0 (0.00%)** |
| **conflitos de lock** (`wallet_lock_conflicts_total`) | **0** |
| novas tentativas automáticas | 0 |
| **atraso da outbox** (`outbox_lag_seconds`) médio / máximo | **0.21 s / 0.89 s** |
| eventos pendentes na outbox (máximo) | 453 |
| outbox vazia depois da carga em | 0s |
| reconciliação | 200 consistentes, 0 divergentes |

| Desfecho | Quantidade |
|---|---|
| created | 17087 |
| replay | 913 |

| Tipo | Requisições | p50 | p95 | p99 (ms) |
|---|---|---|---|---|
| BET | 11981 | 6.7 | 39.2 | 110.1 |
| WIN | 4245 | 6.6 | 33.5 | 108.8 |
| LOSS | 861 | 4.5 | 22.2 | 93.5 |
| REPLAY | 913 | 2.7 | 9.0 | 43.2 |
