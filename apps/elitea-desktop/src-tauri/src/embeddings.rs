//! Embeddings for the local index (ADR-0029 decision 7): OFF unless the
//! person turns them on for a workspace and chooses how.
//!
//! * **On this computer** (`local:<model id>`): a model of the pinned
//!   catalogue (`elitea_local_embed::catalog`), downloaded only once the
//!   person chooses it, into `<app data>/models/<weights sha256>/` (owner-only,
//!   shared by every workspace), verified before every load, run by candle
//!   (Metal on Apple silicon) on a capped thread pool.
//! * **Nothing else yet**: the platform's embeddings route is a separate,
//!   policy-gated choice.
//!
//! The background embedder (`elitea_local_index::embedding`) runs on mains
//! power only and not in Low Power Mode ([`Power`]), unless the person asks
//! for a rebuild; it waits while a turn runs, and drops the model after two
//! idle minutes.

use std::sync::{Arc, Mutex, PoisonError};
use std::time::{Duration, Instant};

use elitea_local_embed::candle_backend::{CandleEmbedder, DevicePreference};
use elitea_local_embed::catalog::{self, ModelSpec};
use elitea_local_embed::manager::ModelStore;
use elitea_local_embed::{EmbedError, Embedder};
use elitea_local_index::embedding::{Conditions, EmbedderSource, OFF};
use elitea_local_index::sqlite_store::VectorSet;

/// The prefix of an on-device choice.
pub const LOCAL: &str = "local:";

/// What a workspace's `embeddings` setting says.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Choice {
    Off,
    Local(&'static ModelSpec),
    /// A value this build does not know (a newer app wrote it): treated as
    /// off, its vectors dropped.
    Unknown,
}

impl Choice {
    #[must_use]
    pub fn parse(value: &str) -> Self {
        if value.is_empty() || value == OFF {
            return Self::Off;
        }
        value
            .strip_prefix(LOCAL)
            .and_then(catalog::model)
            .map_or(Self::Unknown, Self::Local)
    }
}

/// The models of this computer, as the background embedder loads them.
pub struct LocalModels {
    store: ModelStore,
    threads: usize,
}

impl LocalModels {
    /// The cache under `<app data>/models`; CPU work on half the cores.
    #[must_use]
    pub fn new(data_dir: &std::path::Path) -> Self {
        let cores = std::thread::available_parallelism().map_or(2, std::num::NonZero::get);
        Self {
            store: ModelStore::new(data_dir.join("models")),
            threads: (cores / 2).max(1),
        }
    }
}

impl EmbedderSource for LocalModels {
    fn vector_set(&self, choice: &str) -> Option<VectorSet> {
        match Choice::parse(choice) {
            Choice::Local(spec) => Some(VectorSet {
                origin: "local".into(),
                model: spec.id.into(),
                dim: spec.dimension,
            }),
            Choice::Off | Choice::Unknown => None,
        }
    }

    fn load(&self, choice: &str, _workspace_id: &str) -> Result<Arc<dyn Embedder>, EmbedError> {
        let Choice::Local(spec) = Choice::parse(choice) else {
            return Err(EmbedError::Load(format!("no embedder is called {choice}")));
        };
        let dir = self.store.verified_dir(spec)?;
        let started = Instant::now();
        let embedder =
            CandleEmbedder::load(spec, &dir, DevicePreference::Auto)?.with_threads(self.threads)?;
        log::info!(
            "embedding model {} loaded on {} in {:?}",
            spec.id,
            embedder.device_name(),
            started.elapsed()
        );
        Ok(Arc::new(embedder))
    }
}

/// How long a power reading is trusted.
const POWER_TTL: Duration = Duration::from_secs(60);

