//! Configuration, read from the environment.
//!
//! | Variable | Meaning | Default |
//! | --- | --- | --- |
//! | `ELITEA_EXTRACTOR_SOCKET` | the Unix socket to serve on; its directory is shared with the worker | `/run/elitea-extract/extractor.sock` |
//! | `ELITEA_EXTRACTOR_MAX_INPUT_BYTES` | the most bytes accepted for one document; a request's own limit is lowered to it | `52428800` (50 MiB) |
//! | `ELITEA_EXTRACTOR_MAX_PAGES` | the most pages read from a paged format | `1000` |
//! | `ELITEA_EXTRACTOR_MAX_OUTPUT_BYTES` | the most text returned for one document | `20971520` (20 MiB) |
//! | `ELITEA_EXTRACTOR_TIMEOUT_MILLIS` | the longest one extraction may run after its bytes arrived | `120000` |
//! | `ELITEA_EXTRACTOR_STREAM_IDLE_MILLIS` | the longest wait for the next chunk of a document | `30000` |
//! | `ELITEA_EXTRACTOR_CONCURRENCY` | documents extracted at once; a further request waits its own timeout for a slot, then is refused `RESOURCE_EXHAUSTED` | `2` |
//! | `RUST_LOG` | log filter (stderr) | `info` |
//!
//! There is no network setting: the process opens no connection, and it
//! exports no spans (the sidecar crate's telemetry is not linked).
//!
//! MEMORY. Extraction expands a document several times over in memory. When
//! the process runs under a cgroup v2 memory limit, the input cap is lowered
//! so that `concurrency` documents at the cap fit in a fraction of the
//! limit ([`Config::fit_memory`]); a document over that is skipped
//! `too_large` instead of ending the process with an OOM kill the worker
//! would read as `extract_failed`.

use std::path::PathBuf;
use std::time::Duration;

use crate::service::Caps;

/// Bytes of memory the extractor is assumed to need per byte of input, at
/// the worst: a PDF's decompressed streams, the page objects, the text.
const MEMORY_PER_INPUT_BYTE: u64 = 6;

/// A configuration that cannot be used.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct ConfigError(pub String);

impl std::fmt::Display for ConfigError {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.write_str(&self.0)
    }
}

impl std::error::Error for ConfigError {}

/// The process configuration.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Config {
    pub socket: PathBuf,
    pub caps: Caps,
    pub concurrency: usize,
    pub stream_idle: Duration,
}

impl Config {
    /// Reads the configuration through `lookup` (the process environment in
    /// production).
    ///
    /// # Errors
    /// [`ConfigError`] naming the first invalid variable.
    pub fn from_lookup(lookup: impl Fn(&str) -> Option<String>) -> Result<Self, ConfigError> {
        let get = |name: &str| lookup(name).filter(|value| !value.trim().is_empty());
        let number = |name: &str, default: u64| -> Result<u64, ConfigError> {
            get(name).map_or(Ok(default), |value| {
                value
                    .trim()
                    .parse::<u64>()
                    .ok()
                    .filter(|n| *n > 0)
                    .ok_or_else(|| ConfigError(format!("{name} must be a positive integer")))
            })
        };
        let defaults = Caps::default();
        let socket = PathBuf::from(
            get("ELITEA_EXTRACTOR_SOCKET")
                .unwrap_or_else(|| "/run/elitea-extract/extractor.sock".to_owned()),
        );
        if !socket.is_absolute() {
            return Err(ConfigError(
                "ELITEA_EXTRACTOR_SOCKET must be an absolute path".to_owned(),
            ));
        }
        let max_pages = number("ELITEA_EXTRACTOR_MAX_PAGES", defaults.max_pages as u64)?;
        let max_pages = usize::try_from(max_pages)
            .ok()
            .filter(|pages| u32::try_from(*pages).is_ok())
            .ok_or_else(|| ConfigError("ELITEA_EXTRACTOR_MAX_PAGES is too large".to_owned()))?;
        let timeout = number(
            "ELITEA_EXTRACTOR_TIMEOUT_MILLIS",
            u64::try_from(defaults.timeout.as_millis()).unwrap_or(u64::MAX),
        )?;
        let stream_idle = number("ELITEA_EXTRACTOR_STREAM_IDLE_MILLIS", 30_000)?;
        let concurrency = number("ELITEA_EXTRACTOR_CONCURRENCY", 2)?;
        let concurrency = usize::try_from(concurrency)
            .ok()
            .filter(|n| *n <= 64)
            .ok_or_else(|| {
                ConfigError("ELITEA_EXTRACTOR_CONCURRENCY must be at most 64".to_owned())
            })?;
        Ok(Self {
            socket,
            caps: Caps {
                max_input_bytes: number(
                    "ELITEA_EXTRACTOR_MAX_INPUT_BYTES",
                    defaults.max_input_bytes,
                )?,
                max_pages,
                max_output_bytes: number(
                    "ELITEA_EXTRACTOR_MAX_OUTPUT_BYTES",
                    defaults.max_output_bytes,
                )?,
                timeout: Duration::from_millis(timeout),
            },
            concurrency,
            stream_idle: Duration::from_millis(stream_idle),
        })
    }

