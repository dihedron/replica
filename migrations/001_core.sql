-- 001_core: the whole schema until a first deployment exists; until then it
-- is edited in place, never extended by new migrations. The core tables of
-- .specs/veho-store.md §3, their row-level security (§4) and the privileges
-- of veho_api and veho_worker (§2); then the secrets (veho-secrets.md §2),
-- the engine's lease renewal and the housekeeping's cross-tenant functions
-- (veho-engine.md §7). Runs as veho_migrator, which owns the schema veho
-- (packaging/postgres/roles.sql).

GRANT USAGE ON SCHEMA veho TO veho_api, veho_worker;
-- the start-up check of the schema version (STO-C4)
GRANT SELECT ON veho.schema_migrations TO veho_api, veho_worker;

-- The tenant of the transaction (§4.1): NULL when unset or empty, so that a
-- statement without a tenant sees and changes nothing.
CREATE FUNCTION veho.current_tenant() RETURNS uuid
LANGUAGE sql STABLE
AS $$ SELECT nullif(current_setting('veho.tenant_id', true), '')::uuid $$;

CREATE TABLE veho.tenants (
	id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
	name       text NOT NULL UNIQUE CHECK (name <> ''),
	state      text NOT NULL DEFAULT 'active' CHECK (state IN ('active', 'suspended')),
	quotas     jsonb NOT NULL DEFAULT '{}',
	-- the tenant's own settings, such as inline_policies (veho-api-server.md §5.1)
	settings   jsonb NOT NULL DEFAULT '{}',
	created_at timestamptz NOT NULL DEFAULT now()
);

