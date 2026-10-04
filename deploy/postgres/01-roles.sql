-- Executado uma única vez pelo container do Postgres na criação do volume.
-- Senhas de DESENVOLVIMENTO LOCAL; não use em nenhum outro ambiente.

-- dono do schema: roda as migrations
CREATE ROLE wallet_owner LOGIN PASSWORD 'wallet_owner' CREATEDB;

-- aplicação: sem DDL, sem UPDATE/DELETE no ledger (permissões na migration)
CREATE ROLE wallet_app LOGIN PASSWORD 'wallet_app';

CREATE DATABASE wallet OWNER wallet_owner;
REVOKE ALL ON DATABASE wallet FROM PUBLIC;
GRANT CONNECT ON DATABASE wallet TO wallet_app;

\connect wallet
-- o schema public pertence ao dono das tabelas; a aplicação só o usa
ALTER SCHEMA public OWNER TO wallet_owner;
REVOKE CREATE ON SCHEMA public FROM PUBLIC;
GRANT USAGE ON SCHEMA public TO wallet_app;
