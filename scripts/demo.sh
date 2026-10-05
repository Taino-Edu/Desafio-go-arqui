#!/usr/bin/env bash
# Demonstração ponta a ponta contra o ambiente do docker compose:
#
#   docker compose up -d --build
#   ./scripts/demo.sh
#
# Percorre o fluxo autenticado (Keycloak real), HTTP e SQS, e confere cada
# resposta: sai com erro se algo não for o esperado. Requer curl e jq.
set -euo pipefail

API=${API:-http://localhost:${APP_PORT:-8080}}
IDP=${IDP:-http://localhost:${KEYCLOAK_PORT:-8081}/realms/wallet/protocol/openid-connect/token}
RUN=$(date +%s)  # sufixo: a demonstração pode rodar várias vezes

step() { printf '\n\033[1m== %s\033[0m\n' "$*"; }
fail() { printf '\033[31mFALHOU: %s\033[0m\n' "$*" >&2; exit 1; }

token() {
  curl -sf -d grant_type=client_credentials -d client_id="$1" -d client_secret="$2" "$IDP" | jq -r .access_token
}

# call MÉTODO CAMINHO TOKEN [CORPO] [CHAVE] -> preenche $STATUS e $BODY
# (sem pipe: um pipe rodaria a função num subshell e perderia as variáveis)
TMP=$(mktemp)
trap 'rm -f "$TMP"' EXIT
call() {
  local args=(-s -o "$TMP" -w '%{http_code}' -X "$1" "$API$2" -H 'Content-Type: application/json')
  if [[ -n "$3" ]]; then args+=(-H "Authorization: Bearer $3"); fi
  if [[ -n "${4:-}" ]]; then args+=(-d "$4"); fi
  if [[ -n "${5:-}" ]]; then args+=(-H "Idempotency-Key: $5"); fi
  STATUS=$(curl "${args[@]}")
  BODY=$(cat "$TMP")
}
show() { jq -c "$1" <<<"$BODY"; }

expect() { [[ "$STATUS" == "$1" ]] || fail "esperava HTTP $1, veio $STATUS"; }

op() { # op TIPO ID_EXTERNO VALOR [REFERÊNCIA]
  jq -nc --arg kind "$1" --arg ext "$2" --arg amount "$3" --arg ref "${4:-}" \
    --arg player "$PLAYER" --arg wallet "$WALLET" '{
      providerId: "provider-a", externalTransactionId: $ext, playerId: $player, walletId: $wallet,
      roundId: "round-1", gameId: "fortune-chimp", kind: $kind,
      money: {amount: $amount, currency: "BRL"}
    } + (if $ref == "" then {} else {referenceExternalTransactionId: $ref} end)'
}

step "health (público)"
curl -sf "$API/health/ready" | jq -c . || fail "API não está pronta"

step "tokens (client_credentials no Keycloak)"
ADMIN=$(token wallet-service wallet-service-secret)
PROVIDER_A=$(token provider-a provider-a-secret)
PROVIDER_B=$(token provider-b provider-b-secret)
echo "ok: wallet-service, provider-a, provider-b"

step "sem token: 401"
call GET /wallets/00000000-0000-0000-0000-000000000001 ""; expect 401; echo "401"

step "abrir carteira com 1000.00 (serviço interno)"
PLAYER=$(cat /proc/sys/kernel/random/uuid 2>/dev/null || uuidgen)
call POST /wallets "$ADMIN" "{\"playerId\":\"$PLAYER\",\"initialBalance\":{\"amount\":\"1000.00\",\"currency\":\"BRL\"}}"
expect 201; show '{id, balance, version}'
WALLET=$(jq -r .id <<<"$BODY")

step "provedor não abre carteira: 403"
call POST /wallets "$PROVIDER_A" "{\"playerId\":\"$PLAYER\",\"initialBalance\":{\"amount\":\"1.00\",\"currency\":\"USD\"}}"; expect 403; echo 403

step "aposta de 25.00: 201"
BET="bet-$RUN"
call POST /wagering/transactions "$PROVIDER_A" "$(op BET "$BET" 25.00)" "provider-a:$BET"
expect 201; show '{status, balance, idempotentReplay}'

step "a mesma aposta de novo: 200, replay com o MESMO saldo"
call POST /wagering/transactions "$PROVIDER_A" "$(op BET "$BET" 25.00)" "provider-a:$BET"
expect 200; show '{status, balance, idempotentReplay}'

step "mesma chave com outro valor: 409"
call POST /wagering/transactions "$PROVIDER_A" "$(op BET "$BET" 99.00)" "provider-a:$BET"
expect 409; show .error

step "provedor B tentando agir como A: 403, nada gravado"
call POST /wagering/transactions "$PROVIDER_B" "$(op BET "intruso-$RUN" 1.00)" "provider-a:intruso-$RUN"; expect 403; echo 403

step "aposta maior que o saldo: 422 INSUFFICIENT_FUNDS (persistida)"
call POST /wagering/transactions "$PROVIDER_A" "$(op BET "big-$RUN" 5000.00)" "provider-a:big-$RUN"
expect 422; show '{status, failureCode}'

step "REFUND chega ANTES da aposta: 202 PENDING_REFERENCE"
LATE="late-$RUN"
call POST /wagering/transactions "$PROVIDER_A" "$(op REFUND "refund-$RUN" 10.00 "$LATE")" "provider-a:refund-$RUN"
expect 202; show '{status, nextAttemptAt}'

step "a aposta referenciada chega; o worker conclui o REFUND"
call POST /wagering/transactions "$PROVIDER_A" "$(op BET "$LATE" 10.00)" "provider-a:$LATE"
expect 201; show '{status, balance}'
for _ in $(seq 1 50); do
  call GET "/providers/provider-a/wagering/transactions/refund-$RUN" "$PROVIDER_A"; S=$(jq -r .status <<<"$BODY")
  [[ "$S" == PROCESSED ]] && break
  sleep 0.2
done
[[ "$S" == PROCESSED ]] || fail "REFUND não foi concluído (status $S)"
echo "REFUND: PROCESSED"

step "a mesma operação pela fila SQS (mesma idempotência do HTTP)"
SQS_EXT="sqs-$RUN"
MSG=$(jq -nc --arg id "msg-$RUN" --argjson data "$(op BET "$SQS_EXT" 5.00 | jq --arg k "provider-a:$SQS_EXT" '. + {idempotencyKey: $k}')" \
  '{messageId: $id, type: "WagerTransactionRequested", occurredAt: (now | todate), data: $data}')
docker compose exec -T localstack awslocal sqs send-message \
  --queue-url http://localhost:4566/000000000000/wager-transactions.fifo \
  --message-group-id "$WALLET" --message-deduplication-id "msg-$RUN" --message-body "$MSG" >/dev/null
for _ in $(seq 1 50); do
  call GET "/providers/provider-a/wagering/transactions/$SQS_EXT" "$PROVIDER_A"; S=$(jq -r '.status // empty' <<<"$BODY")
  [[ "$S" == PROCESSED ]] && break
  sleep 0.2
done
[[ "$S" == PROCESSED ]] || fail "mensagem SQS não foi processada"
echo "mensagem SQS: PROCESSED"

step "saldo e ledger"
call GET "/wallets/$WALLET" "$ADMIN"
expect 200; show '{balance, version}'
# 1000 - 25 - 10 + 10 - 5 = 970
[[ $(jq -r .balance.amount <<<"$BODY") == "970.00" ]] || fail "saldo diferente de 970.00"
call GET "/wallets/$WALLET/ledger" "$ADMIN"
jq -r '.items[] | "\(.walletVersion)  \(.direction)\t\(.money.amount)\t\(.balanceBefore.amount) -> \(.balanceAfter.amount)"' <<<"$BODY"

step "reconciliação: saldo armazenado x ledger x razão em partidas dobradas"
call POST "/wallets/$WALLET/reconciliation" "$ADMIN"
expect 200; show .
[[ $(jq -r .consistent <<<"$BODY") == "true" ]] || fail "reconciliação divergente"

step "balancete (partidas dobradas): Σ débitos = Σ créditos"
call GET "/accounting/trial-balance?currency=BRL" "$ADMIN"
expect 200
jq -r '.accounts[] | "\(.type)\t\(.account)\tD \(.debits.amount)\tC \(.credits.amount)\tsaldo \(.balance.amount)"' <<<"$BODY"
show '{balanced, totalDebits: .totalDebits.amount, totalCredits: .totalCredits.amount, wallets: .wallets.consistent}'
[[ $(jq -r '.balanced and .wallets.consistent' <<<"$BODY") == "true" ]] || fail "balancete não fecha"

step "métricas (público)"
curl -sf "$API/metrics" | grep -E '^(wager_transactions_total|idempotent_replays_total|idempotency_conflicts_total|sqs_messages_total|outbox_lag_seconds|reconciliations_total)' | head -12

printf '\n\033[32mdemonstração concluída: tudo como esperado\033[0m\n'
