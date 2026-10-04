-- Reverte 000001_init. Apaga todos os dados financeiros: use só em
-- desenvolvimento ou em testes.
DROP TABLE IF EXISTS outbox_events;
DROP TABLE IF EXISTS inbox_messages;
DROP TABLE IF EXISTS wallet_ledger_entries;
DROP TABLE IF EXISTS wager_transactions;
DROP TABLE IF EXISTS wallets;

DROP FUNCTION IF EXISTS outbox_guard_update();
DROP FUNCTION IF EXISTS wallets_check_ledger();
DROP FUNCTION IF EXISTS forbid_mutation();
DROP FUNCTION IF EXISTS ledger_guard_insert();
DROP FUNCTION IF EXISTS wager_tx_guard_update();
DROP FUNCTION IF EXISTS wallets_guard_update();
