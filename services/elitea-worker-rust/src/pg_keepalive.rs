//! FU-221: server-side TCP keepalive for the pools that connect directly to
//! `PostgreSQL` (the Worker's `AgentState` pool and the sandbox supervisor's
//! receipt ledger).
//!
//! sqlx 0.8 sets no client TCP keepalive (its socket only gets `TCP_NODELAY`)
//! and exposes no hook for one, so a half-open connection is detected from the
//! server side instead: each session asks `PostgreSQL` to probe this client
//! after 20 s idle, every 5 s, 3 times. A force-killed Worker or supervisor
//! then releases its locks and connection slots in about 35 s, not hours.
//!
//! The values travel as `-c` startup options. Never apply this to a pool that
//! goes through pgbouncer: pgbouncer refuses unknown startup parameters.

use sqlx::postgres::PgConnectOptions;

/// `tcp_keepalives_idle`, seconds.
pub(crate) const TCP_KEEPALIVES_IDLE_SECONDS: u32 = 20;
/// `tcp_keepalives_interval`, seconds.
pub(crate) const TCP_KEEPALIVES_INTERVAL_SECONDS: u32 = 5;
/// `tcp_keepalives_count`, probes.
pub(crate) const TCP_KEEPALIVES_COUNT: u32 = 3;

/// Add the shared server keepalive settings to a direct `PostgreSQL` connection.
#[must_use]
pub(crate) fn with_server_tcp_keepalives(options: PgConnectOptions) -> PgConnectOptions {
    options.options([
        ("tcp_keepalives_idle", TCP_KEEPALIVES_IDLE_SECONDS),
        ("tcp_keepalives_interval", TCP_KEEPALIVES_INTERVAL_SECONDS),
        ("tcp_keepalives_count", TCP_KEEPALIVES_COUNT),
    ])
}

#[cfg(test)]
mod tests {
    use crate::test_pg::{connect_options, database_url_var, pool_options};

    #[test]
    fn the_startup_options_carry_the_shared_keepalive_and_keep_existing_options() {
        let options = connect_options("postgres://u@db.internal:5432/agentstate")
            .unwrap()
            .with_server_tcp_keepalives();
        assert_eq!(
            options.get_options(),
            Some(
                "-c tcp_keepalives_idle=20 -c tcp_keepalives_interval=5 -c tcp_keepalives_count=3"
            )
        );
        let operator = connect_options(
            "postgres://u@db.internal:5432/agentstate?options=-c%20statement_timeout%3D5000",
        )
        .unwrap()
        .with_server_tcp_keepalives();
        assert_eq!(
            operator.get_options(),
            Some(
                "-c statement_timeout=5000 -c tcp_keepalives_idle=20 \
                 -c tcp_keepalives_interval=5 -c tcp_keepalives_count=3"
            )
        );
    }

    /// Real `PostgreSQL` over TCP applies the settings to every pooled session.
    /// Skipped when `ELITEA_TEST_DATABASE_URL` is unset.
    #[tokio::test]
    async fn a_pooled_session_runs_with_the_shared_server_keepalive() {
        let Ok(url) = database_url_var("ELITEA_TEST_DATABASE_URL") else {
            eprintln!("skipping keepalive session test: set ELITEA_TEST_DATABASE_URL");
            return;
        };
        let options = connect_options(&url)
            .expect("parse database URL")
            .with_server_tcp_keepalives();
        let pool = pool_options()
            .max_connections(2)
            .connect_with(options)
            .await
            .expect("connect test pool");
        let mut first = pool.acquire().await.expect("first connection");
        let mut second = pool.acquire().await.expect("second connection");
        for connection in [&mut first, &mut second] {
            for (setting, expected) in [
                ("tcp_keepalives_idle", "20"),
                ("tcp_keepalives_interval", "5"),
                ("tcp_keepalives_count", "3"),
            ] {
                let value: String = sqlx::query_scalar(&format!("SHOW {setting}"))
                    .fetch_one(&mut **connection)
                    .await
                    .expect("show setting");
                assert_eq!(value, expected, "{setting}");
            }
        }
        drop((first, second));
        pool.close().await;
    }
}
