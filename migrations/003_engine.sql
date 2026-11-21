-- 003_engine: what the engine needs across tenants (.specs/veho-engine.md
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
