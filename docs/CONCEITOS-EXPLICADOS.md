# Conceitos explicados de forma simples

> Versão "sem jargão" do [guia do desafio](GUIA-DO-DESAFIO.md). Cada conceito tem
> uma analogia do dia a dia e, no fim, uma frase pronta (💬) para explicar o plano
> em uma entrevista ou apresentação.

---

## 🎰 O cenário

Pense num **cassino online**:

- O **jogador** tem uma **carteira** com dinheiro (como uma conta no banco).
- O **provedor** é a empresa que fez o jogo (o caça-níquel "fortune-chimp"). O jogo roda no servidor dele, não no nosso.
- Uma **rodada** é cada vez que o jogador aperta "girar".

Quando o jogador gira, o provedor manda uma mensagem para **nós**: "debita R$ 25 do
jogador X, é a aposta da rodada 987". Nós somos o **banco do cassino**: só cuidamos
do dinheiro.

| Mensagem | Significado | O que fazemos |
|---|---|---|
| `BET` | apostou | tira o dinheiro |
| `WIN` | ganhou | coloca o prêmio |
| `LOSS` | perdeu | não mexe em nada, só anota que a rodada acabou |
| `REFUND` | o jogo deu pau, devolve a aposta | devolve |
| `ROLLBACK` | "esquece aquela operação" | faz o contrário dela |

💬 *"O serviço é a carteira do jogador: recebe avisos dos provedores de jogo e movimenta o saldo com segurança."*

---

## 💰 1. Por que dinheiro não pode ser `float`

**Analogia:** o computador guarda `float` em binário, e alguns números não cabem
exatos em binário. É como tentar escrever 1/3 em decimal: 0,3333... nunca termina,
então ele **corta**.

```go
fmt.Println(0.1 + 0.2) // 0.30000000000000004
```

Um centavo errado em milhões de apostas vira um rombo, e ninguém sabe de onde veio.

**Solução:** contar em **centavos inteiros**. R$ 25,00 vira o número inteiro `2500`.
Inteiro não tem erro de arredondamento. E o valor chega como **texto** (`"25.00"`),
não como número, para não virar float nem na leitura do JSON.

💬 *"Guardo dinheiro em centavos como int64. Float tem erro de arredondamento, e em dinheiro isso é inaceitável."*

### Value object (`Money`)

É um "tipinho" que junta **valor + moeda** e não pode ser alterado depois de
criado. Não dá para somar reais com dólares por engano, porque o tipo recusa.

💬 *"Money é imutável e carrega a moeda. Assim é impossível misturar moedas ou alterar um valor sem querer."*

---

## 📒 2. Transação SQL (atomicidade, commit)

**Analogia:** uma transferência bancária tem dois passos, tirar da conta A e
colocar na conta B. Se a luz acabar no meio, o dinheiro não pode sumir.

Uma **transação** no banco de dados é um pacote de operações que vale **tudo ou nada**:

- `BEGIN`: começa o pacote
- ...várias operações...
- `COMMIT`: confirma tudo de uma vez. Se algo falhar antes, o banco **desfaz tudo** sozinho.

💬 *"Saldo, extrato e eventos são gravados na mesma transação: ou tudo acontece, ou nada acontece."*

---

## 📜 3. Ledger (livro-razão)

**Analogia:** é o **extrato do banco**. Cada linha registra "entrou R$ 60" ou "saiu
R$ 25", com o saldo antes e depois.

Regras:

1. **Nunca se apaga nem se edita uma linha (append-only).** Se houve erro, você
   **adiciona** uma linha nova corrigindo, como um estorno no cartão de crédito.
   Assim fica o histórico de tudo, e dá para auditar.
2. **O saldo tem que bater com o extrato.** Somando todas as entradas e subtraindo
   as saídas, o resultado tem que ser igual ao saldo guardado.

**Reconciliação** é exatamente essa conferência: "o saldo bate com o extrato?" Se
não bater, há um bug, e a gente avisa.

