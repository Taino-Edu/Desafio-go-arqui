-- Volta a não exigir a partida de cada lançamento do ledger. As partidas
-- do histórico ficam (são corretas); a 000005 down apaga o razão inteiro.
DROP TRIGGER IF EXISTS ledger_check_journal ON wallet_ledger_entries;
DROP FUNCTION IF EXISTS ledger_check_journal();
ALTER TABLE journal_postings
    DROP CONSTRAINT IF EXISTS journal_postings_transaction_fk,
    DROP CONSTRAINT IF EXISTS journal_account_currency_fk;