-- The topology (veho-distribution-policy.md §2): platform configuration.
CREATE TABLE veho.locations (
	id         text PRIMARY KEY CHECK (id <> ''),
	level      text NOT NULL CHECK (level <> ''),
	parent_id  text REFERENCES veho.locations (id),
	created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE veho.endpoints (
	id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
	tenant_id      uuid NOT NULL REFERENCES veho.tenants (id),
	name           text NOT NULL CHECK (name <> ''),
	location_id    text NOT NULL REFERENCES veho.locations (id),
	driver         text NOT NULL CHECK (driver <> ''),
	config         jsonb NOT NULL DEFAULT '{}',
	credential_ref text,
	created_at     timestamptz NOT NULL DEFAULT now(),
	UNIQUE (tenant_id, id),
	UNIQUE (tenant_id, name)
);

CREATE TABLE veho.policies (
	id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
	tenant_id  uuid NOT NULL REFERENCES veho.tenants (id),
	name       text NOT NULL CHECK (name <> ''),
	version    int NOT NULL CHECK (version > 0),
	spec       jsonb NOT NULL,
	created_at timestamptz NOT NULL DEFAULT now(),
	-- a deleted policy's versions stay, for the jobs that used them
	deleted_at timestamptz,
	UNIQUE (tenant_id, id),
	UNIQUE (tenant_id, name, version)
);

CREATE TABLE veho.jobs (
	id                 uuid PRIMARY KEY DEFAULT gen_random_uuid(),
	tenant_id          uuid NOT NULL,
	idempotency_key    text,
	source_endpoint_id uuid NOT NULL,
	source_path        text NOT NULL,
	source_info        jsonb NOT NULL DEFAULT '{}',
	policy             jsonb NOT NULL,
	policy_ref         uuid,
	state              text NOT NULL DEFAULT 'ACCEPTED'
		CHECK (state IN ('ACCEPTED', 'RUNNING', 'SUCCEEDED', 'FAILED', 'CANCELLED')),
	copies_done        int NOT NULL DEFAULT 0 CHECK (copies_done >= 0),
	satisfied_at       timestamptz,
	completed_at       timestamptz,
	settled_at         timestamptz,
	deadline_at        timestamptz,
	cancel_requested   boolean NOT NULL DEFAULT false,
	created_at         timestamptz NOT NULL DEFAULT now(),
	updated_at         timestamptz NOT NULL DEFAULT now(),
	finished_at        timestamptz,
	last_error         jsonb,
	labels             jsonb NOT NULL DEFAULT '{}',
	-- the hash of the request, so that an idempotency key reused with
	-- another request is refused (API-4)
	request_sha256     bytea,
	UNIQUE (tenant_id, id),
	UNIQUE (tenant_id, idempotency_key),
	FOREIGN KEY (tenant_id, source_endpoint_id) REFERENCES veho.endpoints (tenant_id, id),
	FOREIGN KEY (tenant_id, policy_ref) REFERENCES veho.policies (tenant_id, id)
);
CREATE INDEX jobs_tenant_state ON veho.jobs (tenant_id, state, created_at);

CREATE TABLE veho.transfers (
	id                      uuid PRIMARY KEY DEFAULT gen_random_uuid(),
	tenant_id               uuid NOT NULL,
	job_id                  uuid NOT NULL,
	destination_endpoint_id uuid NOT NULL,
	position                jsonb NOT NULL,
	dest_path               text NOT NULL,
	state                   text NOT NULL DEFAULT 'PENDING'
		CHECK (state IN ('PENDING', 'COPYING', 'VERIFYING', 'RETRY_WAIT', 'SUCCEEDED', 'FAILED', 'CANCELLED')),
	attempt                 int NOT NULL DEFAULT 0 CHECK (attempt >= 0),
	adoptions               int NOT NULL DEFAULT 0 CHECK (adoptions >= 0),
	bytes_done              bigint NOT NULL DEFAULT 0 CHECK (bytes_done >= 0),
	bytes_total             bigint CHECK (bytes_total >= 0),
	resume_token            jsonb,
	lease_owner             text,
	lease_expires_at        timestamptz,
	fence                   bigint NOT NULL DEFAULT 0,
	last_error              jsonb,
	started_at              timestamptz,
	finished_at             timestamptz,
	UNIQUE (tenant_id, id),
	UNIQUE (job_id, destination_endpoint_id),
	FOREIGN KEY (tenant_id, job_id) REFERENCES veho.jobs (tenant_id, id) ON DELETE CASCADE,
	FOREIGN KEY (tenant_id, destination_endpoint_id) REFERENCES veho.endpoints (tenant_id, id)
);
CREATE INDEX transfers_tenant_state ON veho.transfers (tenant_id, state);
CREATE INDEX transfers_lease ON veho.transfers (lease_expires_at) WHERE state IN ('COPYING', 'VERIFYING');

CREATE TABLE veho.parts (
	tenant_id   uuid NOT NULL,
	transfer_id uuid NOT NULL,
	part_no     int NOT NULL CHECK (part_no >= 0),
	"offset"    bigint NOT NULL CHECK ("offset" >= 0),
	length      bigint NOT NULL CHECK (length >= 0),
	sha256      bytea NOT NULL CHECK (length(sha256) = 32),
	etag        text,
	state       text NOT NULL DEFAULT 'committed' CHECK (state IN ('committed')),
	PRIMARY KEY (transfer_id, part_no),
	FOREIGN KEY (tenant_id, transfer_id) REFERENCES veho.transfers (tenant_id, id) ON DELETE CASCADE
);

CREATE TABLE veho.executors (
	id          text PRIMARY KEY CHECK (id <> ''),
	region      text NOT NULL,
	app_version text NOT NULL,
	started_at  timestamptz NOT NULL DEFAULT now(),
	last_seen   timestamptz NOT NULL DEFAULT now()
);

-- Immutable columns (DP-9, STO-6): a job's identity, source and policy, a
-- transfer's identity, destination and position never change.
CREATE FUNCTION veho.jobs_immutable() RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
	IF (NEW.id, NEW.tenant_id, NEW.idempotency_key, NEW.source_endpoint_id, NEW.source_path, NEW.policy, NEW.policy_ref, NEW.created_at,
	    NEW.labels, NEW.request_sha256)
		IS DISTINCT FROM
		(OLD.id, OLD.tenant_id, OLD.idempotency_key, OLD.source_endpoint_id, OLD.source_path, OLD.policy, OLD.policy_ref, OLD.created_at,
		 OLD.labels, OLD.request_sha256) THEN
		RAISE EXCEPTION 'jobs: identity, source and policy are immutable' USING ERRCODE = 'integrity_constraint_violation';
	END IF;
	RETURN NEW;
END $$;
CREATE TRIGGER jobs_immutable BEFORE UPDATE ON veho.jobs FOR EACH ROW EXECUTE FUNCTION veho.jobs_immutable();

CREATE FUNCTION veho.transfers_immutable() RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
	IF (NEW.id, NEW.tenant_id, NEW.job_id, NEW.destination_endpoint_id, NEW.position, NEW.dest_path)
		IS DISTINCT FROM
		(OLD.id, OLD.tenant_id, OLD.job_id, OLD.destination_endpoint_id, OLD.position, OLD.dest_path) THEN
		RAISE EXCEPTION 'transfers: identity, destination and position are immutable' USING ERRCODE = 'integrity_constraint_violation';
	END IF;
	RETURN NEW;
END $$;
CREATE TRIGGER transfers_immutable BEFORE UPDATE ON veho.transfers FOR EACH ROW EXECUTE FUNCTION veho.transfers_immutable();

-- Row-level security (§4.2). The owner, veho_migrator, is exempt (no FORCE):
-- that is what lets the SECURITY DEFINER functions below cross tenants, and
-- the owner is not used at run time otherwise.
ALTER TABLE veho.tenants ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON veho.tenants
	USING (id = veho.current_tenant());

ALTER TABLE veho.endpoints ENABLE ROW LEVEL SECURITY;
ALTER TABLE veho.policies ENABLE ROW LEVEL SECURITY;
ALTER TABLE veho.jobs ENABLE ROW LEVEL SECURITY;
ALTER TABLE veho.transfers ENABLE ROW LEVEL SECURITY;
ALTER TABLE veho.parts ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON veho.endpoints
	USING (tenant_id = veho.current_tenant()) WITH CHECK (tenant_id = veho.current_tenant());
CREATE POLICY tenant_isolation ON veho.policies
	USING (tenant_id = veho.current_tenant()) WITH CHECK (tenant_id = veho.current_tenant());
CREATE POLICY tenant_isolation ON veho.jobs
	USING (tenant_id = veho.current_tenant()) WITH CHECK (tenant_id = veho.current_tenant());
CREATE POLICY tenant_isolation ON veho.transfers
	USING (tenant_id = veho.current_tenant()) WITH CHECK (tenant_id = veho.current_tenant());
CREATE POLICY tenant_isolation ON veho.parts
	USING (tenant_id = veho.current_tenant()) WITH CHECK (tenant_id = veho.current_tenant());

-- Platform operations (§4.2), for the API's platform-admin calls.
CREATE FUNCTION veho.create_tenant(p_name text) RETURNS uuid
LANGUAGE sql SECURITY DEFINER SET search_path = veho, pg_temp
AS $$ INSERT INTO veho.tenants (name) VALUES (p_name) RETURNING id $$;

CREATE FUNCTION veho.put_location(p_id text, p_level text, p_parent text) RETURNS void
LANGUAGE sql SECURITY DEFINER SET search_path = veho, pg_temp
AS $$
	INSERT INTO veho.locations (id, level, parent_id) VALUES (p_id, p_level, p_parent)
	ON CONFLICT (id) DO UPDATE SET level = EXCLUDED.level, parent_id = EXCLUDED.parent_id
$$;

-- What crosses tenants (§4.3): identifiers and tenants only.
CREATE FUNCTION veho.expired_transfers(p_limit int) RETURNS TABLE (tenant_id uuid, transfer_id uuid)
LANGUAGE sql STABLE SECURITY DEFINER SET search_path = veho, pg_temp
AS $$
	SELECT t.tenant_id, t.id FROM veho.transfers t
	WHERE t.state IN ('COPYING', 'VERIFYING') AND t.lease_expires_at < now()
	ORDER BY t.lease_expires_at
	LIMIT p_limit
$$;

CREATE FUNCTION veho.settled_jobs_before(p_before timestamptz, p_limit int) RETURNS TABLE (tenant_id uuid, job_id uuid)
LANGUAGE sql STABLE SECURITY DEFINER SET search_path = veho, pg_temp
AS $$
	SELECT j.tenant_id, j.id FROM veho.jobs j
	WHERE j.settled_at < p_before
	ORDER BY j.settled_at
	LIMIT p_limit
$$;

REVOKE ALL ON FUNCTION veho.create_tenant(text), veho.put_location(text, text, text),
	veho.expired_transfers(int), veho.settled_jobs_before(timestamptz, int) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION veho.create_tenant(text), veho.put_location(text, text, text) TO veho_api;
GRANT EXECUTE ON FUNCTION veho.expired_transfers(int), veho.settled_jobs_before(timestamptz, int) TO veho_worker;

-- Privileges (§2).
GRANT SELECT ON veho.tenants, veho.locations TO veho_api, veho_worker;
GRANT SELECT, INSERT, UPDATE, DELETE ON veho.endpoints, veho.policies TO veho_api;
GRANT SELECT, INSERT, UPDATE ON veho.jobs, veho.transfers, veho.parts TO veho_api;
GRANT SELECT ON veho.executors TO veho_api;
GRANT SELECT ON veho.endpoints, veho.policies TO veho_worker;
GRANT SELECT, INSERT, UPDATE, DELETE ON veho.jobs, veho.transfers, veho.parts, veho.executors TO veho_worker;

-- ---------------------------------------------------------------------------
-- the tables of .specs/veho-secrets.md §2. The KEKs are stored
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

-- The background re-wrap after a KEK rotation (veho-secrets.md §6, backlog
-- 24): housekeeping, as veho_worker, reads the DEKs not under the active
-- KEK across tenants, re-wraps each one, and retires the versions left
-- unused. It sees wrapped DEKs, never a value; an update applies only if
-- the row and the active KEK are as read.
CREATE FUNCTION veho.deks_to_rewrap(p_limit int)
RETURNS TABLE (tenant_id uuid, name text, version int, kek_version int, wrapped_dek bytea, active int)
LANGUAGE sql STABLE SECURITY DEFINER SET search_path = veho, pg_temp
AS $$
	SELECT s.tenant_id, s.name, s.version, s.kek_version, s.wrapped_dek, k.version
	FROM veho.secrets s, veho.kek k
	WHERE k.state = 'active' AND s.kek_version <> k.version
	ORDER BY s.tenant_id, s.name
	LIMIT p_limit
$$;

CREATE FUNCTION veho.rewrap_dek(p_tenant uuid, p_name text, p_version int, p_from int, p_to int, p_wrapped bytea) RETURNS boolean
LANGUAGE sql SECURITY DEFINER SET search_path = veho, pg_temp
AS $$
	WITH u AS (
		UPDATE veho.secrets SET kek_version = p_to, wrapped_dek = p_wrapped
		WHERE tenant_id = p_tenant AND name = p_name AND version = p_version AND kek_version = p_from
			AND p_to = (SELECT version FROM veho.kek WHERE state = 'active')
		RETURNING 1)
	SELECT EXISTS (SELECT 1 FROM u)
$$;

CREATE FUNCTION veho.retire_keks() RETURNS SETOF int
LANGUAGE sql SECURITY DEFINER SET search_path = veho, pg_temp
AS $$
	UPDATE veho.kek k SET state = 'retired', retired_at = now()
	WHERE state = 'retiring' AND NOT EXISTS (SELECT 1 FROM veho.secrets s WHERE s.kek_version = k.version)
	RETURNING version
$$;

REVOKE ALL ON FUNCTION veho.deks_to_rewrap(int), veho.rewrap_dek(uuid, text, int, int, int, bytea), veho.retire_keks() FROM PUBLIC;
GRANT EXECUTE ON FUNCTION veho.deks_to_rewrap(int), veho.rewrap_dek(uuid, text, int, int, int, bytea), veho.retire_keks() TO veho_worker;

-- ---------------------------------------------------------------------------
-- what the engine needs across tenants (.specs/veho-engine.md
-- §7). The heartbeat renews the leases of its executor's transfers, of every
-- tenant, which row-level security would hide from veho_worker.

CREATE FUNCTION veho.renew_leases(p_executor text, p_ttl interval) RETURNS int
LANGUAGE sql SECURITY DEFINER SET search_path = veho, pg_temp
AS $$
	WITH renewed AS (
		UPDATE veho.transfers SET lease_expires_at = now() + p_ttl
		 WHERE lease_owner = p_executor AND state IN ('COPYING', 'VERIFYING')
		RETURNING 1
	)
	SELECT count(*)::int FROM renewed
$$;
REVOKE ALL ON FUNCTION veho.renew_leases(text, interval) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION veho.renew_leases(text, interval) TO veho_worker;

-- ---------------------------------------------------------------------------
-- the cross-tenant reads and repairs of the housekeeping
-- loop (.specs/veho-engine.md §7, veho-audit-housekeeping.md §3), as
-- SECURITY DEFINER functions for veho_worker; each returns the tenant of
-- what it found, so that the loop acts in that tenant's transaction.

-- Jobs not terminal, quiet for p_stale: their workflow is checked.
CREATE FUNCTION veho.quiet_jobs(p_stale interval, p_limit int) RETURNS TABLE (tenant_id uuid, job_id uuid)
LANGUAGE sql STABLE SECURITY DEFINER SET search_path = veho, pg_temp
AS $$
	SELECT j.tenant_id, j.id FROM veho.jobs j
	WHERE j.state IN ('ACCEPTED', 'RUNNING') AND j.updated_at < now() - p_stale
	ORDER BY j.updated_at
	LIMIT p_limit
$$;

-- Jobs not terminal past their deadline.
CREATE FUNCTION veho.expired_jobs(p_limit int) RETURNS TABLE (tenant_id uuid, job_id uuid)
LANGUAGE sql STABLE SECURITY DEFINER SET search_path = veho, pg_temp
AS $$
	SELECT j.tenant_id, j.id FROM veho.jobs j
	WHERE j.state IN ('ACCEPTED', 'RUNNING') AND j.deadline_at < now()
	ORDER BY j.deadline_at
	LIMIT p_limit
$$;

-- Counts an adoption of a transfer (the reconciler's own counter, since
-- ResumeWorkflows resets DBOS's: EXT-2d) and returns it with the tenant.
CREATE FUNCTION veho.adopted(p_transfer uuid) RETURNS TABLE (tenant_id uuid, adoptions int)
LANGUAGE sql SECURITY DEFINER SET search_path = veho, pg_temp
AS $$
	UPDATE veho.transfers SET adoptions = adoptions + 1 WHERE id = p_transfer
	RETURNING tenant_id, adoptions
$$;

-- The tenant of a job, for a workflow found by ID.
CREATE FUNCTION veho.job_tenant(p_job uuid) RETURNS uuid
LANGUAGE sql STABLE SECURITY DEFINER SET search_path = veho, pg_temp
AS $$ SELECT tenant_id FROM veho.jobs WHERE id = p_job $$;

-- Jobs and transfers by state, for the metrics; no tenant (T-19).
CREATE FUNCTION veho.state_counts() RETURNS TABLE (kind text, state text, n bigint)
LANGUAGE sql STABLE SECURITY DEFINER SET search_path = veho, pg_temp
AS $$
	SELECT 'job', state, count(*) FROM veho.jobs GROUP BY state
	UNION ALL
	SELECT 'transfer', state, count(*) FROM veho.transfers GROUP BY state
$$;

REVOKE ALL ON FUNCTION veho.quiet_jobs(interval, int), veho.expired_jobs(int), veho.adopted(uuid),
	veho.job_tenant(uuid), veho.state_counts() FROM PUBLIC;
GRANT EXECUTE ON FUNCTION veho.quiet_jobs(interval, int), veho.expired_jobs(int), veho.adopted(uuid),
	veho.job_tenant(uuid), veho.state_counts() TO veho_worker;

-- ---------------------------------------------------------------------------
-- The API's principals and their keys (.specs/veho-api-server.md §4). A
-- platform administrator has no tenant; every other principal one.

CREATE TABLE veho.principals (
	id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
	tenant_id   uuid REFERENCES veho.tenants (id),
	name        text NOT NULL CHECK (name <> ''),
	role        text NOT NULL CHECK (role IN ('viewer', 'operator', 'admin', 'platform-admin')),
	created_at  timestamptz NOT NULL DEFAULT now(),
	disabled_at timestamptz,
	CHECK ((role = 'platform-admin') = (tenant_id IS NULL))
);
CREATE UNIQUE INDEX principals_name ON veho.principals (coalesce(tenant_id, '00000000-0000-0000-0000-000000000000'), name);

CREATE TABLE veho.api_keys (
	id           text PRIMARY KEY CHECK (id ~ '^[a-z0-9]{12}$'),
	principal_id uuid NOT NULL REFERENCES veho.principals (id) ON DELETE CASCADE,
	tenant_id    uuid REFERENCES veho.tenants (id),
	sha256       bytea NOT NULL CHECK (length(sha256) = 32),
	created_at   timestamptz NOT NULL DEFAULT now(),
	expires_at   timestamptz,
	revoked_at   timestamptz,
	last_used_at timestamptz
);

ALTER TABLE veho.principals ENABLE ROW LEVEL SECURITY;
ALTER TABLE veho.api_keys ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON veho.principals
	USING (tenant_id = veho.current_tenant()) WITH CHECK (tenant_id = veho.current_tenant());
CREATE POLICY tenant_isolation ON veho.api_keys
	USING (tenant_id = veho.current_tenant()) WITH CHECK (tenant_id = veho.current_tenant());

-- authenticate_key returns the principal of a key, before the tenant is
-- known; the caller compares the hash. It records the use at most once a
-- minute.
CREATE FUNCTION veho.authenticate_key(p_id text)
RETURNS TABLE (principal_id uuid, tenant_id uuid, name text, role text, sha256 bytea, valid boolean)
LANGUAGE sql SECURITY DEFINER SET search_path = veho, pg_temp
AS $$
	WITH k AS (
		UPDATE veho.api_keys SET last_used_at = now()
		 WHERE id = p_id AND (last_used_at IS NULL OR last_used_at < now() - interval '1 minute')
		RETURNING id
	)
	SELECT p.id, p.tenant_id, p.name, p.role, a.sha256,
	       a.revoked_at IS NULL AND (a.expires_at IS NULL OR a.expires_at > now()) AND p.disabled_at IS NULL
	         AND (p.tenant_id IS NULL OR (SELECT t.state = 'active' FROM veho.tenants t WHERE t.id = p.tenant_id))
	  FROM veho.api_keys a JOIN veho.principals p ON p.id = a.principal_id
	 WHERE a.id = p_id
$$;
REVOKE ALL ON FUNCTION veho.authenticate_key(text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION veho.authenticate_key(text) TO veho_api;
GRANT SELECT, INSERT, UPDATE ON veho.principals, veho.api_keys TO veho_api;

-- ---------------------------------------------------------------------------
-- OIDC and SPIFFE identities (.specs/veho-api-server.md §4.4): an
-- entitlement gives one subject, or the members of one group, of an issuer
-- (an OIDC issuer or a SPIFFE trust domain) a role in a tenant, or the
-- platform-admin role.

CREATE TABLE veho.entitlements (
	id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
	tenant_id  uuid REFERENCES veho.tenants (id),
	kind       text NOT NULL CHECK (kind IN ('oidc', 'spiffe')),
	issuer     text NOT NULL CHECK (issuer <> ''),
	subject    text CHECK (subject <> ''),
	"group"    text CHECK ("group" <> ''),
	role       text NOT NULL CHECK (role IN ('viewer', 'operator', 'admin', 'platform-admin')),
	created_at timestamptz NOT NULL DEFAULT now(),
	CHECK ((subject IS NULL) <> ("group" IS NULL)),
	CHECK ((role = 'platform-admin') = (tenant_id IS NULL))
);
CREATE INDEX entitlements_lookup ON veho.entitlements (kind, issuer);

ALTER TABLE veho.entitlements ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON veho.entitlements
	USING (tenant_id = veho.current_tenant()) WITH CHECK (tenant_id = veho.current_tenant());

-- entitlements_for returns the entitlements of an identity, before its tenant is
-- known.
CREATE FUNCTION veho.entitlements_for(p_kind text, p_issuer text, p_subject text, p_groups text[])
RETURNS TABLE (id uuid, tenant_id uuid, role text)
LANGUAGE sql STABLE SECURITY DEFINER SET search_path = veho, pg_temp
AS $$
	SELECT e.id, e.tenant_id, e.role FROM veho.entitlements e
	 WHERE e.kind = p_kind AND e.issuer = p_issuer
	   AND (e.subject = p_subject OR e."group" = ANY (p_groups))
	   AND (e.tenant_id IS NULL OR (SELECT t.state = 'active' FROM veho.tenants t WHERE t.id = e.tenant_id))
$$;
REVOKE ALL ON FUNCTION veho.entitlements_for(text, text, text, text[]) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION veho.entitlements_for(text, text, text, text[]) TO veho_api;
GRANT SELECT, INSERT, DELETE ON veho.entitlements TO veho_api;

-- ---------------------------------------------------------------------------
-- Grants on named policies (.specs/veho-api-server.md §5.1, SEC-21): a
-- grantee (key:<name>, oidc:<sub>, spiffe:<id> or group:<name>) gets submit,
-- read or manage on a policy, or the tenant-wide submit_inline.

CREATE TABLE veho.policy_grants (
	tenant_id  uuid NOT NULL REFERENCES veho.tenants (id),
	policy     text CHECK (policy <> ''),
	grantee    text NOT NULL CHECK (grantee ~ '^(key|oidc|spiffe|group):.+'),
	permission text NOT NULL CHECK (permission IN ('submit', 'read', 'manage', 'submit_inline')),
	created_at timestamptz NOT NULL DEFAULT now(),
	CHECK ((permission = 'submit_inline') = (policy IS NULL))
);
CREATE UNIQUE INDEX policy_grants_unique ON veho.policy_grants (tenant_id, coalesce(policy, ''), grantee, permission);

ALTER TABLE veho.policy_grants ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON veho.policy_grants
	USING (tenant_id = veho.current_tenant()) WITH CHECK (tenant_id = veho.current_tenant());
GRANT SELECT, INSERT, DELETE ON veho.policy_grants TO veho_api;

-- ---------------------------------------------------------------------------
-- Administration (.specs/veho-api-server.md §5.2): a platform administrator
-- has no tenant, so its reads and changes of tenants cross them; a tenant
-- admin changes its own tenant's settings.

CREATE FUNCTION veho.list_tenants() RETURNS TABLE (id uuid, name text, state text, quotas jsonb, settings jsonb, created_at timestamptz)
LANGUAGE sql STABLE SECURITY DEFINER SET search_path = veho, pg_temp
AS $$ SELECT id, name, state, quotas, settings, created_at FROM veho.tenants ORDER BY name $$;

CREATE FUNCTION veho.update_tenant(p_id uuid, p_state text, p_quotas jsonb)
RETURNS TABLE (id uuid, name text, state text, quotas jsonb, settings jsonb, created_at timestamptz)
LANGUAGE sql SECURITY DEFINER SET search_path = veho, pg_temp
AS $$
	UPDATE veho.tenants t SET state = coalesce(p_state, t.state), quotas = coalesce(p_quotas, t.quotas)
	 WHERE t.id = p_id
	RETURNING t.id, t.name, t.state, t.quotas, t.settings, t.created_at
$$;

CREATE FUNCTION veho.set_tenant_settings(p_settings jsonb) RETURNS jsonb
LANGUAGE sql SECURITY DEFINER SET search_path = veho, pg_temp
AS $$ UPDATE veho.tenants SET settings = p_settings WHERE id = veho.current_tenant() RETURNING settings $$;

REVOKE ALL ON FUNCTION veho.list_tenants(), veho.update_tenant(uuid, text, jsonb), veho.set_tenant_settings(jsonb) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION veho.list_tenants(), veho.update_tenant(uuid, text, jsonb), veho.set_tenant_settings(jsonb) TO veho_api;

-- ---------------------------------------------------------------------------
-- Webhooks (.specs/veho-webhooks.md): a tenant's HTTPS URLs, and the outbox
-- of the events they receive. An event is written by a trigger, in the
-- transaction that moves a job to a terminal state (INV-1), whichever code
-- path does it. The secret of a webhook is in the secret store, under
-- webhook.<id> (and webhook.<id>.prev during a rotation): never here.

CREATE TABLE veho.webhooks (
	id                   uuid PRIMARY KEY DEFAULT gen_random_uuid(),
	tenant_id            uuid NOT NULL REFERENCES veho.tenants (id),
	name                 text NOT NULL CHECK (name ~ '^[a-z0-9][a-z0-9._-]{0,62}$'),
	url                  text NOT NULL CHECK (url ~ '^https://' AND length(url) <= 2048),
	events               text[] NOT NULL CHECK (events <> '{}' AND events <@ ARRAY['job.succeeded', 'job.failed', 'job.cancelled']),
	enabled              boolean NOT NULL DEFAULT true,
	disabled_reason      text,
	-- while set and in the future, deliveries are signed with the previous
	-- secret too
	prev_secret_until    timestamptz,
	consecutive_failures int NOT NULL DEFAULT 0,
	created_at           timestamptz NOT NULL DEFAULT now(),
	last_success_at      timestamptz,
	last_failure_at      timestamptz,
	UNIQUE (tenant_id, name),
	UNIQUE (tenant_id, id)
);

CREATE TABLE veho.webhook_outbox (
	id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
	tenant_id       uuid NOT NULL,
	webhook_id      uuid NOT NULL,
	event_type      text NOT NULL,
	payload         jsonb NOT NULL,
	created_at      timestamptz NOT NULL DEFAULT now(),
	state           text NOT NULL DEFAULT 'pending' CHECK (state IN ('pending', 'delivered', 'abandoned')),
	attempts        int NOT NULL DEFAULT 0,
	next_attempt_at timestamptz NOT NULL DEFAULT now(),
	finished_at     timestamptz,
	last_class      text,
	FOREIGN KEY (tenant_id, webhook_id) REFERENCES veho.webhooks (tenant_id, id) ON DELETE CASCADE
);
CREATE INDEX webhook_outbox_pending ON veho.webhook_outbox (webhook_id, created_at) WHERE state = 'pending';
CREATE INDEX webhook_outbox_finished ON veho.webhook_outbox (finished_at) WHERE state <> 'pending';

ALTER TABLE veho.webhooks ENABLE ROW LEVEL SECURITY;
ALTER TABLE veho.webhook_outbox ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON veho.webhooks
	USING (tenant_id = veho.current_tenant()) WITH CHECK (tenant_id = veho.current_tenant());
CREATE POLICY tenant_isolation ON veho.webhook_outbox
	USING (tenant_id = veho.current_tenant()) WITH CHECK (tenant_id = veho.current_tenant());
GRANT SELECT, INSERT, UPDATE, DELETE ON veho.webhooks TO veho_api;
GRANT SELECT ON veho.webhook_outbox TO veho_api;
GRANT SELECT ON veho.webhooks TO veho_worker;

-- The event of a job that reaches a terminal state. A job that is reopened
-- (retry) and settles again is announced again, with its own event id.
CREATE FUNCTION veho.job_settled_event() RETURNS trigger
LANGUAGE plpgsql SECURITY DEFINER SET search_path = veho, pg_temp
AS $$
DECLARE
	ev text := CASE NEW.state WHEN 'SUCCEEDED' THEN 'job.succeeded' WHEN 'FAILED' THEN 'job.failed' ELSE 'job.cancelled' END;
	done int;
	failed int;
BEGIN
	IF NOT EXISTS (SELECT 1 FROM veho.webhooks w WHERE w.tenant_id = NEW.tenant_id AND w.enabled AND ev = ANY (w.events)) THEN
		RETURN NEW;
	END IF;
	SELECT count(*) FILTER (WHERE t.state = 'SUCCEEDED'), count(*) FILTER (WHERE t.state = 'FAILED')
	  INTO done, failed FROM veho.transfers t WHERE t.job_id = NEW.id;
	INSERT INTO veho.webhook_outbox (tenant_id, webhook_id, event_type, payload)
	SELECT w.tenant_id, w.id, ev, jsonb_build_object(
		'tenant_id', NEW.tenant_id,
		'job', jsonb_build_object('id', NEW.id, 'state', NEW.state,
			'settled_at', coalesce(NEW.settled_at, NEW.finished_at, now()),
			'destinations', jsonb_build_object('succeeded', done, 'failed', failed)))
	FROM veho.webhooks w WHERE w.tenant_id = NEW.tenant_id AND w.enabled AND ev = ANY (w.events);
	RETURN NEW;
END $$;
CREATE TRIGGER jobs_settled_event AFTER UPDATE OF state ON veho.jobs
	FOR EACH ROW WHEN (NEW.state IN ('SUCCEEDED', 'FAILED', 'CANCELLED') AND OLD.state IS DISTINCT FROM NEW.state)
	EXECUTE FUNCTION veho.job_settled_event();

-- What the dispatcher (veho_worker) does across tenants: the head of each
-- webhook's queue when it is due, the outcome of a delivery, and the
-- retention. Per-webhook queues: a stuck webhook delays only itself (W-6).
CREATE FUNCTION veho.due_webhook_events(p_limit int)
RETURNS TABLE (id uuid, tenant_id uuid, webhook_id uuid, event_type text, payload jsonb, created_at timestamptz, attempts int,
               url text, prev_secret_until timestamptz)
LANGUAGE sql STABLE SECURITY DEFINER SET search_path = veho, pg_temp
AS $$
	SELECT h.id, h.tenant_id, h.webhook_id, h.event_type, h.payload, h.created_at, h.attempts, w.url, w.prev_secret_until
	  FROM (SELECT DISTINCT ON (o.webhook_id) o.* FROM veho.webhook_outbox o WHERE o.state = 'pending'
	         ORDER BY o.webhook_id, o.created_at) h
	  JOIN veho.webhooks w ON w.id = h.webhook_id AND w.enabled
	 WHERE h.next_attempt_at <= now()
	 ORDER BY h.next_attempt_at
	 LIMIT p_limit
$$;

-- p_ok: delivered; otherwise the event waits p_wait, or is abandoned after
-- p_max attempts, and a webhook that fails p_disable times in a row is
-- disabled. Returns the event's new state and whether the webhook was
-- disabled by this call.
CREATE FUNCTION veho.webhook_delivered(p_id uuid, p_ok boolean, p_class text, p_wait interval, p_max int, p_disable int)
RETURNS TABLE (state text, disabled boolean)
LANGUAGE plpgsql SECURITY DEFINER SET search_path = veho, pg_temp
AS $$
DECLARE
	wh uuid;
	n int;
	st text;
	dis boolean := false;
BEGIN
	IF p_ok THEN
		UPDATE veho.webhook_outbox o SET state = 'delivered', attempts = o.attempts + 1, finished_at = now(), last_class = p_class
		 WHERE o.id = p_id AND o.state = 'pending' RETURNING o.webhook_id INTO wh;
		UPDATE veho.webhooks w SET consecutive_failures = 0, last_success_at = now() WHERE w.id = wh;
		RETURN QUERY SELECT 'delivered'::text, false;
		RETURN;
	END IF;
	UPDATE veho.webhook_outbox o
	   SET attempts = o.attempts + 1, last_class = p_class,
	       state = CASE WHEN o.attempts + 1 >= p_max THEN 'abandoned' ELSE 'pending' END,
	       finished_at = CASE WHEN o.attempts + 1 >= p_max THEN now() END,
	       next_attempt_at = now() + p_wait
	 WHERE o.id = p_id AND o.state = 'pending' RETURNING o.webhook_id, o.state INTO wh, st;
	UPDATE veho.webhooks w SET consecutive_failures = w.consecutive_failures + 1, last_failure_at = now() WHERE w.id = wh
	RETURNING w.consecutive_failures INTO n;
	IF n >= p_disable THEN
		UPDATE veho.webhooks w SET enabled = false, disabled_reason = n || ' consecutive failed deliveries'
		 WHERE w.id = wh AND w.enabled;
		dis := FOUND;
	END IF;
	RETURN QUERY SELECT coalesce(st, 'pending'), dis;
END $$;

-- Retention (§3.4): delivered and abandoned events after their own times,
-- anything older than the abandoned time, and the oldest pending events of
-- a webhook beyond p_cap, abandoned. Returns the events abandoned for the cap.
CREATE FUNCTION veho.purge_webhook_outbox(p_delivered interval, p_abandoned interval, p_cap int) RETURNS int
LANGUAGE plpgsql SECURITY DEFINER SET search_path = veho, pg_temp
AS $$
DECLARE capped int;
BEGIN
	DELETE FROM veho.webhook_outbox o WHERE
		(o.state = 'delivered' AND o.finished_at < now() - p_delivered)
		OR (o.state = 'abandoned' AND o.finished_at < now() - p_abandoned)
		OR (o.state = 'pending' AND o.created_at < now() - p_abandoned);
	WITH ranked AS (
		SELECT o.id, row_number() OVER (PARTITION BY o.webhook_id ORDER BY o.created_at DESC) AS r
		  FROM veho.webhook_outbox o WHERE o.state = 'pending')
	UPDATE veho.webhook_outbox o SET state = 'abandoned', finished_at = now(), last_class = 'capped'
	  FROM ranked WHERE ranked.id = o.id AND ranked.r > p_cap;
	GET DIAGNOSTICS capped = ROW_COUNT;
	RETURN capped;
END $$;

-- Pending events across tenants, for the metric (no tenant is named).
CREATE FUNCTION veho.webhook_outbox_pending() RETURNS bigint
LANGUAGE sql STABLE SECURITY DEFINER SET search_path = veho, pg_temp
AS $$ SELECT count(*) FROM veho.webhook_outbox WHERE state = 'pending' $$;

REVOKE ALL ON FUNCTION veho.job_settled_event(), veho.due_webhook_events(int),
	veho.webhook_delivered(uuid, boolean, text, interval, int, int), veho.purge_webhook_outbox(interval, interval, int),
	veho.webhook_outbox_pending() FROM PUBLIC;
GRANT EXECUTE ON FUNCTION veho.due_webhook_events(int), veho.webhook_delivered(uuid, boolean, text, interval, int, int),
	veho.purge_webhook_outbox(interval, interval, int), veho.webhook_outbox_pending() TO veho_worker;

-- ---------------------------------------------------------------------------
-- The endpoints' network checks (.specs/veho-network-checks.md §4): the
-- flows, found by the API node, and each executor's results.

CREATE TABLE veho.endpoint_checks (
	id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
	tenant_id   uuid NOT NULL,
	endpoint_id uuid NOT NULL,
	flows       jsonb NOT NULL DEFAULT '[]',
	error       text,
	nodes       text[] NOT NULL,
	created_at  timestamptz NOT NULL DEFAULT now(),
	UNIQUE (tenant_id, id),
	FOREIGN KEY (tenant_id, endpoint_id) REFERENCES veho.endpoints (tenant_id, id) ON DELETE CASCADE
);
CREATE INDEX endpoint_checks_endpoint ON veho.endpoint_checks (endpoint_id, created_at DESC);

CREATE TABLE veho.endpoint_check_results (
	check_id   uuid NOT NULL,
	tenant_id  uuid NOT NULL,
	node_id    text NOT NULL,
	region     text NOT NULL,
	results    jsonb NOT NULL,
	checked_at timestamptz NOT NULL DEFAULT now(),
	PRIMARY KEY (check_id, node_id),
	FOREIGN KEY (tenant_id, check_id) REFERENCES veho.endpoint_checks (tenant_id, id) ON DELETE CASCADE
);

ALTER TABLE veho.endpoint_checks ENABLE ROW LEVEL SECURITY;
ALTER TABLE veho.endpoint_check_results ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON veho.endpoint_checks
	USING (tenant_id = veho.current_tenant()) WITH CHECK (tenant_id = veho.current_tenant());
CREATE POLICY tenant_isolation ON veho.endpoint_check_results
	USING (tenant_id = veho.current_tenant()) WITH CHECK (tenant_id = veho.current_tenant());
GRANT SELECT, INSERT, DELETE ON veho.endpoint_checks TO veho_api;
GRANT SELECT ON veho.endpoint_check_results TO veho_api;
GRANT SELECT ON veho.endpoint_checks TO veho_worker;
GRANT SELECT, INSERT ON veho.endpoint_check_results TO veho_worker;

-- The checks this executor has to answer, across tenants: it names it, the
-- check is not older than p_within, and it has not answered.
CREATE FUNCTION veho.pending_endpoint_checks(p_node text, p_within interval) RETURNS TABLE (tenant_id uuid, check_id uuid)
LANGUAGE sql STABLE SECURITY DEFINER SET search_path = veho, pg_temp
AS $$
	SELECT c.tenant_id, c.id FROM veho.endpoint_checks c
	WHERE p_node = ANY (c.nodes) AND c.created_at > now() - p_within
	  AND NOT EXISTS (SELECT 1 FROM veho.endpoint_check_results r WHERE r.check_id = c.id AND r.node_id = p_node)
	ORDER BY c.created_at
	LIMIT 16
$$;
REVOKE ALL ON FUNCTION veho.pending_endpoint_checks(text, interval) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION veho.pending_endpoint_checks(text, interval) TO veho_worker;

-- The endpoints due for a periodic network check (veho-network-checks.md
-- §11), across tenants: identifiers only; never checked first, then the
-- oldest check.
CREATE FUNCTION veho.due_endpoint_checks(p_interval interval, p_limit int) RETURNS TABLE (tenant_id uuid, endpoint_name text)
LANGUAGE sql STABLE SECURITY DEFINER SET search_path = veho, pg_temp
AS $$
	SELECT e.tenant_id, e.name FROM veho.endpoints e
	LEFT JOIN LATERAL (SELECT max(c.created_at) AS last FROM veho.endpoint_checks c WHERE c.endpoint_id = e.id) l ON true
	WHERE l.last IS NULL OR l.last < now() - p_interval
	ORDER BY l.last NULLS FIRST, e.created_at
	LIMIT p_limit
$$;

-- The endpoints whose latest complete check has a flow unreachable from
-- some node, across tenants, for the metric (no tenant is named).
CREATE FUNCTION veho.unreachable_endpoints() RETURNS bigint
LANGUAGE sql STABLE SECURITY DEFINER SET search_path = veho, pg_temp
AS $$
	SELECT count(*) FROM veho.endpoints e
	CROSS JOIN LATERAL (
		SELECT c.id FROM veho.endpoint_checks c
		WHERE c.endpoint_id = e.id
		  AND (SELECT count(*) FROM veho.endpoint_check_results r WHERE r.check_id = c.id) >= cardinality(c.nodes)
		ORDER BY c.created_at DESC LIMIT 1) last
	WHERE EXISTS (SELECT 1 FROM veho.endpoint_check_results r, jsonb_array_elements(r.results) x
	              WHERE r.check_id = last.id AND x->>'status' = 'unreachable')
$$;
REVOKE ALL ON FUNCTION veho.due_endpoint_checks(interval, int), veho.unreachable_endpoints() FROM PUBLIC;
GRANT EXECUTE ON FUNCTION veho.due_endpoint_checks(interval, int), veho.unreachable_endpoints() TO veho_worker;
