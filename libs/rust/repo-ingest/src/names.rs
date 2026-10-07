//! What the operator calls each ingest setting.
//!
//! A refusal says which setting to raise (`over MAX_FILE_BYTES=…`), and the
//! name of that setting is the consuming engine's own environment variable,
//! not this crate's. So the engine hands its names in, once, through
//! [`crate::limits::IngestLimits::names`] and
//! [`crate::egress::EgressPolicy::named`]; without them a message names the
//! neutral setting ([`SettingNames::NEUTRAL`]).

/// The settings a message may name, and the clone's `User-Agent`.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct SettingNames {
    /// The most bytes a clone may fetch.
    pub max_clone_bytes: &'static str,
    /// The most files (and directories) a checkout may hold.
    pub max_file_count: &'static str,
    /// The largest single file.
    pub max_file_bytes: &'static str,
    /// The most bytes of files selected for analysis.
    pub max_parsed_bytes: &'static str,
    /// The clone (or folder download) deadline, in seconds.
    pub clone_timeout_seconds: &'static str,
    /// The most objects an artifact folder may hold.
    pub artifact_max_files: &'static str,
    /// The most bytes an artifact folder may hold.
    pub artifact_max_bytes: &'static str,
    /// The git hosts a clone may reach.
    pub git_allowlist: &'static str,
    /// The `User-Agent` of every git request: the consuming engine's name.
    pub user_agent: &'static str,
}

impl SettingNames {
    /// The names used when an engine supplies none.
    pub const NEUTRAL: Self = Self {
        max_clone_bytes: "MAX_CLONE_BYTES",
        max_file_count: "MAX_FILE_COUNT",
        max_file_bytes: "MAX_FILE_BYTES",
        max_parsed_bytes: "MAX_PARSED_BYTES",
        clone_timeout_seconds: "CLONE_TIMEOUT_SECONDS",
        artifact_max_files: "ARTIFACT_MAX_FILES",
        artifact_max_bytes: "ARTIFACT_MAX_BYTES",
        git_allowlist: "GIT_ALLOWLIST",
        user_agent: concat!("elitea-repo-ingest/", env!("CARGO_PKG_VERSION")),
    };
}

impl Default for SettingNames {
    fn default() -> Self {
        Self::NEUTRAL
    }
}
