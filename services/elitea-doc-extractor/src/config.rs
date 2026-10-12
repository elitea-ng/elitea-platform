//! Configuration, read from the environment.
//!
//! | Variable | Meaning | Default |
//! | --- | --- | --- |
//! | `ELITEA_EXTRACTOR_SOCKET` | the Unix socket to serve on; its directory is shared with the worker | `/run/elitea-extract/extractor.sock` |
//! | `ELITEA_EXTRACTOR_PEER_UIDS` | the uids whose connections are served (comma-separated; the worker's), read from the peer's `SO_PEERCRED`; any other peer is `PERMISSION_DENIED`. `Capabilities` (the health probe) is served to this process's own uid as well, whatever is listed, because the probe is this binary asking itself | this process's own uid |
//! | `ELITEA_EXTRACTOR_MAX_INPUT_BYTES` | the most bytes accepted for one document; a request's own limit is lowered to it | `52428800` (50 MiB) |
//! | `ELITEA_EXTRACTOR_MAX_PAGES` | the most pages read from a paged format | `1000` |
//! | `ELITEA_EXTRACTOR_MAX_OUTPUT_BYTES` | the most text returned for one document | `20971520` (20 MiB) |
//! | `ELITEA_EXTRACTOR_TIMEOUT_MILLIS` | the longest one extraction may run after its bytes arrived; rounded UP to whole seconds for the parser, which the request then waits for plus a short grace | `120000` |
//! | `ELITEA_EXTRACTOR_STREAM_IDLE_MILLIS` | the longest wait for the next chunk of a document | `30000` |
//! | `ELITEA_EXTRACTOR_CONCURRENCY` | documents extracted at once; not below the worker's own document concurrency (4), so a worker that keeps that many in flight never waits | `4` |
//! | `ELITEA_EXTRACTOR_SLOT_WAIT_MILLIS` | the longest a further request waits for a slot before `RESOURCE_EXHAUSTED`; separate from the extraction timeout, so queueing is never reported as exhaustion | `600000` (10 min) |
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
//! would read as `extract_failed`. The fit only ever LOWERS the cap. The
//! limit the defaults need to stand as configured is
//! [`Config::memory_limit_for_caps`]: 50 MiB × 4 × 6 × 2 = 2400 MiB, which
//! is why the chart's and the compose file's default limit is 2560 MiB and
//! not a round 2 GiB (under which the fit would quietly lower the cap to
//! ~44 MiB).

use std::collections::BTreeSet;
use std::path::PathBuf;
use std::time::Duration;

use crate::service::{Capacity, Caps, own_uid};

/// Bytes of memory the extractor is assumed to need per byte of input, at
/// the worst. The document is held ONCE (the bytes read off the stream move
/// into the extractor without a copy, `service.rs` / `extract_document`);
/// on top of that come the parser's decompressed streams, its page objects
/// and the text (xberg's PDF backend was measured at up to four times the
/// input), and a transient chunk-sized receive buffer. One copy plus four
/// of expansion, rounded up to six for the parser's own allocations.
const MEMORY_PER_INPUT_BYTE: u64 = 6;

