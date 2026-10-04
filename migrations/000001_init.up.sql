-- 000001_init: schema financeiro com invariantes impostas pelo banco.
--
-- Papéis (criados fora da migration, ver deploy/postgres/01-roles.sql):
--   wallet_owner  dono das tabelas; executa as migrations
--   wallet_app    usado pela aplicação; sem UPDATE/DELETE no ledger
--
-- Dinheiro: BIGINT em unidades mínimas (centavos) + CHAR(3) ISO 4217.

-- =====================================================================
-- wallets
-- =====================================================================
CREATE TABLE wallets (
    id            UUID        PRIMARY KEY,
    player_id     UUID        NOT NULL,
    currency      CHAR(3)     NOT NULL CHECK (currency ~ '^[A-Z]{3}$'),
    balance_minor BIGINT      NOT NULL CHECK (balance_minor >= 0),
    version       BIGINT      NOT NULL CHECK (version >= 1),
    created_at    TIMESTAMPTZ NOT NULL,
    updated_at    TIMESTAMPTZ NOT NULL,
    CONSTRAINT wallets_player_currency_uq UNIQUE (player_id, currency),
    -- alvo da FK composta do ledger (garante moeda do lançamento = moeda da carteira)
    CONSTRAINT wallets_id_currency_uq UNIQUE (id, currency),
    CONSTRAINT wallets_updated_after_created CHECK (updated_at >= created_at)
);

-- Identidade imutável e versão coerente com a mudança de saldo:
-- saldo mudou  => version = OLD.version + 1
-- saldo igual  => version = OLD.version
CREATE FUNCTION wallets_guard_update() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.id <> OLD.id OR NEW.player_id <> OLD.player_id
       OR NEW.currency <> OLD.currency OR NEW.created_at <> OLD.created_at THEN
        RAISE EXCEPTION 'wallets: id, player_id, currency and created_at are immutable'
            USING ERRCODE = 'check_violation';
    END IF;
    IF NEW.balance_minor <> OLD.balance_minor AND NEW.version <> OLD.version + 1 THEN
        RAISE EXCEPTION 'wallets: version must increase by exactly 1 when the balance changes'
            USING ERRCODE = 'check_violation';
    END IF;
    IF NEW.balance_minor = OLD.balance_minor AND NEW.version <> OLD.version THEN
        RAISE EXCEPTION 'wallets: version must not change without a balance change'
            USING ERRCODE = 'check_violation';
    END IF;
    RETURN NEW;
END $$;

CREATE TRIGGER wallets_guard_update
    BEFORE UPDATE ON wallets
    FOR EACH ROW EXECUTE FUNCTION wallets_guard_update();

