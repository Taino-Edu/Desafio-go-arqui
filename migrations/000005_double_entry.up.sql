-- 000005_double_entry (EXPANSÃO): razão em partidas dobradas.
--
-- Cada movimentação vira um lançamento contábil em journal_postings: pelo
-- menos duas partidas, Σ débitos = Σ créditos. O ledger por carteira
-- (wallet_ledger_entries) continua sendo o extrato do jogador e passa a ser
-- exatamente a partida da conta da carteira em cada lançamento.
--
-- Plano de contas (ponto de vista da operadora):
--   wallet:<walletId>             LIABILITY  o que a casa deve ao jogador
--   cash:<moeda>                  ASSET      dinheiro que entrou (saldo de abertura)
--   provider:<providerId>:<moeda> REVENUE    apostas − prêmios do provedor (GGR)
--
-- As contas da casa NÃO guardam saldo: um saldo atualizado a cada aposta
-- seria uma linha disputada por todas as carteiras (um lock global). O saldo
-- delas é a soma das partidas.
--
-- Implantação em duas etapas (expandir / contrair), sem parar a API:
--   000005 (esta): só cria o que é novo. Instâncias antigas seguem
--          funcionando (não tocam nas tabelas novas); as novas gravam ledger
--          e razão juntos, e o razão novo já é conferido no COMMIT.
--   deploy da aplicação nova em todas as instâncias.
--   000006: preenche o histórico em lote e passa a EXIGIR a partida de todo
--          lançamento do ledger.

CREATE TABLE ledger_accounts (
    code        TEXT        PRIMARY KEY,
    type        TEXT        NOT NULL CHECK (type IN ('ASSET', 'LIABILITY', 'REVENUE')),
    currency    CHAR(3)     NOT NULL CHECK (currency ~ '^[A-Z]{3}$'),
    wallet_id   UUID        UNIQUE,
    provider_id TEXT        CHECK (provider_id <> ''),
    created_at  TIMESTAMPTZ NOT NULL,
    -- alvo da FK composta das partidas (moeda da partida = moeda da conta)
    CONSTRAINT ledger_accounts_code_currency_uq UNIQUE (code, currency),
    -- conta de carteira na moeda da carteira
    CONSTRAINT ledger_accounts_wallet_fk FOREIGN KEY (wallet_id, currency) REFERENCES wallets (id, currency),
    -- o código é derivado do tipo: não existe conta com nome enganoso
    CONSTRAINT ledger_accounts_shape CHECK (
        (type = 'LIABILITY' AND wallet_id IS NOT NULL AND provider_id IS NULL
            AND code = 'wallet:' || wallet_id::text)
        OR (type = 'ASSET' AND wallet_id IS NULL AND provider_id IS NULL
            AND code = 'cash:' || currency)
        OR (type = 'REVENUE' AND wallet_id IS NULL AND provider_id IS NOT NULL
            AND code = 'provider:' || provider_id || ':' || currency)
    )
);

-- As FKs (transação e conta) entram na 000006, DEPOIS do preenchimento do
-- histórico: assim são validadas numa consulta só, em vez de uma checagem
-- (com lock de linha) por partida, que custava ~18 s a cada 385 mil
-- lançamentos. Até lá, journal_check_entry exige transação e conta.
CREATE TABLE journal_postings (
    transaction_id UUID        NOT NULL,
    account_code   TEXT        NOT NULL,
    direction      TEXT        NOT NULL CHECK (direction IN ('DEBIT', 'CREDIT')),
    amount_minor   BIGINT      NOT NULL CHECK (amount_minor > 0),
    currency       CHAR(3)     NOT NULL,
    created_at     TIMESTAMPTZ NOT NULL,
    -- uma partida por conta em cada lançamento
    PRIMARY KEY (transaction_id, account_code)
);

-- extrato e saldo de uma conta (GGR de um provedor, caixa)
CREATE INDEX journal_postings_account_idx ON journal_postings (account_code);

-- Imutáveis, como o ledger: correção é um lançamento novo.
CREATE TRIGGER journal_no_update_delete
    BEFORE UPDATE OR DELETE ON journal_postings
    FOR EACH ROW EXECUTE FUNCTION forbid_mutation();
CREATE TRIGGER journal_no_truncate
    BEFORE TRUNCATE ON journal_postings
    FOR EACH STATEMENT EXECUTE FUNCTION forbid_mutation();
