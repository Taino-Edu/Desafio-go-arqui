## Capacidade: 1 instância, sem limite de taxa

| Parâmetro | Valor |
|---|---|
| instâncias | 1 (http://localhost:8080) |
| clientes simultâneos | 32 |
| taxa pedida | sem limite (cada cliente envia assim que recebe a resposta: mede a capacidade) |
| duração medida | 1m0s (após 5s de aquecimento) |
| carteiras | 200 (10% das operações numa carteira disputada) |
| mistura | 70% BET, 25% WIN, 5% LOSS; 5% de reenvios |
| máquina do gerador | linux/amd64, 4 CPUs, go1.27.1 |

| Resultado | Valor |
|---|---|
| requisições | 38959 |
| **throughput** | **648 req/s** |
| **latência p50 / p95 / p99** | **38.6 / 108.4 / 265.1 ms** |
| latência máxima | 949.4 ms |
| **erros** (503, 409, transporte, outros) | **0 (0.00%)** |
| **conflitos de lock** (`wallet_lock_conflicts_total`) | **0** |
| novas tentativas automáticas | 0 |
| **atraso da outbox** (`outbox_lag_seconds`) médio / máximo | **26.04 s / 52.14 s** |
| eventos pendentes na outbox (máximo) | 61698 |
| outbox vazia depois da carga em | 49.3s |
| reconciliação | 200 consistentes, 0 divergentes |

| Desfecho | Quantidade |
|---|---|
| created | 37033 |
| replay | 1926 |

| Tipo | Requisições | p50 | p95 | p99 (ms) |
|---|---|---|---|---|
| BET | 25824 | 39.8 | 111.5 | 265.7 |
| WIN | 9337 | 39.8 | 114.6 | 279.1 |
| LOSS | 1872 | 32.4 | 117.1 | 281.8 |
| REPLAY | 1926 | 25.0 | 40.0 | 49.0 |
