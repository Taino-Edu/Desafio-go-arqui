-- 000006_double_entry_enforce (CONTRAÇÃO): histórico no razão e a regra
-- "todo lançamento do ledger tem a sua partida".
--
-- Rodar só depois que TODAS as instâncias gravam o razão (aplicação com a
-- 000005). Uma instância antiga ainda no ar falharia no COMMIT de cada
-- movimentação a partir daqui.
--
-- Custo: preenche e confere o histórico com comandos em lote, não linha a
-- linha. Medido com 385 mil lançamentos: 16 s (com as FKs já existentes,
-- checando linha a linha: 57 s; com o gatilho linha a linha: > 5 min).
-- Durante esse tempo as ESCRITAS de dinheiro esperam; leituras seguem. Para
-- um histórico muito maior, preencha antes, em lotes, com a aplicação no ar
-- (os lançamentos sem partida são só os antigos, um conjunto que não cresce
-- mais); esta migration então só confere.

-- Pausa as movimentações. A ordem evita deadlock: quem já tem uma carteira
-- travada (FOR UPDATE) termina antes; nenhuma começa até o fim. Sem isso, o
-- preenchimento (FK de ledger_accounts para wallets) e uma aposta em curso
-- poderiam esperar um pelo outro.
LOCK TABLE wallets IN EXCLUSIVE MODE;
LOCK TABLE wallet_ledger_entries IN SHARE MODE;

-- O histórico entra sem o gatilho linha a linha; a conferência é em lote,
-- logo abaixo, com as mesmas regras.
ALTER TABLE journal_postings DISABLE TRIGGER journal_check_entry;

INSERT INTO ledger_accounts (code, type, currency, wallet_id, created_at)
SELECT 'wallet:' || id::text, 'LIABILITY', currency, id, created_at
  FROM wallets
    ON CONFLICT (code) DO NOTHING;

INSERT INTO ledger_accounts (code, type, currency, provider_id, created_at)
SELECT DISTINCT ON (code) code, type, currency, provider_id, created_at
  FROM (
    SELECT CASE WHEN t.kind = 'OPENING' THEN 'cash:' || e.currency
                ELSE 'provider:' || t.provider_id || ':' || e.currency END AS code,
           CASE WHEN t.kind = 'OPENING' THEN 'ASSET' ELSE 'REVENUE' END AS type,
           e.currency, t.provider_id, e.created_at
      FROM wallet_ledger_entries e
      JOIN wager_transactions t ON t.id = e.transaction_id
  ) c
 ORDER BY code, created_at
    ON CONFLICT (code) DO NOTHING;

-- só os lançamentos que ainda não têm partidas (a aplicação nova já grava)
INSERT INTO journal_postings (transaction_id, account_code, direction, amount_minor, currency, created_at)
SELECT e.transaction_id, a.code, a.direction, e.amount_minor, e.currency, e.created_at
  FROM wallet_ledger_entries e
  JOIN wager_transactions t ON t.id = e.transaction_id
 CROSS JOIN LATERAL (VALUES
        ('wallet:' || e.wallet_id::text, e.direction),
        (CASE WHEN t.kind = 'OPENING' THEN 'cash:' || e.currency
              ELSE 'provider:' || t.provider_id || ':' || e.currency END,
         CASE e.direction WHEN 'DEBIT' THEN 'CREDIT' ELSE 'DEBIT' END)
       ) AS a(code, direction)
 WHERE NOT EXISTS (SELECT 1 FROM journal_postings p WHERE p.transaction_id = e.transaction_id);

-- Conferência em lote do razão inteiro: as regras de journal_check_entry e
-- de ledger_check_journal, num comando cada.
DO $$
DECLARE
    bad UUID;