/// The input cap the memory fit never goes below: a cap under 1 MiB would
/// skip every real document, which says "the memory limit is a mistake"
/// more clearly as skips than as a silently useless process.
const INPUT_CAP_FLOOR: u64 = 1 << 20;

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
    pub capacity: Capacity,
    /// The uids served (`SO_PEERCRED`); the worker's.
    pub peer_uids: BTreeSet<u32>,
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
        let capacity_defaults = Capacity::default();
        let stream_idle = number(
            "ELITEA_EXTRACTOR_STREAM_IDLE_MILLIS",
            u64::try_from(capacity_defaults.stream_idle.as_millis()).unwrap_or(u64::MAX),
        )?;
        let slot_wait = number(
            "ELITEA_EXTRACTOR_SLOT_WAIT_MILLIS",
            u64::try_from(capacity_defaults.slot_wait.as_millis()).unwrap_or(u64::MAX),
        )?;
        let concurrency = number(
            "ELITEA_EXTRACTOR_CONCURRENCY",
            u64::try_from(capacity_defaults.concurrency).unwrap_or(4),
        )?;
        let concurrency = usize::try_from(concurrency)
            .ok()
            .filter(|n| *n <= 64)
            .ok_or_else(|| {
                ConfigError("ELITEA_EXTRACTOR_CONCURRENCY must be at most 64".to_owned())
            })?;
        let peer_uids = match get("ELITEA_EXTRACTOR_PEER_UIDS") {
            None => BTreeSet::from([own_uid()]),
            Some(list) => list
                .split(',')
                .map(str::trim)
                .filter(|part| !part.is_empty())
                .map(|part| {
                    part.parse::<u32>().map_err(|_| {
                        ConfigError(
                            "ELITEA_EXTRACTOR_PEER_UIDS must be comma-separated uids".to_owned(),
                        )
                    })
                })
                .collect::<Result<BTreeSet<u32>, ConfigError>>()?,
        };
        if peer_uids.is_empty() {
            return Err(ConfigError(
                "ELITEA_EXTRACTOR_PEER_UIDS must name at least one uid".to_owned(),
            ));
        }
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
            capacity: Capacity {
                concurrency,
                slot_wait: Duration::from_millis(slot_wait),
                stream_idle: Duration::from_millis(stream_idle),
            },
            peer_uids,
        })
    }

    /// Lowers the input cap so that `concurrency` documents at the cap,
    /// expanded `MEMORY_PER_INPUT_BYTE` (six) times, fit in half of
    /// `memory_limit` (the container's `memory.max`); the other half is the
    /// process itself and the parser's own overhead. The result is
    /// `min(configured, max(budget, floor))`: the configured cap is NEVER
    /// raised, not even to the 1 MiB floor. Returns the cap it lowered to,
    /// when it did.
    pub fn fit_memory(&mut self, memory_limit: u64) -> Option<u64> {
        let concurrency = u64::try_from(self.capacity.concurrency.max(1)).unwrap_or(1);
        let budget = memory_limit / 2 / concurrency / MEMORY_PER_INPUT_BYTE;
        let fitted = budget.max(INPUT_CAP_FLOOR).min(self.caps.max_input_bytes);
        if fitted < self.caps.max_input_bytes {
            self.caps.max_input_bytes = fitted;
            return Some(fitted);
        }
        None
    }

    /// The smallest container memory limit under which [`Config::fit_memory`]
    /// leaves this configuration's input cap as it is: the cap, times the
    /// concurrency, times the expansion, times two (the half kept for the
    /// process). The deployment defaults (Helm `worker.extractor.resources.
    /// limits.memory`, compose `mem_limit`) are set at or above this figure
    /// for the default caps, and the render test asserts it.
    #[must_use]
    pub fn memory_limit_for_caps(&self) -> u64 {
        let concurrency = u64::try_from(self.capacity.concurrency.max(1)).unwrap_or(1);
        self.caps
            .max_input_bytes
            .saturating_mul(concurrency)
            .saturating_mul(MEMORY_PER_INPUT_BYTE)
            .saturating_mul(2)
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
        assert_eq!(config.capacity, Capacity::default());
        assert_eq!(config.capacity.concurrency, 4);
        assert_eq!(config.capacity.slot_wait, Duration::from_mins(10));
        assert_eq!(config.capacity.stream_idle, Duration::from_secs(30));
        assert_eq!(config.peer_uids, BTreeSet::from([own_uid()]));
    }

    #[test]
    fn peer_uids_are_a_comma_separated_list_and_a_bad_one_is_named() {
        let config = config(&[("ELITEA_EXTRACTOR_PEER_UIDS", " 10001, 10002 ,")]).expect("config");
        assert_eq!(config.peer_uids, BTreeSet::from([10001, 10002]));
        let error = config_error(&[("ELITEA_EXTRACTOR_PEER_UIDS", "worker")]);
        assert!(error.0.contains("PEER_UIDS"), "{error}");
        let error = config_error(&[("ELITEA_EXTRACTOR_PEER_UIDS", ",")]);
        assert!(error.0.contains("at least one"), "{error}");
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
            ("ELITEA_EXTRACTOR_SLOT_WAIT_MILLIS", "4000"),
            ("ELITEA_EXTRACTOR_CONCURRENCY", "3"),
        ])
        .expect("config");
        assert_eq!(config.caps.max_input_bytes, 1024);
        assert_eq!(config.caps.max_pages, 7);
        assert_eq!(config.caps.max_output_bytes, 2048);
        assert_eq!(config.caps.timeout, Duration::from_millis(500));
        assert_eq!(config.capacity.stream_idle, Duration::from_millis(250));
        assert_eq!(config.capacity.slot_wait, Duration::from_secs(4));
        assert_eq!(config.capacity.concurrency, 3);
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
        // The floor itself does not fit again: the cap is already there.
        assert_eq!(config.fit_memory(1 << 20), None);
    }

    #[test]
    fn the_default_deployment_limit_keeps_the_default_cap() {
        // The defaults need 50 MiB × 4 × 6 × 2 = 2400 MiB. The chart and the
        // compose file set 2560 MiB (deploy/helm/elitea/values.yaml
        // worker.extractor.resources.limits.memory, docker-compose.
        // standalone-rust-agent.yml mem_limit; the render test asserts the
        // chart's figure against this one): the fit leaves the cap alone
        // there, and would NOT at a round 2 GiB.
        let config = config(&[]).expect("config");
        assert_eq!(config.memory_limit_for_caps(), 2400 << 20);
        let mut at_default = config.clone();
        assert_eq!(at_default.fit_memory(2560 << 20), None);
        assert_eq!(at_default.caps.max_input_bytes, 50 << 20);
        let mut exact = config.clone();
        assert_eq!(exact.fit_memory(2400 << 20), None);
        let mut two_gib = config;
        assert_eq!(two_gib.fit_memory(2 << 30), Some((2u64 << 30) / 2 / 4 / 6));
        assert!(two_gib.caps.max_input_bytes < 50 << 20);
    }

    #[test]
    fn the_memory_fit_never_raises_a_configured_cap() {
        // Configured under the floor: a tiny limit must leave it alone
        // (min(configured, max(budget, floor)) = configured), not lift it
        // to the floor.
        let mut small = config(&[
            ("ELITEA_EXTRACTOR_MAX_INPUT_BYTES", "4096"),
            ("ELITEA_EXTRACTOR_CONCURRENCY", "2"),
        ])
        .expect("config");
        assert_eq!(small.fit_memory(1 << 20), None);
        assert_eq!(small.caps.max_input_bytes, 4096);
        // Configured above the floor, budget under it: lowered TO the
        // floor, never above the configured cap.
        let mut mid = config(&[
            ("ELITEA_EXTRACTOR_MAX_INPUT_BYTES", "2097152"),
            ("ELITEA_EXTRACTOR_CONCURRENCY", "4"),
        ])
        .expect("config");
        assert_eq!(mid.fit_memory(1 << 20), Some(1 << 20));
        assert_eq!(mid.caps.max_input_bytes, 1 << 20);
        // A budget between the floor and the cap is the budget itself.
        let mut roomy =
            config(&[("ELITEA_EXTRACTOR_MAX_INPUT_BYTES", "52428800")]).expect("config");
        let budget = (512u64 << 20) / 2 / 4 / 6;
        assert_eq!(roomy.fit_memory(512 << 20), Some(budget));
        assert!(budget > INPUT_CAP_FLOOR && budget < 52_428_800);
    }
}
