-- 004_housekeeping: the cross-tenant reads and repairs of the housekeeping
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
