//! Configuration, read from the environment.
//!
//! | Variable | Meaning | Default |
//! | --- | --- | --- |
//! | `ELITEA_VECTOR_GRPC_ADDR` | gRPC listener (mTLS) | `0.0.0.0:9470` |
//! | `ELITEA_VECTOR_HEALTH_ADDR` | plain HTTP `/healthz`, `/readyz` | `0.0.0.0:9471` |
//! | `ELITEA_VECTOR_TLS_CERT_FILE`, `ELITEA_VECTOR_TLS_KEY_FILE` | server certificate | required |
//! | `ELITEA_VECTOR_TLS_CLIENT_CA_FILE` | CA of every caller's client certificate | required |
//! | `ELITEA_VECTOR_CLIENT_CERT_FILE`, `ELITEA_VECTOR_CLIENT_KEY_FILE` | client certificate to elitea-main; one DNS or SPIFFE SAN | the server pair |
//! | `ELITEA_VECTOR_ADMIN_IDENTITIES` | comma-separated administrator identities (`dns:<name>` or `spiffe://…`) | none |
//! | `ELITEA_VECTOR_QDRANT_URL` | Qdrant gRPC URL | required |
//! | `ELITEA_VECTOR_QDRANT_API_KEY_FILE` | file holding the Qdrant API key | none |
//! | `ELITEA_VECTOR_COLLECTION_REPLICATION_FACTOR`, `ELITEA_VECTOR_COLLECTION_SHARD_NUMBER` | settings of a new collection | `1`, `1` |
//! | `ELITEA_VECTOR_INTROSPECTION_URL` | elitea-main's private control listener, `https://…` | required |
//! | `ELITEA_VECTOR_INTROSPECTION_CA_FILE` | CA of elitea-main's server certificate | required |
//! | `ELITEA_VECTOR_INTROSPECTION_SERVER_NAME` | TLS server name of elitea-main | the URL host |
//! | `ELITEA_VECTOR_INTROSPECTION_TIMEOUT_MS` | per-call timeout | `2000` |
//! | `ELITEA_VECTOR_INTROSPECTION_CACHE_MAX_SECONDS` | longest a verified token is cached | `300` |
//!
//! A Qdrant served over `https://` with a private CA is trusted through
//! `SSL_CERT_FILE`: the Qdrant client reads the platform trust store only.

use std::collections::HashSet;
use std::net::SocketAddr;
use std::path::PathBuf;
use std::time::Duration;

/// A configuration that cannot be used.
#[derive(Debug, thiserror::Error)]
#[error("{0}")]
pub struct ConfigError(String);

/// The service configuration.
#[derive(Clone, Debug)]
pub struct Config {
    pub grpc_addr: SocketAddr,
    pub health_addr: SocketAddr,
    pub tls_cert_file: PathBuf,
    pub tls_key_file: PathBuf,
    pub tls_client_ca_file: PathBuf,
    pub client_cert_file: PathBuf,
    pub client_key_file: PathBuf,
    pub admin_identities: HashSet<String>,
    pub qdrant_url: String,
    pub qdrant_api_key_file: Option<PathBuf>,
    pub replication_factor: u32,
    pub shard_number: u32,
    pub introspection_url: String,
    pub introspection_ca_file: PathBuf,
    pub introspection_server_name: Option<String>,
    pub introspection_timeout: Duration,
    pub introspection_cache_max_seconds: i64,
}

