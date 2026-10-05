## Sustentável: 3 instâncias, 300 req/s

| Parâmetro | Valor |
|---|---|
| instâncias | 3 (http://localhost:9091, http://localhost:9092, http://localhost:9093) |
| clientes simultâneos | 32 |
| taxa pedida | 300 req/s (carga aberta, ritmo fixo) |
| duração medida | 1m0s (após 5s de aquecimento) |
| carteiras | 200 (10% das operações numa carteira disputada) |
| mistura | 70% BET, 25% WIN, 5% LOSS; 5% de reenvios |
| máquina do gerador | linux/amd64, 4 CPUs, go1.27.1 |

| Resultado | Valor |
|---|---|
| requisições | 18003 |
| **throughput** | **300 req/s** |
| **latência p50 / p95 / p99** | **9.8 / 23.8 / 44.6 ms** |
| latência máxima | 224.1 ms |
| **erros** (503, 409, transporte, outros) | **0 (0.00%)** |
| **conflitos de lock** (`wallet_lock_conflicts_total`) | **0** |
| novas tentativas automáticas | 0 |
| **atraso da outbox** (`outbox_lag_seconds`) médio / máximo | **0.47 s / 4.50 s** |
| eventos pendentes na outbox (máximo) | 1903 |
| outbox vazia depois da carga em | 0s |
| reconciliação | 200 consistentes, 0 divergentes |

| Desfecho | Quantidade |
|---|---|
| created | 17108 |
| replay | 895 |

| Tipo | Requisições | p50 | p95 | p99 (ms) |
|---|---|---|---|---|
| BET | 12022 | 10.2 | 24.8 | 46.0 |
| WIN | 4214 | 10.3 | 24.0 | 45.0 |
| LOSS | 872 | 6.5 | 16.8 | 44.6 |
| REPLAY | 895 | 3.9 | 9.4 | 18.6 |
