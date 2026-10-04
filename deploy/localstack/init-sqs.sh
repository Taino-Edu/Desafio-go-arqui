#!/bin/bash
# Executado pelo LocalStack quando fica pronto (/etc/localstack/init/ready.d).
# Cria as filas FIFO de entrada e de saída, cada uma com sua DLQ e redrive.
set -euo pipefail

REGION="${AWS_DEFAULT_REGION:-us-east-1}"
ACCOUNT="000000000000"
MAX_RECEIVE_COUNT="${SQS_MAX_RECEIVE_COUNT:-5}"
VISIBILITY_TIMEOUT="${SQS_VISIBILITY_TIMEOUT:-30}"

create_fifo_with_dlq() {
  local name="$1" dlq="$2"
  local dlq_arn="arn:aws:sqs:${REGION}:${ACCOUNT}:${dlq}"

  # DLQ guarda a mensagem por 14 dias para análise
  awslocal sqs create-queue --queue-name "$dlq" --attributes \
    "FifoQueue=true,ContentBasedDeduplication=false,MessageRetentionPeriod=1209600" >/dev/null

  awslocal sqs create-queue --queue-name "$name" --attributes "$(cat <<EOF
{
  "FifoQueue": "true",
  "ContentBasedDeduplication": "false",
  "VisibilityTimeout": "${VISIBILITY_TIMEOUT}",
  "MessageRetentionPeriod": "345600",
  "RedrivePolicy": "{\"deadLetterTargetArn\":\"${dlq_arn}\",\"maxReceiveCount\":\"${MAX_RECEIVE_COUNT}\"}"
}
EOF
)" >/dev/null
  echo "queue ${name} -> dlq ${dlq} (maxReceiveCount=${MAX_RECEIVE_COUNT}, visibility=${VISIBILITY_TIMEOUT}s)"
}

# entrada: operações dos provedores
create_fifo_with_dlq "wager-transactions.fifo" "wager-transactions-dlq.fifo"
# saída: eventos publicados pela outbox
create_fifo_with_dlq "wallet-events.fifo" "wallet-events-dlq.fifo"

# Política de acesso da fila de entrada (controle no broker). Na AWS, cada
# provedor publica com o próprio papel IAM e só o papel do consumidor recebe e
# apaga. O LocalStack Community aceita e guarda a política, mas não a aplica
# (aplicação de IAM é recurso pago); ela fica aqui como especificação.
INPUT_URL="http://localhost:4566/${ACCOUNT}/wager-transactions.fifo"
POLICY=$(cat /etc/localstack/policies/wager-transactions-policy.json 2>/dev/null || true)
if [ -n "$POLICY" ]; then
  awslocal sqs set-queue-attributes --queue-url "$INPUT_URL" \
    --attributes "{\"Policy\": $(printf '%s' "$POLICY" | python3 -c 'import json,sys;print(json.dumps(sys.stdin.read()))')}" >/dev/null
  echo "queue policy applied to wager-transactions.fifo"
fi

echo "sqs init done"
