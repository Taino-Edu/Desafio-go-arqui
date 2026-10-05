## Capacidade: 3 instâncias, sem limite de taxa

| Parâmetro | Valor |
|---|---|
| instâncias | 3 (http://localhost:9091, http://localhost:9092, http://localhost:9093) |
| clientes simultâneos | 32 |
| taxa pedida | sem limite (cada cliente envia assim que recebe a resposta: mede a capacidade) |
| duração medida | 1m0s (após 5s de aquecimento) |
| carteiras | 200 (10% das operações numa carteira disputada) |
| mistura | 70% BET, 25% WIN, 5% LOSS; 5% de reenvios |
| máquina do gerador | linux/amd64, 4 CPUs, go1.27.1 |

| Resultado | Valor |
|---|---|
| requisições | 25377 |
| **throughput** | **422 req/s** |
| **latência p50 / p95 / p99** | **33.2 / 270.4 / 1097.5 ms** |
| latência máxima | 5089.0 ms |
| **erros** (503, 409, transporte, outros) | **0 (0.00%)** |
| **conflitos de lock** (`wallet_lock_conflicts_total`) | **0** |
| novas tentativas automáticas | 1 |
| **atraso da outbox** (`outbox_lag_seconds`) médio / máximo | **16.44 s / 30.50 s** |
| eventos pendentes na outbox (máximo) | 7977 |
| outbox vazia depois da carga em | 8.2s |
| reconciliação | 200 consistentes, 0 divergentes |

| Desfecho | Quantidade |
|---|---|
| created | 24085 |
| replay | 1292 |

| Tipo | Requisições | p50 | p95 | p99 (ms) |
|---|---|---|---|---|
| BET | 17092 | 35.1 | 297.4 | 1109.8 |
| WIN | 5821 | 35.4 | 323.8 | 1140.0 |
| LOSS | 1172 | 22.7 | 255.5 | 1148.6 |
| REPLAY | 1292 | 10.0 | 28.2 | 46.3 |