-- =====================================================================
-- wager_transactions
-- =====================================================================
CREATE TABLE wager_transactions (
    id                                UUID        PRIMARY KEY,
    origin                            TEXT        NOT NULL CHECK (origin IN ('INTERNAL', 'EXTERNAL')),
    kind                              TEXT        NOT NULL CHECK (kind IN ('OPENING', 'BET', 'WIN', 'LOSS', 'REFUND', 'ROLLBACK')),
    status                            TEXT        NOT NULL CHECK (status IN ('PENDING', 'PENDING_REFERENCE', 'PROCESSED', 'REJECTED', 'FAILED')),
    -- sem FK para wallets: uma operação rejeitada com WALLET_NOT_FOUND também
    -- precisa ser persistida (para replay idempotente) e aponta para uma
    -- carteira inexistente.
    wallet_id                         UUID        NOT NULL,
    player_id                         UUID        NOT NULL,
    amount_minor                      BIGINT      NOT NULL CHECK (amount_minor >= 0),
    currency                          CHAR(3)     NOT NULL CHECK (currency ~ '^[A-Z]{3}$'),

    -- metadados externos (NULL em OPENING)
    provider_id                       TEXT,
    external_transaction_id           TEXT,
    idempotency_key                   TEXT,
    payload_hash                      TEXT,
    round_id                          TEXT,
    game_id                           TEXT,
    reference_external_transaction_id TEXT,

    -- resultado
    reference_transaction_id          UUID        REFERENCES wager_transactions (id),
    failure_code                      TEXT,
    balance_after_minor               BIGINT      CHECK (balance_after_minor >= 0),

    -- espera por referência
    attempts                          INTEGER     NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    next_attempt_at                   TIMESTAMPTZ,

    created_at                        TIMESTAMPTZ NOT NULL,
    updated_at                        TIMESTAMPTZ NOT NULL,

    CONSTRAINT wager_tx_updated_after_created CHECK (updated_at >= created_at),

    -- operações internas e externas são distinguidas pelo schema
    CONSTRAINT wager_tx_origin_shape CHECK (
        (origin = 'INTERNAL' AND kind = 'OPENING'
            AND provider_id IS NULL AND external_transaction_id IS NULL
            AND idempotency_key IS NULL AND payload_hash IS NULL
            AND round_id IS NULL AND game_id IS NULL
            AND reference_external_transaction_id IS NULL
            AND reference_transaction_id IS NULL)
        OR
        (origin = 'EXTERNAL' AND kind <> 'OPENING'
            AND provider_id IS NOT NULL AND provider_id <> ''
            AND external_transaction_id IS NOT NULL AND external_transaction_id <> ''
            AND idempotency_key IS NOT NULL AND idempotency_key <> ''
            AND payload_hash IS NOT NULL AND payload_hash <> ''
            AND round_id IS NOT NULL AND round_id <> ''
            AND game_id IS NOT NULL AND game_id <> '')
    ),

    -- política de valor zero: só LOSS é zero; LOSS é sempre zero
    CONSTRAINT wager_tx_amount_policy CHECK (
        (kind = 'LOSS' AND amount_minor = 0) OR (kind <> 'LOSS' AND amount_minor > 0)
    ),

    -- referência obrigatória em REFUND/ROLLBACK, opcional em WIN, proibida no resto
    CONSTRAINT wager_tx_reference_policy CHECK (
        (kind IN ('REFUND', 'ROLLBACK') AND reference_external_transaction_id IS NOT NULL)
        OR (kind = 'WIN')
        OR (kind IN ('OPENING', 'BET', 'LOSS') AND reference_external_transaction_id IS NULL
            AND reference_transaction_id IS NULL)
    ),

    -- campos exigidos por estado
    CONSTRAINT wager_tx_status_shape CHECK (
        CASE status
            WHEN 'PROCESSED'         THEN balance_after_minor IS NOT NULL AND failure_code IS NULL
            WHEN 'REJECTED'          THEN failure_code IS NOT NULL
            WHEN 'FAILED'            THEN failure_code IS NOT NULL
            WHEN 'PENDING_REFERENCE' THEN next_attempt_at IS NOT NULL AND failure_code IS NULL
            ELSE failure_code IS NULL
        END
    )
);

-- idempotência: uma operação financeira por (provedor, id externo) e uma
-- chave por provedor. Linhas internas têm NULL e não colidem.
CREATE UNIQUE INDEX wager_tx_provider_external_uq
    ON wager_transactions (provider_id, external_transaction_id);
CREATE UNIQUE INDEX wager_tx_provider_idempotency_key_uq
    ON wager_transactions (provider_id, idempotency_key);

-- no máximo um crédito de abertura por carteira
CREATE UNIQUE INDEX wager_tx_one_opening_per_wallet
    ON wager_transactions (wallet_id) WHERE kind = 'OPENING';

-- no máximo uma reversão bem-sucedida por transação referenciada
-- (REFUND ou ROLLBACK; impede devolver o mesmo débito duas vezes)
CREATE UNIQUE INDEX wager_tx_one_reversal_per_reference
    ON wager_transactions (reference_transaction_id)
    WHERE kind IN ('REFUND', 'ROLLBACK') AND status = 'PROCESSED';

-- busca de referências e varredura do worker de pendências
CREATE INDEX wager_tx_provider_reference_idx
    ON wager_transactions (provider_id, reference_external_transaction_id)
    WHERE reference_external_transaction_id IS NOT NULL;
CREATE INDEX wager_tx_due_idx
    ON wager_transactions (next_attempt_at)
    WHERE status IN ('PENDING', 'PENDING_REFERENCE');