    /// Lowers the input cap so that `concurrency` documents at the cap,
    /// expanded `MEMORY_PER_INPUT_BYTE` (six) times, fit in half of
    /// `memory_limit` (the container's `memory.max`); the other half is the
    /// process itself and the parser's own overhead. Returns the cap it
    /// lowered to, when it did.
    pub fn fit_memory(&mut self, memory_limit: u64) -> Option<u64> {
        let concurrency = u64::try_from(self.concurrency.max(1)).unwrap_or(1);
        let budget = memory_limit / 2 / concurrency / MEMORY_PER_INPUT_BYTE;
        if budget < self.caps.max_input_bytes {
            // A cap under 1 MiB would skip every real document; such a limit
            // is a deployment mistake and is reported as what it would do.
            self.caps.max_input_bytes = budget.max(1 << 20);
            return Some(self.caps.max_input_bytes);
        }
        None
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::collections::HashMap;

    fn config(pairs: &[(&str, &str)]) -> Result<Config, ConfigError> {
        let map: HashMap<String, String> = pairs
            .iter()
            .map(|(k, v)| ((*k).to_owned(), (*v).to_owned()))
            .collect();
        Config::from_lookup(|name| map.get(name).cloned())
    }

    #[test]
    fn defaults_are_the_caps_defaults() {
        let config = config(&[]).expect("config");
        assert_eq!(
            config.socket,
            PathBuf::from("/run/elitea-extract/extractor.sock")
        );
        assert_eq!(config.caps, Caps::default());
        assert_eq!(config.concurrency, 2);
        assert_eq!(config.stream_idle, Duration::from_secs(30));
    }

    #[test]
    fn every_number_is_read_and_a_bad_one_is_named() {
        let config = config(&[
            ("ELITEA_EXTRACTOR_SOCKET", "/tmp/x.sock"),
            ("ELITEA_EXTRACTOR_MAX_INPUT_BYTES", "1024"),
            ("ELITEA_EXTRACTOR_MAX_PAGES", "7"),
            ("ELITEA_EXTRACTOR_MAX_OUTPUT_BYTES", "2048"),
            ("ELITEA_EXTRACTOR_TIMEOUT_MILLIS", "500"),
            ("ELITEA_EXTRACTOR_STREAM_IDLE_MILLIS", "250"),
            ("ELITEA_EXTRACTOR_CONCURRENCY", "3"),
        ])
        .expect("config");
        assert_eq!(config.caps.max_input_bytes, 1024);
        assert_eq!(config.caps.max_pages, 7);
        assert_eq!(config.caps.max_output_bytes, 2048);
        assert_eq!(config.caps.timeout, Duration::from_millis(500));
        assert_eq!(config.stream_idle, Duration::from_millis(250));
        assert_eq!(config.concurrency, 3);
        let error = config_error(&[("ELITEA_EXTRACTOR_MAX_PAGES", "0")]);
        assert!(error.0.contains("ELITEA_EXTRACTOR_MAX_PAGES"), "{error}");
        let error = config_error(&[("ELITEA_EXTRACTOR_CONCURRENCY", "65")]);
        assert!(error.0.contains("CONCURRENCY"), "{error}");
        let error = config_error(&[("ELITEA_EXTRACTOR_SOCKET", "relative.sock")]);
        assert!(error.0.contains("absolute"), "{error}");
    }

    fn config_error(pairs: &[(&str, &str)]) -> ConfigError {
        config(pairs).expect_err("refused")
    }

    #[test]
    fn the_input_cap_follows_the_memory_limit() {
        let mut config = config(&[("ELITEA_EXTRACTOR_CONCURRENCY", "2")]).expect("config");
        // 50 MiB × 2 × 6 = 600 MiB needed; a 256 MiB container halves to
        // 128 MiB, split two ways and divided by the expansion: ~10.6 MiB.
        assert_eq!(config.fit_memory(256 << 20), Some((256 << 20) / 2 / 2 / 6));
        // Plenty of memory: unchanged.
        let mut roomy = config.clone();
        roomy.caps.max_input_bytes = 50 << 20;
        assert_eq!(roomy.fit_memory(8 << 30), None);
        // A tiny limit still leaves 1 MiB, so the mistake shows as skips.
        assert_eq!(config.fit_memory(1 << 20), Some(1 << 20));
    }
}