/// Mains power and not Low Power Mode, read from the system at most once a
/// minute. A computer that cannot tell (no battery, no `pmset`) runs.
#[derive(Default)]
pub struct Power {
    cached: Mutex<Option<(Instant, Option<&'static str>)>>,
}

impl Conditions for Power {
    fn hold(&self) -> Option<&'static str> {
        let mut cached = self.cached.lock().unwrap_or_else(PoisonError::into_inner);
        if let Some((at, hold)) = *cached
            && at.elapsed() < POWER_TTL
        {
            return hold;
        }
        let hold = read_power();
        *cached = Some((Instant::now(), hold));
        hold
    }
}

#[cfg(target_os = "macos")]
fn read_power() -> Option<&'static str> {
    let run = |args: &[&str]| {
        std::process::Command::new("/usr/bin/pmset")
            .args(args)
            .output()
            .ok()
            .filter(|out| out.status.success())
            .map(|out| String::from_utf8_lossy(&out.stdout).into_owned())
    };
    if run(&["-g", "batt"]).is_some_and(|text| on_battery(&text)) {
        return Some("on_battery");
    }
    if run(&["-g"]).is_some_and(|text| low_power(&text)) {
        return Some("low_power");
    }
    None
}

#[cfg(not(target_os = "macos"))]
fn read_power() -> Option<&'static str> {
    // Linux: a mains supply reporting offline while a battery exists.
    let Ok(entries) = std::fs::read_dir("/sys/class/power_supply") else {
        return None;
    };
    let mut battery = false;
    let mut mains_online = false;
    for entry in entries.flatten() {
        let read =
            |name: &str| std::fs::read_to_string(entry.path().join(name)).unwrap_or_default();
        match read("type").trim() {
            "Battery" => battery = true,
            "Mains" => mains_online |= read("online").trim() == "1",
            _ => {}
        }
    }
    (battery && !mains_online).then_some("on_battery")
}

/// `pmset -g batt`: "Now drawing from 'Battery Power'".
#[cfg_attr(not(target_os = "macos"), allow(dead_code))]
fn on_battery(text: &str) -> bool {
    text.lines()
        .next()
        .is_some_and(|line| line.contains("'Battery Power'"))
}

/// `pmset -g`: a ` lowpowermode 1` (or `powermode 1` on newer systems)
/// line.
#[cfg_attr(not(target_os = "macos"), allow(dead_code))]
fn low_power(text: &str) -> bool {
    text.lines().any(|line| {
        let mut words = line.split_whitespace();
        matches!(
            (words.next(), words.next()),
            (Some("lowpowermode" | "powermode"), Some("1"))
        )
    })
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn choices_parse_and_unknown_ones_are_off() {
        assert_eq!(Choice::parse("off"), Choice::Off);
        assert_eq!(Choice::parse(""), Choice::Off);
        assert!(matches!(
            Choice::parse("local:multilingual-e5-small"),
            Choice::Local(spec) if spec.dimension == 384
        ));
        assert_eq!(Choice::parse("local:nope"), Choice::Unknown);
        assert_eq!(Choice::parse("remote"), Choice::Unknown);
        let dir = tempfile::tempdir().unwrap();
        let models = LocalModels::new(dir.path());
        let set = models.vector_set("local:qwen3-embedding-0.6b").unwrap();
        assert_eq!(set.stamp(), "local:qwen3-embedding-0.6b:512");
        assert!(models.vector_set("off").is_none());
        // Not downloaded: refused before anything loads.
        let refused = models
            .load("local:multilingual-e5-small", "ws")
            .err()
            .unwrap();
        assert!(refused.to_string().contains("not downloaded"), "{refused}");
    }

    #[test]
    fn power_readings_parse() {
        assert!(on_battery(
            "Now drawing from 'Battery Power'\n -InternalBattery-0 (id=1)\t80%; discharging;"
        ));
        assert!(!on_battery(
            "Now drawing from 'AC Power'\n -InternalBattery-0 (id=1)\t100%; charged;"
        ));
        assert!(low_power(
            "System-wide power settings:\nCurrently in use:\n lowpowermode         1\n sleep 1\n"
        ));
        assert!(!low_power(
            "Currently in use:\n lowpowermode         0\n powermode 0\n"
        ));
    }
}
