-- 002_secrets: the tables of .specs/veho-secrets.md §2. The KEKs are stored
-- wrapped by an unwrap key that is never in the database (SEC-25); a secret
-- is a value encrypted under its own data key, wrapped by a KEK (SEC-24).

CREATE TABLE veho.kek (
	version       int PRIMARY KEY CHECK (version > 0),
	wrapped       bytea NOT NULL,
	unwrap_key_id text NOT NULL CHECK (unwrap_key_id <> ''),
	state         text NOT NULL CHECK (state IN ('active', 'retiring', 'retired')),
	created_at    timestamptz NOT NULL DEFAULT now(),
	retired_at    timestamptz
);
-- at most one active version
CREATE UNIQUE INDEX kek_one_active ON veho.kek ((true)) WHERE state = 'active';

CREATE TABLE veho.secrets (
	tenant_id   uuid NOT NULL REFERENCES veho.tenants (id),
	name        text NOT NULL CHECK (name ~ '^[a-z0-9][a-z0-9._-]{0,62}$'),
	version     int NOT NULL CHECK (version > 0),
	kek_version int NOT NULL REFERENCES veho.kek (version),
	wrapped_dek bytea NOT NULL,
	ciphertext  bytea NOT NULL,
	created_at  timestamptz NOT NULL DEFAULT now(),
	updated_at  timestamptz NOT NULL DEFAULT now(),
	PRIMARY KEY (tenant_id, name)
);
CREATE INDEX secrets_kek_version ON veho.secrets (kek_version);

ALTER TABLE veho.secrets ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON veho.secrets
	USING (tenant_id = veho.current_tenant()) WITH CHECK (tenant_id = veho.current_tenant());

-- A secret an endpoint references cannot be deleted (§2). credential_ref is
-- text, so this trigger stands in for a foreign key; it runs as the caller,
-- whose row-level security shows the endpoints of the secret's own tenant.
CREATE FUNCTION veho.secrets_in_use() RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
	IF EXISTS (SELECT 1 FROM veho.endpoints
	           WHERE tenant_id = OLD.tenant_id AND credential_ref = 'db://' || OLD.name) THEN
		RAISE EXCEPTION 'secret % is used by an endpoint', OLD.name USING ERRCODE = '23503';
	END IF;
	RETURN OLD;
END $$;
CREATE TRIGGER secrets_in_use BEFORE DELETE ON veho.secrets
	FOR EACH ROW EXECUTE FUNCTION veho.secrets_in_use();

GRANT SELECT ON veho.kek TO veho_api, veho_worker;
GRANT SELECT, INSERT, UPDATE, DELETE ON veho.secrets TO veho_api;
GRANT SELECT ON veho.secrets TO veho_worker;
