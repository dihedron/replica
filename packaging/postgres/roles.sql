-- The database roles, the database and its schemas (.specs/veho-store.md §2),
-- created by operations, not by migrations. Run once, as a superuser, with
-- the passwords as psql variables:
--   psql -v migrator_password=... -v dbos_password=... -v api_password=... \
--        -v worker_password=... -f roles.sql
-- The lab (test/lab/lab.sh up) uses this file too.
\set ON_ERROR_STOP on

CREATE ROLE veho_migrator LOGIN PASSWORD :'migrator_password';
CREATE ROLE veho_dbos     LOGIN PASSWORD :'dbos_password';
CREATE ROLE veho_api      LOGIN PASSWORD :'api_password';
CREATE ROLE veho_worker   LOGIN PASSWORD :'worker_password';

CREATE DATABASE veho OWNER veho_migrator;
REVOKE ALL ON DATABASE veho FROM PUBLIC;
GRANT CONNECT ON DATABASE veho TO veho_dbos, veho_api, veho_worker;

\connect veho
REVOKE ALL ON SCHEMA public FROM PUBLIC;

-- veho: the application's schema, owned by the migrator, which owns every
-- object in it; the migrations grant the other roles what they need.
CREATE SCHEMA veho AUTHORIZATION veho_migrator;
GRANT USAGE ON SCHEMA veho TO veho_api, veho_worker;

-- dbos: DBOS's schema, which DBOS fills at launch as veho_dbos. The API
-- enqueues workflows in its own transactions (EXT-1), so it gets the rights on
-- every table DBOS creates there, now or in a later version.
CREATE SCHEMA dbos AUTHORIZATION veho_dbos;
GRANT USAGE ON SCHEMA dbos TO veho_api;
ALTER DEFAULT PRIVILEGES FOR ROLE veho_dbos IN SCHEMA dbos GRANT SELECT, INSERT, UPDATE ON TABLES TO veho_api;
ALTER DEFAULT PRIVILEGES FOR ROLE veho_dbos IN SCHEMA dbos GRANT USAGE, SELECT ON SEQUENCES TO veho_api;

ALTER ROLE veho_migrator IN DATABASE veho SET search_path = veho;
ALTER ROLE veho_dbos     IN DATABASE veho SET search_path = dbos;
ALTER ROLE veho_api      IN DATABASE veho SET search_path = veho, dbos;
ALTER ROLE veho_worker   IN DATABASE veho SET search_path = veho;
