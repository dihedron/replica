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