E quem garante que ninguém edita o extrato é o **próprio banco de dados** (uma
regra lá dentro que bloqueia edição), não só o código Go.

💬 *"O ledger é um extrato imutável. Toda correção é um lançamento novo, e a reconciliação prova que o saldo bate com o extrato."*

---

## 🔁 4. Entrega "pelo menos uma vez" (at-least-once)

**Analogia:** você manda um Pix e o app trava. Você não sabe se foi. O que você
faz? **Tenta de novo.**

Na internet é igual: o provedor mandou "debita 25", nós debitamos, mas a resposta
se perdeu no caminho. O provedor **reenvia**. A fila também reenvia se acha que
ninguém processou.

Ou seja: **a mesma mensagem VAI chegar mais de uma vez.** Não é bug, é a
realidade de sistemas distribuídos.

💬 *"Assumo que toda mensagem pode chegar repetida, então o sistema precisa aguentar isso."*

---

## 🔑 5. Idempotência

**Analogia:** o botão do elevador. Apertar 1 vez ou 10 vezes dá o mesmo
resultado: o elevador vem uma vez.

Idempotente = **repetir não muda o resultado**. Nós precisamos que "debita 25"
repetido 50 vezes debite **só uma vez**.

**Como:** cada operação vem com uma **chave de idempotência**, um "número de
protocolo" (ex.: `provider-a:transaction-123`).

1. Na primeira vez, gravamos o protocolo no banco e processamos.
2. Na segunda vez, o banco diz "esse protocolo já existe". Aí **não processamos de
   novo**, só devolvemos a mesma resposta da primeira vez.

E o **hash**? É uma "impressão digital" do conteúdo da mensagem. Serve para pegar
trapaça ou erro: se chegar o **mesmo protocolo com conteúdo diferente** (antes era
25, agora é 250), recusamos com **conflito (409)**.

Isso tem que ficar **no banco**, não na memória. Se o servidor reiniciar, a memória
apaga e ele esqueceria os protocolos já usados.

💬 *"Cada operação tem uma chave única gravada no banco. Se chegar repetida, devolvo o resultado original sem reprocessar. Mesma chave com conteúdo diferente é conflito."*

---

## 🏃 6. Concorrência e lost update

**Analogia:** duas pessoas sacando da **mesma conta conjunta**, ao mesmo tempo, em
caixas eletrônicos diferentes. A conta tem R$ 100.

```
Caixa A olha: tem 100     Caixa B olha: tem 100
A: "dá pra sacar 80!"     B: "dá pra sacar 80!"
A saca → grava 20         B saca → grava 20
```

Saíram R$ 160 de uma conta com R$ 100. Um dos saques "se perdeu" no saldo. Isso é
o **lost update** (atualização perdida).

**Solução: lock (trava).** É como uma **fila no caixa**: quem chega primeiro "pega
a chave" da conta, e o outro espera.

```
A pega a chave → vê 100 → saca 80 → saldo 20 → devolve a chave
B pega a chave → vê 20  → "não dá pra sacar 80" → RECUSADO
```

Resultado correto: uma aposta aceita, uma recusada, saldo R$ 20. **Esse é o teste
obrigatório do desafio.**

Detalhes importantes:

- A trava é **por carteira**. Carteiras diferentes não esperam umas pelas outras.
  Seria horrível travar o cassino inteiro por causa de um jogador (o desafio proíbe
  "lock global").
- Um `Mutex` do Go **não serve**, porque roda em 3 servidores diferentes e cada um
  tem sua memória. A trava tem que ser **no banco** (`SELECT ... FOR UPDATE`), que é
  o único lugar que todos compartilham.

**Os dois tipos de lock:**

- **Pessimista** ("vai dar briga, então tranco antes"): pega a trava, depois mexe.
  É o que vamos usar.
- **Otimista** ("acho que ninguém vai mexer"): não tranca. Na hora de salvar,
  confere a **versão** ("a carteira ainda está na versão 5?"). Se alguém mudou,
  tenta de novo.