CREATE TRIGGER ledger_accounts_no_update_delete
    BEFORE UPDATE OR DELETE ON ledger_accounts
    FOR EACH ROW EXECUTE FUNCTION forbid_mutation();
CREATE TRIGGER ledger_accounts_no_truncate
    BEFORE TRUNCATE ON ledger_accounts
    FOR EACH STATEMENT EXECUTE FUNCTION forbid_mutation();

-- No COMMIT, cada lançamento precisa:
--  0. ser de uma transação existente, em contas existentes na moeda delas
--     (o papel das FKs até a 000006);
--  1. ter pelo menos duas partidas, numa moeda só, com Σ débitos = Σ créditos;
--  2. ter, para cada partida em conta de carteira, o lançamento idêntico no
--     ledger da carteira (mesma transação, direção e valor);
--  3. usar a contrapartida certa: a conta do provedor DA TRANSAÇÃO, e o
--     caixa só na abertura.
-- Adiado (DEFERRED): a aplicação grava as partidas e o ledger em qualquer
-- ordem dentro da transação. A 000006 aplica as mesmas regras em lote ao
-- histórico.
--
-- Custo: roda no COMMIT, ainda com a carteira travada, então cada consulta
-- aqui é fila para a próxima aposta da mesma carteira. Por isso as regras
-- são UMA consulta. O gatilho dispara por partida e cada disparo confere o
-- lançamento inteiro: pular os repetidos ("já conferi este") abriria um
-- desvio (uma partida acrescentada depois, noutra transação, ou depois de um
-- SET CONSTRAINTS ... IMMEDIATE, passaria sem conferência).
CREATE FUNCTION journal_check_entry() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    n_postings      INTEGER;
    n_currencies    INTEGER;
    net             NUMERIC;
    missing_ref     BOOLEAN;
    wallet_mismatch BOOLEAN;
    bad_counterpart BOOLEAN;
BEGIN
    SELECT count(*), count(DISTINCT p.currency),
           COALESCE(SUM(CASE p.direction WHEN 'DEBIT' THEN p.amount_minor ELSE -p.amount_minor END), 0),
           bool_or(a.code IS NULL OR t.id IS NULL),
           bool_or(a.wallet_id IS NOT NULL AND e.id IS NULL),
           bool_or((a.type = 'REVENUE' AND a.provider_id IS DISTINCT FROM t.provider_id)
                OR (a.type = 'ASSET' AND t.kind <> 'OPENING'))
      INTO n_postings, n_currencies, net, missing_ref, wallet_mismatch, bad_counterpart
      FROM journal_postings p
      LEFT JOIN ledger_accounts a ON a.code = p.account_code AND a.currency = p.currency
      LEFT JOIN wager_transactions t ON t.id = p.transaction_id
      LEFT JOIN wallet_ledger_entries e
             ON e.wallet_id = a.wallet_id AND e.transaction_id = p.transaction_id
            AND e.direction = p.direction AND e.amount_minor = p.amount_minor
     WHERE p.transaction_id = NEW.transaction_id;

    IF missing_ref THEN
        RAISE EXCEPTION 'journal: entry % references a missing transaction or account', NEW.transaction_id
            USING ERRCODE = 'foreign_key_violation';
    END IF;
    IF n_postings < 2 OR n_currencies <> 1 OR net <> 0 THEN
        RAISE EXCEPTION 'journal: entry % is not balanced (% postings, % currencies, debits - credits = %)',
            NEW.transaction_id, n_postings, n_currencies, net
            USING ERRCODE = 'check_violation';
    END IF;
    IF wallet_mismatch THEN
        RAISE EXCEPTION 'journal: wallet posting of % has no matching wallet ledger entry', NEW.transaction_id
            USING ERRCODE = 'check_violation';
    END IF;
    IF bad_counterpart THEN
        RAISE EXCEPTION 'journal: counterpart account of % does not match the transaction', NEW.transaction_id
            USING ERRCODE = 'check_violation';
    END IF;
    RETURN NULL;
END $$;

CREATE CONSTRAINT TRIGGER journal_check_entry
    AFTER INSERT ON journal_postings
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW EXECUTE FUNCTION journal_check_entry();

REVOKE ALL ON ledger_accounts, journal_postings FROM PUBLIC;
GRANT SELECT, INSERT ON ledger_accounts  TO wallet_app; -- sem UPDATE/DELETE
GRANT SELECT, INSERT ON journal_postings TO wallet_app;
