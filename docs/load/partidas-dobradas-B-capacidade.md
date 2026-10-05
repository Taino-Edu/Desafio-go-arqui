## B: com partidas dobradas

| Parâmetro | Valor |
|---|---|
| instâncias | 1 (http://localhost:9091) |
| clientes simultâneos | 32 |
| taxa pedida | sem limite (cada cliente envia assim que recebe a resposta: mede a capacidade) |
| duração medida | 1m0s (após 5s de aquecimento) |
| carteiras | 200 (10% das operações numa carteira disputada) |
| mistura | 70% BET, 25% WIN, 5% LOSS; 5% de reenvios |
| máquina do gerador | linux/amd64, 4 CPUs, go1.27.1 |

| Resultado | Valor |
|---|---|
| requisições | 27841 |
| **throughput** | **463 req/s** |
| **latência p50 / p95 / p99** | **53.6 / 158.1 / 423.0 ms** |
| latência máxima | 1340.2 ms |
| **erros** (503, 409, transporte, outros) | **0 (0.00%)** |
| **conflitos de lock** (`wallet_lock_conflicts_total`) | **0** |
| novas tentativas automáticas | 0 |
| **atraso da outbox** (`outbox_lag_seconds`) médio / máximo | **24.91 s / 48.85 s** |
| eventos pendentes na outbox (máximo) | 41449 |
| outbox vazia depois da carga em | 33.8s |
| reconciliação | 200 consistentes, 0 divergentes |

| Desfecho | Quantidade |
|---|---|
| created | 26403 |
| replay | 1438 |

| Tipo | Requisições | p50 | p95 | p99 (ms) |
|---|---|---|---|---|
| BET | 18385 | 55.2 | 170.2 | 434.0 |
| WIN | 6678 | 55.5 | 164.7 | 431.7 |
| LOSS | 1340 | 41.1 | 142.1 | 379.9 |
| REPLAY | 1438 | 32.7 | 48.7 | 60.1 |