CREATE INDEX wager_tx_wallet_idx ON wager_transactions (wallet_id, created_at);

-- Máquina de estados e imutabilidade no banco:
--  * colunas de identidade e de negócio nunca mudam;
--  * estado terminal (PROCESSED/REJECTED/FAILED) não aceita nenhuma alteração;
--  * só as transições abaixo são aceitas (PENDING_REFERENCE -> PENDING_REFERENCE
--    é o reagendamento de tentativa).
CREATE FUNCTION wager_tx_guard_update() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF OLD.status IN ('PROCESSED', 'REJECTED', 'FAILED') THEN
        RAISE EXCEPTION 'wager_transactions: % is terminal and cannot change', OLD.status
            USING ERRCODE = 'check_violation';
    END IF;

    IF (NEW.id, NEW.origin, NEW.kind, NEW.wallet_id, NEW.player_id, NEW.amount_minor, NEW.currency,
        NEW.provider_id, NEW.external_transaction_id, NEW.idempotency_key, NEW.payload_hash,
        NEW.round_id, NEW.game_id, NEW.reference_external_transaction_id, NEW.created_at)
       IS DISTINCT FROM
       (OLD.id, OLD.origin, OLD.kind, OLD.wallet_id, OLD.player_id, OLD.amount_minor, OLD.currency,
        OLD.provider_id, OLD.external_transaction_id, OLD.idempotency_key, OLD.payload_hash,
        OLD.round_id, OLD.game_id, OLD.reference_external_transaction_id, OLD.created_at) THEN
        RAISE EXCEPTION 'wager_transactions: business fields are immutable'
            USING ERRCODE = 'check_violation';
    END IF;

    IF NOT (
        (OLD.status = 'PENDING' AND NEW.status IN ('PROCESSED', 'REJECTED', 'FAILED', 'PENDING_REFERENCE'))
        OR (OLD.status = 'PENDING_REFERENCE' AND NEW.status IN ('PROCESSED', 'REJECTED', 'FAILED', 'PENDING_REFERENCE'))
    ) THEN
        RAISE EXCEPTION 'wager_transactions: invalid transition % -> %', OLD.status, NEW.status
            USING ERRCODE = 'check_violation';
    END IF;

    RETURN NEW;
END $$;

CREATE TRIGGER wager_tx_guard_update
    BEFORE UPDATE ON wager_transactions
    FOR EACH ROW EXECUTE FUNCTION wager_tx_guard_update();

-- =====================================================================
-- wallet_ledger_entries (append-only)
-- =====================================================================
CREATE TABLE wallet_ledger_entries (
    id                   UUID        PRIMARY KEY,
    wallet_id            UUID        NOT NULL,
    transaction_id       UUID        NOT NULL REFERENCES wager_transactions (id),
    -- versão da carteira produzida por este lançamento; dá ordem estável e
    -- encadeia os lançamentos (ver ledger_guard_insert)
    wallet_version       BIGINT      NOT NULL CHECK (wallet_version >= 1),
    direction            TEXT        NOT NULL CHECK (direction IN ('DEBIT', 'CREDIT')),
    amount_minor         BIGINT      NOT NULL CHECK (amount_minor > 0),
    currency             CHAR(3)     NOT NULL,
    balance_before_minor BIGINT      NOT NULL CHECK (balance_before_minor >= 0),
    balance_after_minor  BIGINT      NOT NULL CHECK (balance_after_minor >= 0),
    created_at           TIMESTAMPTZ NOT NULL,

    CONSTRAINT ledger_wallet_currency_fk
        FOREIGN KEY (wallet_id, currency) REFERENCES wallets (id, currency),
    CONSTRAINT ledger_wallet_transaction_uq UNIQUE (wallet_id, transaction_id),
    CONSTRAINT ledger_wallet_version_uq UNIQUE (wallet_id, wallet_version),
    CONSTRAINT ledger_arithmetic CHECK (
        (direction = 'CREDIT' AND balance_after_minor = balance_before_minor + amount_minor)
        OR (direction = 'DEBIT' AND balance_after_minor = balance_before_minor - amount_minor)
    )
);

