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
//! | `ELITEA_VECTOR_MAX_COLLECTIONS` | a **soft** cap on `emb_*` collections: creating one beyond it is `RESOURCE_EXHAUSTED`, but concurrent creators can overshoot it by their number. Use `ELITEA_VECTOR_ALLOWED_SPACES` for a hard bound | `64` |
//! | `ELITEA_VECTOR_MAX_DIMENSION` | the largest space dimension a caller may create | `4096` |
//! | `ELITEA_VECTOR_ALLOWED_SPACES` | comma-separated `slug:dimension`; when set, only these spaces may be created: the **hard** bound on collections (recommended in production) | any |
//! | `ELITEA_VECTOR_INTROSPECTION_URL` | elitea-main's private control listener, `https://…` | required |
//! | `ELITEA_VECTOR_INTROSPECTION_CA_FILE` | CA of elitea-main's server certificate | required |
//! | `ELITEA_VECTOR_INTROSPECTION_SERVER_NAME` | TLS server name of elitea-main | the URL host |
//! | `ELITEA_VECTOR_INTROSPECTION_TIMEOUT_MS` | per-call timeout | `2000` |
//! | `ELITEA_VECTOR_INTROSPECTION_CACHE_MAX_SECONDS` | longest a verified engine callback token is cached (a worker claim token: 5 s); above 300 it is clamped to 300 and logged | `60` |
//!
//! A Qdrant served over `https://` with a private CA is trusted through
//! `SSL_CERT_FILE`: the Qdrant client reads the platform trust store only.

use std::collections::HashSet;

use crate::auth::{DEFAULT_MAX_TTL_SECONDS, HARD_MAX_TTL_SECONDS};
use crate::layout::Space;
use crate::service::DEFAULT_MAX_DIMENSION;
use crate::store::DEFAULT_MAX_COLLECTIONS;
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
    pub max_collections: usize,
    pub max_dimension: u32,
    pub allowed_spaces: Option<HashSet<Space>>,
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
        let admin_identities = admin_identities(get("ELITEA_VECTOR_ADMIN_IDENTITIES").as_deref())?;
        let allowed_spaces = get("ELITEA_VECTOR_ALLOWED_SPACES")
            .map(|value| parse_spaces(&value))
            .transpose()?;
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
            max_collections: usize::try_from(number(
                "ELITEA_VECTOR_MAX_COLLECTIONS",
                DEFAULT_MAX_COLLECTIONS as u64,
                100_000,
            )?)
            .unwrap_or(DEFAULT_MAX_COLLECTIONS),
            max_dimension: u32::try_from(number(
                "ELITEA_VECTOR_MAX_DIMENSION",
                u64::from(DEFAULT_MAX_DIMENSION),
                65_536,
            )?)
            .unwrap_or(DEFAULT_MAX_DIMENSION),
            allowed_spaces,
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
                u64::try_from(DEFAULT_MAX_TTL_SECONDS).unwrap_or(60),
                86_400,
            )?)
            .map_or(DEFAULT_MAX_TTL_SECONDS, clamp_cache_seconds),
        })
    }
}

/// The configured cache cap held to [`HARD_MAX_TTL_SECONDS`], logging when it
/// had to change.
fn clamp_cache_seconds(seconds: i64) -> i64 {
    if seconds > HARD_MAX_TTL_SECONDS {
        tracing::warn!(
            configured = seconds,
            applied = HARD_MAX_TTL_SECONDS,
            "ELITEA_VECTOR_INTROSPECTION_CACHE_MAX_SECONDS exceeds the hard maximum; clamped"
        );
        return HARD_MAX_TTL_SECONDS;
    }
    seconds
}

fn admin_identities(value: Option<&str>) -> Result<HashSet<String>, ConfigError> {
    let identities: HashSet<String> = value
        .map(|value| {
            value
                .split(',')
                .map(str::trim)
                .filter(|identity| !identity.is_empty())
                .map(str::to_owned)
                .collect()
        })
        .unwrap_or_default();
    for identity in &identities {
        if !(identity.starts_with("dns:") || identity.starts_with("spiffe://")) {
            return Err(ConfigError(
                "ELITEA_VECTOR_ADMIN_IDENTITIES entries are dns:<name> or spiffe:// URIs"
                    .to_owned(),
            ));
        }
    }
    Ok(identities)
}

/// `slug:dimension,slug:dimension`.
fn parse_spaces(value: &str) -> Result<HashSet<Space>, ConfigError> {
    value
        .split(',')
        .map(str::trim)
        .filter(|entry| !entry.is_empty())
        .map(|entry| {
            entry
                .rsplit_once(':')
                .and_then(|(slug, dimension)| Space::new(slug, dimension.parse().ok()?).ok())
                .ok_or_else(|| {
                    ConfigError(format!(
                        "ELITEA_VECTOR_ALLOWED_SPACES entry {entry:?} is not slug:dimension"
                    ))
                })
        })
        .collect()
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
        assert_eq!(config.max_collections, 64);
        assert_eq!(config.max_dimension, 4096);
        assert!(config.allowed_spaces.is_none());
        assert_eq!(config.introspection_timeout, Duration::from_secs(2));
        assert_eq!(config.introspection_cache_max_seconds, 60);
    }

    #[test]
    fn the_cache_cap_is_clamped_to_five_minutes() {
        for (value, want) in [
            ("1", 1),
            ("120", 120),
            ("300", 300),
            ("301", 300),
            ("86400", 300),
        ] {
            let mut env = base();
            env.insert("ELITEA_VECTOR_INTROSPECTION_CACHE_MAX_SECONDS", value);
            assert_eq!(
                load(&env).expect("valid").introspection_cache_max_seconds,
                want,
                "{value}"
            );
        }
    }

    #[test]
    fn the_space_limits_are_read() {
        let mut env = base();
        env.insert("ELITEA_VECTOR_MAX_COLLECTIONS", "8");
        env.insert("ELITEA_VECTOR_MAX_DIMENSION", "3072");
        env.insert(
            "ELITEA_VECTOR_ALLOWED_SPACES",
            "text-embedding-3-small:1536, bge-m3:1024",
        );
        let config = load(&env).expect("valid");
        assert_eq!(config.max_collections, 8);
        assert_eq!(config.max_dimension, 3072);
        let allowed = config.allowed_spaces.expect("allowlist");
        assert_eq!(allowed.len(), 2);
        assert!(allowed.contains(&Space::new("bge-m3", 1024).expect("space")));
        for bad in ["bge-m3", "bge-m3:x", "BAD:4", ":4", "bge-m3:0"] {
            let mut env = base();
            env.insert("ELITEA_VECTOR_ALLOWED_SPACES", bad);
            assert!(load(&env).is_err(), "{bad}");
        }
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