BEGIN
    SELECT transaction_id INTO bad
      FROM journal_postings
     GROUP BY transaction_id
    HAVING count(*) < 2 OR count(DISTINCT currency) <> 1
        OR SUM(CASE direction WHEN 'DEBIT' THEN amount_minor ELSE -amount_minor END) <> 0
     LIMIT 1;
    IF bad IS NOT NULL THEN
        RAISE EXCEPTION 'journal: entry % is not balanced', bad USING ERRCODE = 'check_violation';
    END IF;

    SELECT e.transaction_id INTO bad
      FROM wallet_ledger_entries e
     WHERE NOT EXISTS (
            SELECT 1 FROM journal_postings p
             WHERE p.transaction_id = e.transaction_id
               AND p.account_code = 'wallet:' || e.wallet_id::text
               AND p.direction = e.direction AND p.amount_minor = e.amount_minor)
     LIMIT 1;
    IF bad IS NOT NULL THEN
        RAISE EXCEPTION 'ledger: entry % has no journal posting', bad USING ERRCODE = 'check_violation';
    END IF;

    SELECT p.transaction_id INTO bad
      FROM journal_postings p
      JOIN ledger_accounts a ON a.code = p.account_code AND a.wallet_id IS NOT NULL
     WHERE NOT EXISTS (
            SELECT 1 FROM wallet_ledger_entries e
             WHERE e.wallet_id = a.wallet_id AND e.transaction_id = p.transaction_id
               AND e.direction = p.direction AND e.amount_minor = p.amount_minor)
     LIMIT 1;
    IF bad IS NOT NULL THEN
        RAISE EXCEPTION 'journal: wallet posting of % has no ledger entry', bad USING ERRCODE = 'check_violation';
    END IF;

    SELECT p.transaction_id INTO bad
      FROM journal_postings p
      JOIN ledger_accounts a ON a.code = p.account_code
      JOIN wager_transactions t ON t.id = p.transaction_id
     WHERE (a.type = 'REVENUE' AND a.provider_id IS DISTINCT FROM t.provider_id)
        OR (a.type = 'ASSET' AND t.kind <> 'OPENING')
     LIMIT 1;
    IF bad IS NOT NULL THEN
        RAISE EXCEPTION 'journal: counterpart of % does not match the transaction', bad USING ERRCODE = 'check_violation';
    END IF;
END $$;

-- FKs agora, com o histórico já dentro: cada uma é validada numa consulta
-- só. Travam escritas (já pausadas), não leituras.
ALTER TABLE journal_postings
    ADD CONSTRAINT journal_postings_transaction_fk
        FOREIGN KEY (transaction_id) REFERENCES wager_transactions (id),
    ADD CONSTRAINT journal_account_currency_fk
        FOREIGN KEY (account_code, currency) REFERENCES ledger_accounts (code, currency);

ALTER TABLE journal_postings ENABLE TRIGGER journal_check_entry;

-- O outro sentido, daqui em diante: todo lançamento do ledger da carteira
-- tem a partida correspondente no razão. Com journal_check_entry, o ledger
-- da carteira é a projeção exata do razão na conta dela.
CREATE FUNCTION ledger_check_journal() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM journal_postings
         WHERE transaction_id = NEW.transaction_id
           AND account_code = 'wallet:' || NEW.wallet_id::text
           AND direction = NEW.direction AND amount_minor = NEW.amount_minor
    ) THEN
        RAISE EXCEPTION 'ledger: entry % of wallet % has no journal posting', NEW.transaction_id, NEW.wallet_id
            USING ERRCODE = 'check_violation';
    END IF;
    RETURN NULL;
END $$;

CREATE CONSTRAINT TRIGGER ledger_check_journal
    AFTER INSERT ON wallet_ledger_entries
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW EXECUTE FUNCTION ledger_check_journal();

-- Estatísticas já: o preenchimento acabou de colocar centenas de milhares
-- de partidas, e até o autovacuum passar o planejador não sabe disso. Medido
-- com 385 mil lançamentos: a carga logo depois da migration rodou a 274
-- req/s; a mesma carga com estatísticas, a 463 req/s (as consultas do
-- gatilho rodam no COMMIT, com a carteira travada).
ANALYZE journal_postings, ledger_accounts;
