# Runtime: SQL toolkit enforces egress guard and requires `ELITEA_DB_EGRESS_ALLOWLIST` for private databases (FU-078, 5e-0 / 5e-1)

- **What changed:** The Rust worker `sql` toolkit (`execute_sql` and
  `list_tables_and_columns`) now resolves and classifies every database target
  through `elitea-egress-guard` before opening TCP. Connections pin TCP to the
  checked IP address while TLS verifies the original configured hostname
  (`PgSslMode::VerifyFull` for PostgreSQL, `MySqlSslMode::VerifyIdentity` for
  MySQL). PostgreSQL options no longer read ambient `PG*` environment variables
  or TLS files. A new worker environment variable, `ELITEA_DB_EGRESS_ALLOWLIST`
  (Helm key `worker.dbEgressAllowlist`), controls private database permits.
- **Who is affected:** Deployments that use the `sql` toolkit against databases
  on private networks (RFC 1918, loopback, CGNAT, ULA, or site-local addresses,
  including IPv4-mapped, NAT64, or 6to4 embeddings). Public database endpoints
  continue to work with no configuration change. Link-local and cloud metadata
  addresses are always refused.
- **What to do:** Set `worker.dbEgressAllowlist` in Helm values (or
  `ELITEA_DB_EGRESS_ALLOWLIST` in compose) to a comma-separated list of private
  database CIDRs, IP literals, or `localhost` entries with optional `:port`
  pins, for example `10.20.0.0/16,192.168.50.10:5432`. Bare hostnames do not
  permit private IP ranges. Any malformed entry causes worker startup to fail
  closed.