💬 *"Uso lock pessimista por carteira no Postgres: duas operações na mesma carteira entram em fila, e carteiras diferentes rodam em paralelo. Uma constraint no banco impede saldo negativo como última defesa."*

---

## 🚦 7. Máquina de estados

**Analogia:** o status de um **pedido no iFood**: "recebido → preparando → saiu →
entregue". Ele não volta de "entregue" para "preparando".

| Estado | Significado |
|---|---|
| `PENDING` | recebi, ainda processando |
| `PENDING_REFERENCE` | estou esperando outra operação chegar (ver item 9) |
| `PROCESSED` ✅ | deu certo (final) |
| `REJECTED` ❌ | regra de negócio recusou, ex.: sem saldo (final) |
| `FAILED` 💥 | erro técnico permanente (final) |

Os estados "finais" **nunca mudam**. O código bloqueia transições inválidas.

**Falha transitória vs. permanente:**

- **Transitória:** "o banco caiu por 3 segundos". Tenta de novo depois.
- **Permanente:** "a mensagem está corrompida e nunca vai funcionar". Para de tentar e registra.

💬 *"Cada operação segue uma máquina de estados. Estados finais são imutáveis, e falhas temporárias geram nova tentativa, não mudança de estado."*

---

## ↩️ 8. Reversões: REFUND e ROLLBACK

- **REFUND** = "devolve a aposta". Só serve para uma BET.
- **ROLLBACK** = "desfaz como se não tivesse acontecido". Faz o **contrário**:
  - rollback de uma aposta: devolve o dinheiro
  - rollback de um prêmio: **tira** o prêmio de volta (e pode faltar saldo!)

Ambos precisam dizer **qual operação estão desfazendo** (a "referência").

**O perigo:** devolver a mesma aposta **duas vezes** (um REFUND e depois um
ROLLBACK da mesma aposta). Isso é dar dinheiro de graça.

**Nossa regra:** cada aposta pode ser desfeita **uma vez só**. E quem garante é o
banco, com uma regra de unicidade.

💬 *"Cada operação pode ser revertida no máximo uma vez, garantido por um índice único no banco. Assim nunca devolvo o mesmo dinheiro duas vezes."*

---

## ⏳ 9. Referência que ainda não chegou + backoff

**Analogia:** chega uma carta dizendo "cancele o pedido 123", mas o pedido 123
ainda não chegou ao correio. Não faz sentido rasgar a carta: você **guarda** e
confere de novo daqui a pouco.

Na internet, as mensagens podem chegar **fora de ordem**. Então:

1. O ROLLBACK chega antes da BET → salvamos como `PENDING_REFERENCE` ("esperando").
2. Um **worker** (um processo em segundo plano) volta para conferir de tempos em tempos.
3. A BET chegou → aplica o ROLLBACK.
4. Passou muito tempo e nada → desiste e marca como rejeitado (`REFERENCE_NOT_FOUND`).

**Backoff exponencial:** esperar cada vez mais entre as tentativas (1s, 2s, 4s,
8s, 16s...), para não ficar martelando à toa. O **jitter** é um pouco de
aleatoriedade para que os 3 servidores não tentem todos no mesmo segundo.

💬 *"Reversão que chega antes da referência fica pendente, e um worker tenta de novo com backoff exponencial até um limite. Depois disso, rejeita."*

---

## 📬 10. Fila (SQS), DLQ e FIFO

**Analogia:** a fila é uma **caixa de correio** entre sistemas. O provedor deixa a
carta, e nós pegamos quando der. Se nosso servidor estiver ocupado ou fora do ar, a
carta fica esperando lá.

Como o SQS funciona:

1. Pegamos uma carta e ela fica **escondida** por 30s (visibility timeout), para
   outro servidor não pegar a mesma.
2. Processou? **Jogamos a carta fora** (delete).
3. Não jogou fora (o servidor morreu)? Depois dos 30s a carta **reaparece** e
   alguém tenta de novo. Daí vem a repetição do item 4.
4. Falhou 5 vezes? Vai para a **DLQ** (Dead Letter Queue), a "caixa de cartas
   problemáticas", para um humano olhar.

