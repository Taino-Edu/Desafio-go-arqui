## A: sem partidas dobradas

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
| requisições | 35460 |
| **throughput** | **590 req/s** |
| **latência p50 / p95 / p99** | **44.2 / 110.4 / 263.6 ms** |
| latência máxima | 1006.5 ms |
| **erros** (503, 409, transporte, outros) | **0 (0.00%)** |
| **conflitos de lock** (`wallet_lock_conflicts_total`) | **0** |
| novas tentativas automáticas | 0 |
| **atraso da outbox** (`outbox_lag_seconds`) médio / máximo | **28.02 s / 59.06 s** |
| eventos pendentes na outbox (máximo) | 63552 |
| outbox vazia depois da carga em | 1m26.3s |
| reconciliação | 200 consistentes, 0 divergentes |

| Desfecho | Quantidade |
|---|---|
| created | 33629 |
| replay | 1831 |

| Tipo | Requisições | p50 | p95 | p99 (ms) |
|---|---|---|---|---|
| BET | 23543 | 45.4 | 115.4 | 266.0 |
| WIN | 8384 | 45.5 | 113.3 | 280.1 |
| LOSS | 1702 | 37.0 | 100.9 | 242.2 |
| REPLAY | 1831 | 28.5 | 42.5 | 54.2 |
