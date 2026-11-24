//! SQL toolkit configuration, admission, egress-guarded drivers, and tools.
//!
//! Preserves the SDK's two-tool catalog (`execute_sql`, `list_tables_and_columns`)
//! for `PostgreSQL` and `MySQL` behind the `toolkit-sql` feature. Connections
//! dial through `elitea-egress-guard` (public by default; private targets only
//! via `ELITEA_DB_EGRESS_ALLOWLIST` permits), pin the TCP socket to the checked
//! IP while verifying TLS against the original hostname (`PgSslMode::VerifyFull`,
//! `MySqlSslMode::VerifyIdentity`), and treat arbitrary SQL as an effect.

pub(in crate::toolkits) mod client;
pub(in crate::toolkits) mod config;
pub(in crate::toolkits) mod lexer;
pub(in crate::toolkits) mod project;
pub(in crate::toolkits) mod tools;