impl Config {
    /// Reads the configuration through `lookup` (the process environment in
    /// production).
    ///
    /// # Errors
    /// [`ConfigError`] naming the first missing or invalid variable.
    pub fn from_lookup(lookup: impl Fn(&str) -> Option<String>) -> Result<Self, ConfigError> {
        let get = |name: &str| lookup(name).filter(|value| !value.trim().is_empty());
        let required =
            |name: &str| get(name).ok_or_else(|| ConfigError(format!("{name} is required")));
        let address = |name: &str, default: &str| {
            get(name)
                .unwrap_or_else(|| default.to_owned())
                .parse::<SocketAddr>()
                .map_err(|_| ConfigError(format!("{name} must be host:port")))
        };
        let number = |name: &str, default: u64, max: u64| -> Result<u64, ConfigError> {
            match get(name) {
                None => Ok(default),
                Some(value) => value
                    .trim()
                    .parse::<u64>()
                    .ok()
                    .filter(|value| (1..=max).contains(value))
                    .ok_or_else(|| ConfigError(format!("{name} must be 1 to {max}"))),
            }
        };

        let tls_cert_file = PathBuf::from(required("ELITEA_VECTOR_TLS_CERT_FILE")?);
        let tls_key_file = PathBuf::from(required("ELITEA_VECTOR_TLS_KEY_FILE")?);
        let client_cert_file = get("ELITEA_VECTOR_CLIENT_CERT_FILE").map(PathBuf::from);
        let client_key_file = get("ELITEA_VECTOR_CLIENT_KEY_FILE").map(PathBuf::from);
        if client_cert_file.is_some() != client_key_file.is_some() {
            return Err(ConfigError(
                "ELITEA_VECTOR_CLIENT_CERT_FILE and ELITEA_VECTOR_CLIENT_KEY_FILE go together"
                    .to_owned(),
            ));
        }
        let admin_identities = get("ELITEA_VECTOR_ADMIN_IDENTITIES")
            .map(|value| {
                value
                    .split(',')
                    .map(str::trim)
                    .filter(|identity| !identity.is_empty())
                    .map(str::to_owned)
                    .collect::<HashSet<_>>()
            })
            .unwrap_or_default();
        for identity in &admin_identities {
            if !(identity.starts_with("dns:") || identity.starts_with("spiffe://")) {
                return Err(ConfigError(
                    "ELITEA_VECTOR_ADMIN_IDENTITIES entries are dns:<name> or spiffe:// URIs"
                        .to_owned(),
                ));
            }
        }
        let qdrant_url = required("ELITEA_VECTOR_QDRANT_URL")?;
        if !(qdrant_url.starts_with("http://") || qdrant_url.starts_with("https://")) {
            return Err(ConfigError(
                "ELITEA_VECTOR_QDRANT_URL must be http:// or https://".to_owned(),
            ));
        }
        let introspection_url = required("ELITEA_VECTOR_INTROSPECTION_URL")?;
        if !introspection_url.starts_with("https://") {
            return Err(ConfigError(
                "ELITEA_VECTOR_INTROSPECTION_URL must be https://".to_owned(),
            ));
        }
        Ok(Self {
            grpc_addr: address("ELITEA_VECTOR_GRPC_ADDR", "0.0.0.0:9470")?,
            health_addr: address("ELITEA_VECTOR_HEALTH_ADDR", "0.0.0.0:9471")?,
            client_cert_file: client_cert_file.unwrap_or_else(|| tls_cert_file.clone()),
            client_key_file: client_key_file.unwrap_or_else(|| tls_key_file.clone()),
            tls_cert_file,
            tls_key_file,
            tls_client_ca_file: PathBuf::from(required("ELITEA_VECTOR_TLS_CLIENT_CA_FILE")?),
            admin_identities,
            qdrant_url,
            qdrant_api_key_file: get("ELITEA_VECTOR_QDRANT_API_KEY_FILE").map(PathBuf::from),
            replication_factor: u32::try_from(number(
                "ELITEA_VECTOR_COLLECTION_REPLICATION_FACTOR",
                1,
                16,
            )?)
            .unwrap_or(1),
            shard_number: u32::try_from(number("ELITEA_VECTOR_COLLECTION_SHARD_NUMBER", 1, 256)?)
                .unwrap_or(1),
            introspection_url,
            introspection_ca_file: PathBuf::from(required("ELITEA_VECTOR_INTROSPECTION_CA_FILE")?),
            introspection_server_name: get("ELITEA_VECTOR_INTROSPECTION_SERVER_NAME"),
            introspection_timeout: Duration::from_millis(number(
                "ELITEA_VECTOR_INTROSPECTION_TIMEOUT_MS",
                2000,
                60_000,
            )?),
            introspection_cache_max_seconds: i64::try_from(number(
                "ELITEA_VECTOR_INTROSPECTION_CACHE_MAX_SECONDS",
                300,
                86_400,
            )?)
            .unwrap_or(300),
        })
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::collections::HashMap;

    fn base() -> HashMap<&'static str, &'static str> {
        HashMap::from([
            ("ELITEA_VECTOR_TLS_CERT_FILE", "/c"),
            ("ELITEA_VECTOR_TLS_KEY_FILE", "/k"),
            ("ELITEA_VECTOR_TLS_CLIENT_CA_FILE", "/ca"),
            ("ELITEA_VECTOR_QDRANT_URL", "http://qdrant:6334"),
            (
                "ELITEA_VECTOR_INTROSPECTION_URL",
                "https://elitea-main:9443",
            ),
            ("ELITEA_VECTOR_INTROSPECTION_CA_FILE", "/mca"),
        ])
    }

    fn load(env: &HashMap<&'static str, &'static str>) -> Result<Config, ConfigError> {
        Config::from_lookup(|name| env.get(name).map(|value| (*value).to_owned()))
    }

    #[test]
    fn defaults_apply() {
        let config = load(&base()).expect("valid");
        assert_eq!(config.grpc_addr.port(), 9470);
        assert_eq!(config.health_addr.port(), 9471);
        assert_eq!(config.client_cert_file, PathBuf::from("/c"));
        assert!(config.admin_identities.is_empty());
        assert_eq!(config.introspection_timeout, Duration::from_secs(2));
    }

    #[test]
    fn invalid_values_are_refused() {
        for (name, value) in [
            ("ELITEA_VECTOR_INTROSPECTION_URL", "http://elitea-main:9443"),
            ("ELITEA_VECTOR_QDRANT_URL", "qdrant:6334"),
            ("ELITEA_VECTOR_ADMIN_IDENTITIES", "elitea-main"),
            ("ELITEA_VECTOR_CLIENT_CERT_FILE", "/only-cert"),
            ("ELITEA_VECTOR_INTROSPECTION_TIMEOUT_MS", "0"),
        ] {
            let mut env = base();
            env.insert(name, value);
            assert!(load(&env).is_err(), "{name}={value}");
        }
        let mut env = base();
        env.remove("ELITEA_VECTOR_TLS_CLIENT_CA_FILE");
        assert!(load(&env).is_err());
    }
}