-- Encadeamento: o saldo anterior de um lançamento é o saldo posterior do
-- lançamento da versão anterior (ou zero, se não houver). A transação do
-- lançamento precisa ser da mesma carteira.
CREATE FUNCTION ledger_guard_insert() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    prev_after BIGINT;
    tx_wallet  UUID;
BEGIN
    SELECT wallet_id INTO tx_wallet FROM wager_transactions WHERE id = NEW.transaction_id;
    IF tx_wallet IS DISTINCT FROM NEW.wallet_id THEN
        RAISE EXCEPTION 'ledger: transaction % does not belong to wallet %', NEW.transaction_id, NEW.wallet_id
            USING ERRCODE = 'check_violation';
    END IF;

    SELECT balance_after_minor INTO prev_after
      FROM wallet_ledger_entries
     WHERE wallet_id = NEW.wallet_id AND wallet_version < NEW.wallet_version
     ORDER BY wallet_version DESC
     LIMIT 1;

    IF NEW.balance_before_minor <> COALESCE(prev_after, 0) THEN
        RAISE EXCEPTION 'ledger: balance_before (%) does not match previous balance_after (%)',
            NEW.balance_before_minor, COALESCE(prev_after, 0)
            USING ERRCODE = 'check_violation';
    END IF;
    RETURN NEW;
END $$;

CREATE TRIGGER ledger_guard_insert
    BEFORE INSERT ON wallet_ledger_entries
    FOR EACH ROW EXECUTE FUNCTION ledger_guard_insert();

-- Append-only: nenhuma alteração ou exclusão, nem pelo dono da tabela.
CREATE FUNCTION forbid_mutation() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION '%: % is not allowed (append-only)', TG_TABLE_NAME, TG_OP
        USING ERRCODE = 'insufficient_privilege';
END $$;

CREATE TRIGGER ledger_no_update_delete
    BEFORE UPDATE OR DELETE ON wallet_ledger_entries
    FOR EACH ROW EXECUTE FUNCTION forbid_mutation();
CREATE TRIGGER ledger_no_truncate
    BEFORE TRUNCATE ON wallet_ledger_entries
    FOR EACH STATEMENT EXECUTE FUNCTION forbid_mutation();

-- Registros financeiros também não são apagados.
CREATE TRIGGER wallets_no_delete
    BEFORE DELETE ON wallets
    FOR EACH ROW EXECUTE FUNCTION forbid_mutation();
CREATE TRIGGER wallets_no_truncate
    BEFORE TRUNCATE ON wallets
    FOR EACH STATEMENT EXECUTE FUNCTION forbid_mutation();
CREATE TRIGGER wager_tx_no_delete
    BEFORE DELETE ON wager_transactions
    FOR EACH ROW EXECUTE FUNCTION forbid_mutation();
CREATE TRIGGER wager_tx_no_truncate
    BEFORE TRUNCATE ON wager_transactions
    FOR EACH STATEMENT EXECUTE FUNCTION forbid_mutation();

-- =====================================================================
-- Saldo = ledger, verificado no COMMIT
-- =====================================================================
-- Toda carteira com saldo diferente de zero ou versão acima de 1 precisa ter
-- o lançamento da sua versão atual, com balance_after igual ao saldo. Como o
-- gatilho é adiado (DEFERRED), a aplicação pode gravar carteira e lançamento
-- em qualquer ordem dentro da transação; o COMMIT falha se não baterem.
CREATE FUNCTION wallets_check_ledger() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    cur_balance BIGINT;
    cur_version BIGINT;
BEGIN
    -- relê a linha: se ela mudou de novo na mesma transação, vale o estado final
    SELECT balance_minor, version INTO cur_balance, cur_version FROM wallets WHERE id = NEW.id;
    IF cur_balance = 0 AND cur_version = 1 THEN
        RETURN NULL; -- carteira aberta com saldo zero: sem lançamento
    END IF;
    IF NOT EXISTS (
        SELECT 1 FROM wallet_ledger_entries
         WHERE wallet_id = NEW.id
           AND wallet_version = cur_version
           AND balance_after_minor = cur_balance
    ) THEN
        RAISE EXCEPTION 'wallets: balance % at version % has no matching ledger entry', cur_balance, cur_version
            USING ERRCODE = 'check_violation';
    END IF;
    RETURN NULL;