**FIFO** = "primeiro a entrar, primeiro a sair", ou seja, mantém a ordem. Agrupamos
por carteira: dentro de uma carteira a ordem é respeitada, e carteiras diferentes
andam em paralelo.

**LocalStack** é um programa que simula a AWS no seu computador, para não precisar
de conta na Amazon.

💬 *"Consumo do SQS e só apago a mensagem depois de gravar no banco. Se o servidor cair, ela volta. Depois de várias falhas, vai para a DLQ."*

---

## 📥 11. Inbox

**Analogia:** um **caderno de protocolo** na portaria: "carta nº msg-123 recebida e
tratada ✔".

Antes de processar uma mensagem da fila, anotamos o ID dela no caderno (uma tabela
no banco), **na mesma transação** em que mexemos no saldo. Se a carta voltar,
olhamos o caderno: "já tratei essa". Então só jogamos fora, sem processar de novo.

Por que importa: o servidor pode morrer **depois** de gravar no banco e **antes**
de apagar da fila. A mensagem volta, mas o caderno salva a gente.

💬 *"A inbox registra cada mensagem processada na mesma transação da operação. Uma reentrega é reconhecida e descartada."*

---

## 📤 12. Outbox (o conceito mais importante para explicar bem)

Depois de debitar, precisamos **avisar outros sistemas** ("o saldo do jogador
mudou"). Isso é um **evento** publicado numa fila.

**O problema (dual write):** são dois lugares, o banco e a fila, e não dá para
gravar nos dois "ao mesmo tempo".

- Se **avisar antes** de gravar e a gravação falhar → avisamos uma coisa que **não
  aconteceu**. ❌ (Eliminatório no desafio.)
- Se **gravar e depois avisar** e o servidor morrer no meio → o aviso **se perde
  para sempre**. ❌

**Solução, com analogia:** em vez de ligar direto para o cliente, você **anota o
recado num post-it e cola na mesma pasta** do pedido. Depois, um estagiário (o
worker) passa recolhendo os post-its e faz as ligações.

1. Na **mesma transação** do saldo, gravamos o evento numa tabela `outbox` (o
   post-it). Se o saldo foi gravado, o post-it também foi. Se não, nenhum dos dois.
2. Um **worker separado** lê a outbox e publica na fila.
3. Publicou → marca "enviado".
4. Morreu entre publicar e marcar? Publica **de novo**, mas com o **mesmo ID de
   evento** (`eventId`). Quem recebe vê que é repetido e ignora.

Com 3 servidores, cada um "reserva" post-its diferentes (`SKIP LOCKED` = "pula os
que outro já pegou"). Se um morrer segurando post-its, a reserva expira e outro assume.

💬 *"Uso transactional outbox: o evento é gravado na mesma transação do saldo e publicado depois por um worker. Nunca publico algo que não foi confirmado e nunca perco um evento confirmado. Republicações mantêm o mesmo eventId."*

---

## 🔐 13. Autenticação: OAuth, Keycloak, JWT

- **Autenticação** = *quem é você?* (crachá)
- **Autorização** = *o que você pode fazer?* (quais portas o crachá abre)

**Keycloak** é a **portaria** que emite crachás. Nós não cadastramos senhas:
confiamos na portaria.

**client_credentials** é o login de **sistema para sistema** (não tem humano). O
provedor tem um usuário e uma senha de máquina, pede um crachá ao Keycloak e recebe
um **JWT**.

**JWT** é o crachá digital: um texto assinado que diz "este é o provider-a, válido
até 15h". Nós conferimos a **assinatura** (para saber que não é falso) e a **validade**.

**Regra de ouro do desafio:** quem você é vem do **crachá**, não do que você
escreve no pedido. Se o provider-a mandar `"providerId": "provider-b"` no corpo,
recusamos (403). Ninguém mexe nas transações dos outros.

💬 *"O Keycloak emite tokens via client_credentials. Valido a assinatura e extraio o providerId do token, nunca do body, e cada provedor só acessa os próprios dados."*

---

## 🧩 14. Uber Fx (injeção de dependência e ciclo de vida)

**Analogia:** montar um carro. O motor precisa de combustível, as rodas precisam do
eixo... Em vez de você montar na mão e na ordem certa, o Fx é a **linha de
montagem**: você diz "para fazer X eu preciso de Y" e ele monta tudo na ordem.

```go
func NewWalletService(db *pgxpool.Pool) *WalletService // "preciso de um banco"
```

O Fx vê isso e entrega o banco pronto.

**Ciclo de vida:** ligar e desligar na ordem certa.

- **Ligar:** banco → fila → servidor HTTP → workers.
- **Desligar (SIGTERM):** para de aceitar pedidos novos → termina o que estava
  fazendo → **por último** fecha o banco. Desligar o banco antes quebraria o
  trabalho em andamento.

💬 *"Uso Fx para montar as dependências e controlar o ciclo de vida. No shutdown, paro de aceitar trabalho, termino o que está em andamento e só depois fecho as conexões."*

---

## 📊 15. Observabilidade

É o **painel do carro**: velocímetro, luz de óleo...

- **Logs:** um diário do que aconteceu, com os IDs para rastrear uma operação específica.
- **Métricas:** números ao longo do tempo (quantas apostas por minuto, quantas
  falharam, quantos eventos estão atrasados).
- **Health check:** "está vivo?" e "está pronto?" (o banco e a fila respondem?).

### Os três tipos de métrica

| Tipo | Analogia | Exemplo no projeto |
|---|---|---|
| **Counter** (contador) | o **hodômetro** do carro: só sobe | `wager_transactions_total`: quantas apostas já foram processadas |
| **Gauge** (medidor) | o **marcador de combustível**: sobe e desce | `outbox_lag_seconds`: há quanto tempo o evento mais antigo espera para sair |
| **Histogram** | as **faixas de tempo de entrega** de um app de comida: quantos pedidos chegaram em até 10 min, até 30 min... | `wager_processing_duration_seconds`: quanto tempo cada operação levou |

O Prometheus "passa na loja" a cada poucos segundos, lê o `/metrics` e guarda
os números. Com isso dá para fazer gráfico e alarme ("mais de 5 conflitos de
lock por minuto", "outbox atrasada mais de 1 minuto").

**Cardinalidade:** cada combinação de rótulos vira uma série guardada para
sempre. Por isso **nunca** se usa o id da carteira como rótulo: seria como
criar uma coluna nova na planilha para cada cliente. Usamos o **molde** da
rota (`/wallets/{walletId}`), não o endereço com o id.

### correlationId: o número de protocolo

É como o **número de protocolo** de uma central de atendimento: cada setor que
mexe no seu caso anota o mesmo número. Aqui, a requisição (ou a mensagem da
fila) recebe um id, e **todo log e todo evento** daquela operação carrega esse
id. Para investigar um problema, basta filtrar por ele.

### Reconciliação: contar o caixa com uma foto

**Analogia:** você vai conferir o caixa de uma loja aberta. Conta as notas da
gaveta e depois soma os recibos. Se uma venda acontece **entre** as duas
contagens, os números não batem, e não é roubo, é só o momento errado.

A solução é tirar uma **foto** da loja num instante e contar tudo nela. No
banco, essa foto é uma transação `REPEATABLE READ` somente leitura: o saldo e
a soma do extrato são lidos no mesmo instante. Testamos: sem a foto, 91 de
1530 conferências feitas durante apostas acusaram um erro que não existia.

E a reconciliação **só confere, nunca conserta**. Se não bater, ela avisa
(na resposta, num log de erro e numa métrica), e a correção é decisão humana,
com um lançamento novo no extrato.

💬 *"Logs estruturados com IDs de rastreio, métricas de negócio e de falhas, e health checks de liveness e readiness. A reconciliação confere saldo contra extrato numa foto consistente do banco e só avisa, nunca corrige sozinha."*

---

## 🎯 Resumo para explicar o plano em 30 segundos

> "Recebo operações de provedores por HTTP e por fila. Assumo que tudo pode chegar
> repetido, fora de ordem e ao mesmo tempo. Dinheiro em centavos inteiros.
> Idempotência por chave única no banco. Concorrência com lock por carteira no
> Postgres. Tudo (saldo, extrato imutável, registro da mensagem e eventos) gravado
> numa transação só. Eventos são publicados depois do commit via outbox.
> Autenticação pelo Keycloak, com o provedor vindo do token. O Fx monta tudo e
> desliga na ordem certa. A reconciliação prova que o saldo bate com o extrato,
> e as métricas mostram duplicatas, conflitos, DLQ e atraso da outbox."

---

## 🎥 Vídeos e leituras para estudar

Nenhum destes vídeos foi assistido para conferir a qualidade. A maioria está em
inglês, e dá para ativar a legenda automática em português no YouTube.

**Ordem sugerida:** idempotência → lock → outbox/inbox → o resto. São os três
conceitos que mais pesam na nota.

### Vídeos

**Outbox:**

- [Transactional Outbox Pattern in System Design](https://www.youtube.com/watch?v=T5pu0lH2Dwc)
- [What is the Transactional Outbox Pattern?](https://www.youtube.com/watch?v=5YLpjPmsPCA)
- [Transactional Outbox Pattern Explained](https://www.youtube.com/watch?v=B2W-xDcrCls)
- Canal **CodeOpinion**, vídeo "Reliably Save State & Publish Events (Outbox Pattern)"
  (buscar o título no YouTube)

**Ledger:** vídeos de contabilidade, não de programação, mas que mostram bem a ideia
de que todo dinheiro que entra ou sai vira uma linha no extrato.

- [Double Entry and Ledger Accounts Explained](https://www.youtube.com/watch?v=rWtJ3avCsP8)
- [What is Double Entry Accounting?](https://www.youtube.com/watch?v=uprjZ2Se0e8)

### Buscas em português no YouTube

A dica de ouro é a **Rinha de Backend 2024**: um desafio brasileiro muito parecido,
com créditos e débitos simultâneos na mesma conta sem deixar o saldo estourar. Tem
muitos vídeos em português, inclusive em Go.

| Conceito | O que buscar |
|---|---|
| Concorrência / lock | `rinha de backend 2024 go`, `select for update postgres`, `lock pessimista otimista` |
| Idempotência | `idempotência api` (há um vídeo do Augusto Galego sobre o tema) |
| Outbox / Inbox | `outbox pattern full cycle`, `outbox pattern português` |
| Fila / SQS / DLQ | `sqs dead letter queue`, `mensageria at least once` |
| Keycloak / JWT | `keycloak client credentials`, `jwt explicado` |
| Uber Fx | `uber fx golang` |

### Leituras curtas em português

- [Transactional Outbox: resolvendo o Dual Write](https://dev.to/lzocate-li/transactional-outbox-pattern-resolvendo-o-dual-write-4923)
- [Consistência de dados e padrão Outbox](https://dev.to/rafaelpadovezi/consistencia-de-dados-e-padrao-outbox-188p)
- [Garantindo idempotência com o padrão Inbox](https://dev.to/actor-dev/garantindo-idempotencia-com-o-padrao-inbox-id1)
- [Pagamentos idempotentes: study case](https://dev.to/tarcisioaraujo/pagamentos-idempotentes-um-study-case-de-arquitetura-com-redis-e-banco-de-dados-3n06)
- [API idempotente (TabNews)](https://www.tabnews.com.br/iamferraz/api-idempotente-garanta-operacoes-sem-duplicacao-entenda-a-importancia-da-idempotencia-em-apis)
- [Outbox Pattern: O que é isso? (slides)](https://speakerdeck.com/eusouodaniel/outbox-pattern-o-que-e-isso)
- [Padrões de sistemas distribuídos: Outbox](https://typefully.com/mateuscviegas/padroes-de-sistemas-distribuidos-outbox-juCOZcd)
