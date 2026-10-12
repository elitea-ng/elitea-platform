//! Dead-peer detection for every tonic channel the Worker opens to Main.
//!
//! A Main Pod that is force-deleted sends no FIN or RST. Without probes the
//! Worker's connection stays in conntrack toward the dead Pod IP and every call
//! times out until the kernel drops it (measured: about 16 minutes). HTTP/2
//! PING frames find the dead peer in a bounded time, and tonic then reconnects
//! lazily to the Service address. Reconnect pacing is left as it was.

use std::time::Duration;

use tonic::transport::Endpoint;

/// Idle time before an HTTP/2 PING is sent. `keep_alive_while_idle` makes it
/// apply to a channel with no call in flight.
pub(crate) const HTTP2_KEEPALIVE_INTERVAL: Duration = Duration::from_secs(10);
/// How long an unanswered PING waits before the connection is closed.
pub(crate) const HTTP2_KEEPALIVE_TIMEOUT: Duration = Duration::from_secs(5);
/// TCP keepalive idle time, a second line for a peer that stops mid-frame.
pub(crate) const TCP_KEEPALIVE_IDLE: Duration = Duration::from_secs(15);
/// The shortest claim lease a dead peer must be found within. A dead Main is
/// detected after `interval + timeout`, with room for several lease renewals.
#[cfg(test)]
pub(crate) const MIN_CLAIM_LEASE_FOR_DETECTION: Duration = Duration::from_mins(1);

/// The probe schedule. Production code uses [`KeepAlive::PRODUCTION`] only;
/// tests pass a short schedule to a black-holed peer.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) struct KeepAlive {
    pub(crate) http2_interval: Duration,
    pub(crate) http2_timeout: Duration,
    pub(crate) tcp_idle: Duration,
}

impl KeepAlive {
    pub(crate) const PRODUCTION: Self = Self {
        http2_interval: HTTP2_KEEPALIVE_INTERVAL,
        http2_timeout: HTTP2_KEEPALIVE_TIMEOUT,
        tcp_idle: TCP_KEEPALIVE_IDLE,
    };

    /// Worst-case time to notice a peer that stops answering.
    #[cfg(test)]
    pub(crate) const fn detection_bound(self) -> Duration {
        self.http2_interval.saturating_add(self.http2_timeout)
    }
}

/// Apply the production schedule to an endpoint that reaches Main.
pub(crate) fn with_keepalive(endpoint: Endpoint) -> Endpoint {
    with_schedule(endpoint, KeepAlive::PRODUCTION)
}

pub(crate) fn with_schedule(endpoint: Endpoint, schedule: KeepAlive) -> Endpoint {
    endpoint
        .http2_keep_alive_interval(schedule.http2_interval)
        .keep_alive_timeout(schedule.http2_timeout)
        .keep_alive_while_idle(true)
        .tcp_keepalive(Some(schedule.tcp_idle))
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn production_schedule_finds_a_dead_peer_well_within_the_claim_lease() {
        let schedule = KeepAlive::PRODUCTION;
        assert_eq!(schedule.http2_interval, Duration::from_secs(10));
        assert_eq!(schedule.http2_timeout, Duration::from_secs(5));
        assert_eq!(schedule.tcp_idle, Duration::from_secs(15));
        // At most a quarter of the shortest lease: several renewals remain.
        assert!(schedule.detection_bound() * 4 <= MIN_CLAIM_LEASE_FOR_DETECTION);
        // The PING must time out before the next one is due.
        assert!(schedule.http2_timeout < schedule.http2_interval);
    }

    /// `Endpoint` hides its settings, so this guards the wiring instead: every
    /// production file that builds a dynamic endpoint must apply the schedule.
    #[test]
    fn every_production_endpoint_to_main_applies_the_keepalive_schedule() {
        fn walk(dir: &std::path::Path, found: &mut Vec<std::path::PathBuf>) {
            for entry in std::fs::read_dir(dir).unwrap() {
                let path = entry.unwrap().path();
                if path.is_dir() {
                    walk(&path, found);
                } else if path.extension().is_some_and(|extension| extension == "rs") {
                    found.push(path);
                }
            }
        }
        let mut files = Vec::new();
        walk(
            &std::path::Path::new(env!("CARGO_MANIFEST_DIR")).join("src"),
            &mut files,
        );
        let mut checked = 0;
        for path in files {
            let name = path.file_name().unwrap().to_string_lossy().into_owned();
            if name.ends_with("_tests.rs") || name == "keepalive.rs" {
                continue;
            }
            let text = std::fs::read_to_string(&path).unwrap();
            if text.contains("Endpoint::from_shared(") {
                checked += 1;
                assert!(
                    text.contains("with_keepalive"),
                    "{name}: an endpoint to Main must apply the keepalive schedule"
                );
            }
        }
        assert!(checked >= 4, "{checked} endpoint builders found");
    }
}