END $$;

CREATE CONSTRAINT TRIGGER wallets_check_ledger
    AFTER INSERT OR UPDATE ON wallets
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW EXECUTE FUNCTION wallets_check_ledger();

-- =====================================================================
-- inbox_messages
-- =====================================================================
CREATE TABLE inbox_messages (
    consumer_name TEXT        NOT NULL CHECK (consumer_name <> ''),
    message_id    TEXT        NOT NULL CHECK (message_id <> ''),
    payload_hash  TEXT        NOT NULL CHECK (payload_hash <> ''),
    received_at   TIMESTAMPTZ NOT NULL,
    completed_at  TIMESTAMPTZ,
    PRIMARY KEY (consumer_name, message_id),
    CONSTRAINT inbox_completed_after_received CHECK (completed_at IS NULL OR completed_at >= received_at)
);

-- =====================================================================
-- outbox_events
-- =====================================================================
CREATE TABLE outbox_events (
    event_id        UUID        PRIMARY KEY,
    aggregate_type  TEXT        NOT NULL CHECK (aggregate_type IN ('wallet', 'wager_transaction')),
    aggregate_id    UUID        NOT NULL,
    event_type      TEXT        NOT NULL,
    event_version   INTEGER     NOT NULL CHECK (event_version >= 1),
    correlation_id  TEXT        NOT NULL,
    causation_id    UUID,
    payload         JSONB       NOT NULL,
    occurred_at     TIMESTAMPTZ NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    attempts        INTEGER     NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    next_attempt_at TIMESTAMPTZ NOT NULL,
    locked_until    TIMESTAMPTZ,
    locked_by       TEXT,
    published_at    TIMESTAMPTZ,
    last_error      TEXT
);

-- varredura do publisher: só o que falta publicar
CREATE INDEX outbox_unpublished_idx
    ON outbox_events (next_attempt_at)
    WHERE published_at IS NULL;

-- O payload é um snapshot imutável; um evento publicado não volta a pendente.
CREATE FUNCTION outbox_guard_update() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF (NEW.event_id, NEW.aggregate_type, NEW.aggregate_id, NEW.event_type, NEW.event_version,
        NEW.correlation_id, NEW.causation_id, NEW.payload, NEW.occurred_at, NEW.created_at)
       IS DISTINCT FROM
       (OLD.event_id, OLD.aggregate_type, OLD.aggregate_id, OLD.event_type, OLD.event_version,
        OLD.correlation_id, OLD.causation_id, OLD.payload, OLD.occurred_at, OLD.created_at) THEN
        RAISE EXCEPTION 'outbox_events: event identity and payload are immutable'
            USING ERRCODE = 'check_violation';
    END IF;
    IF OLD.published_at IS NOT NULL AND NEW.published_at IS DISTINCT FROM OLD.published_at THEN
        RAISE EXCEPTION 'outbox_events: published_at cannot change once set'
            USING ERRCODE = 'check_violation';
    END IF;
    RETURN NEW;
END $$;

CREATE TRIGGER outbox_guard_update
    BEFORE UPDATE ON outbox_events
    FOR EACH ROW EXECUTE FUNCTION outbox_guard_update();

-- =====================================================================
-- Permissões do papel da aplicação
-- =====================================================================
REVOKE ALL ON wallets, wager_transactions, wallet_ledger_entries, inbox_messages, outbox_events FROM PUBLIC;

GRANT SELECT, INSERT, UPDATE ON wallets            TO wallet_app;
GRANT SELECT, INSERT, UPDATE ON wager_transactions TO wallet_app;
GRANT SELECT, INSERT         ON wallet_ledger_entries TO wallet_app; -- sem UPDATE/DELETE
GRANT SELECT, INSERT, UPDATE ON inbox_messages     TO wallet_app;
GRANT SELECT, INSERT, UPDATE ON outbox_events      TO wallet_app;
