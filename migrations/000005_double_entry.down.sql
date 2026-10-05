-- Remove o razão em partidas dobradas. O ledger por carteira não muda: ele
-- continua completo sozinho.
DROP TABLE IF EXISTS journal_postings;
DROP TABLE IF EXISTS ledger_accounts;
DROP FUNCTION IF EXISTS journal_check_entry();
