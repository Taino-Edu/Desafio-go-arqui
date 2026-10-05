-- 000003_outbox_pending_seq_index: reivindicação da outbox com custo
-- proporcional ao lote, não ao acúmulo.
--
-- O publicador percorre os pendentes em ordem de gravação (seq) e, para cada
-- um, verifica se há pendente anterior do mesmo agregado. Achado pelo teste
-- de carga (docs/LOAD-TEST.md): com as estatísticas coletadas quando quase
-- nada estava pendente, o planejador fazia essa verificação pelo índice de
-- next_attempt_at, varrendo TODOS os pendentes para cada linha. Com 20 mil
-- pendentes, um lote de 50 custava 1 milhão de comparações; sob carga, a
-- consulta estourava o statement_timeout e a publicação parava.
--
-- outbox_unpublished_idx (next_attempt_at) só servia à consulta antiga; sem
-- ele, a verificação usa outbox_unpublished_aggregate_seq_idx.
CREATE INDEX outbox_pending_seq_idx
    ON outbox_events (seq)
    WHERE published_at IS NULL;

DROP INDEX outbox_unpublished_idx;
