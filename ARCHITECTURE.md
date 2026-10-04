# Arquitetura

Registro das decisões técnicas. Cada seção é preenchida conforme a fase
correspondente é implementada (ver o roteiro em
[docs/GUIA-DO-DESAFIO.md](docs/GUIA-DO-DESAFIO.md#parte-13-roteiro-de-construção-ordem-sugerida)).

| Seção | Status |
|---|---|
| [Organização dos pacotes](#organização-dos-pacotes) | ✅ |
| [Dinheiro (`Money`)](#dinheiro-money) | ✅ |
| Carteira, ledger e transações | ⏳ fase 2 |
| Banco, migrations e transação SQL | ⏳ fase 3 |
| Idempotência | ⏳ fase 5 |
| Concorrência e locks | ⏳ fase 5 |
| Reversões e referências pendentes | ⏳ fase 6 |
| Autenticação e autorização | ⏳ fase 7 |
| Inbox, SQS e DLQ | ⏳ fase 8 |
| Outbox | ⏳ fase 9 |
| Uber Fx e shutdown | ⏳ fase 4+ |
| Observabilidade | ⏳ fase 10 |

---

## Organização dos pacotes

```
internal/domain/   regras de negócio puras: só stdlib, sem Fx, HTTP, SQS ou pgx
internal/app/      casos de uso (orquestram domínio + interfaces de persistência)
internal/adapters/ postgres, http, sqs, auth, observabilidade
cmd/server/        composição com Uber Fx
```

O domínio não importa nenhuma biblioteca de infraestrutura. Isso permite
testá-lo sem banco e trocar adaptadores sem tocar nas regras.

---

## Dinheiro (`Money`)

Pacote: [`internal/domain/money`](internal/domain/money).

### Representação

`int64` em **unidades mínimas** (centavos), com escala fixa de **2 casas**.
`"25.00"` vira `2500`. Nenhum caminho (parsing, cálculo, serialização ou
persistência) usa `float32`/`float64`.

| Aspecto | Decisão |
|---|---|
| Tipo | `Money{minor int64, currency Currency}`, campos privados, imutável |
| Moeda | `Currency` validada; aceitas `BRL`, `USD`, `EUR` (todas com 2 casas) |
| Persistência | `BIGINT` (centavos) + `CHAR(3)` (moeda) |
| Limites | de `-92233720368547758.08` a `92233720368547758.07` |
| Overflow | parsing, `Add`, `Sub` e `Neg` devolvem erro em vez de estourar |
| Valor zero | `Money{}` e `Currency{}` são inválidos; operações devolvem `ErrUninitialized` |

Por que `int64` e não uma biblioteca decimal: a escala é fixa, a aritmética
inteira é exata e rápida, mapeia diretamente para `BIGINT`, e não traz
dependência. O custo é tratar overflow manualmente, o que está coberto por testes.

### Contrato externo e parsing

Formato: `{"amount":"25.00","currency":"BRL"}`. O valor é **string** JSON; um
número JSON (`25.00`) é rejeitado sem ser convertido para float.

`money.Parse` aceita **somente a forma canônica**: `^(0|[1-9][0-9]*)\.[0-9]{2}$`.

| Rejeitado | Erro |
|---|---|
| vazio, `NaN`, `Infinity`, `1e3`, `+1.00`, `25,00`, espaços, `025.00`, `.50` | `ErrInvalidFormat` |
| `25`, `25.0`, `25.001`, `25.000` | `ErrInvalidScale` |
| `-1.00`, `-0.00` | `ErrNegativeAmount` |
| acima de `92233720368547758.07` | `ErrAmountTooLarge` |
| moeda desconhecida ou minúscula (`brl`) | `ErrInvalidCurrency` |

Todos os erros de valor casam com `errors.Is(err, money.ErrInvalidAmount)`,
o que permite ao adaptador HTTP mapear a família inteira para `400`.

**Não há normalização nem arredondamento.** Como só a forma canônica é aceita,
o texto recebido já é o texto que entra no hash de idempotência: `"25.00"` e
`"25.0"` nunca geram hashes diferentes para a mesma operação, porque o segundo
é rejeitado.

### Valores negativos

- **Entrada externa** (`Parse`): negativos são rejeitados.
- **Cálculos internos** (`Sub`, `Neg`, `FromMinorUnits`): negativos são
  permitidos, por exemplo a `difference` da reconciliação.
- **Saldo da carteira**: a proibição de saldo negativo é responsabilidade da
  `Wallet` e de uma `CHECK` no banco, não do `Money`.

`UnmarshalJSON` aceita negativos para permitir o round-trip de valores internos.
Entradas financeiras externas devem passar por `Parse`.

### Compatibilidade de moedas

`Add`, `Sub` e `Cmp` entre moedas diferentes devolvem `ErrCurrencyMismatch`.
`Equal` entre moedas diferentes devolve `false`.
